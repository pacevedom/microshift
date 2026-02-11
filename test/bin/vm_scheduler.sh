#!/bin/bash
#
# Dynamic VM Scheduler for MicroShift Scenario Testing
#
# This scheduler manages VM resources for parallel scenario execution,
# implementing VM reuse for compatible scenarios and queuing when
# host resources are exhausted.
#
# Usage:
#   vm_scheduler.sh orchestrate <scenario_dir>  - Schedule all scenarios in directory
#   vm_scheduler.sh status                      - Show current scheduler state

set -euo pipefail

SCRIPTDIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck source=test/bin/common.sh
source "${SCRIPTDIR}/common.sh"

# Scheduler state directory
SCHEDULER_STATE_DIR="${IMAGEDIR}/scheduler-state"
VM_REGISTRY="${SCHEDULER_STATE_DIR}/vms"
SCENARIO_QUEUE="${SCHEDULER_STATE_DIR}/queue"
SCENARIO_STATUS="${SCHEDULER_STATE_DIR}/scenarios"
LOCK_DIR="${SCHEDULER_STATE_DIR}/locks"
SCHEDULER_LOG="${SCHEDULER_STATE_DIR}/scheduler.log"

# VM pool settings (from common.sh)
VM_POOL_BASENAME="vm-storage"
VM_DISK_BASEDIR="${IMAGEDIR}/${VM_POOL_BASENAME}"

# Scenario info directory (also from common.sh)
SCENARIO_INFO_DIR="${SCENARIO_INFO_DIR:-${IMAGEDIR}/scenario-info}"

# Host resource limits (configurable via environment, defaults from system)
# Detect system resources if not specified
_SYSTEM_VCPUS=$(nproc 2>/dev/null || echo 48)
_SYSTEM_MEMORY_KB=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}' || echo 100663296)
_SYSTEM_MEMORY_MB=$((_SYSTEM_MEMORY_KB / 1024))

HOST_TOTAL_VCPUS="${HOST_TOTAL_VCPUS:-${_SYSTEM_VCPUS}}"
HOST_TOTAL_MEMORY="${HOST_TOTAL_MEMORY:-${_SYSTEM_MEMORY_MB}}"

# System reserved resources (for host OS, hypervisor overhead, etc.)
SYSTEM_RESERVED_VCPUS="${SYSTEM_RESERVED_VCPUS:-2}"
SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-4096}"  # 4GB in MB

# Available resources after system reservation
HOST_AVAILABLE_VCPUS=$((HOST_TOTAL_VCPUS - SYSTEM_RESERVED_VCPUS))
HOST_AVAILABLE_MEMORY=$((HOST_TOTAL_MEMORY - SYSTEM_RESERVED_MEMORY))

# Timeouts for VM operations (in seconds)
# VM creation includes kickstart installation
VM_CREATE_TIMEOUT="${VM_CREATE_TIMEOUT:-600}"  # 10 minutes
# Default test timeout - scenarios can override via test_timeout in requirements
VM_TEST_TIMEOUT="${VM_TEST_TIMEOUT:-3600}"  # 60 minutes

# Calculated resource requirements (populated during planning phase)
declare -i STATIC_TOTAL_VCPUS=0
declare -i STATIC_TOTAL_MEMORY=0
declare -i DYNAMIC_AVAILABLE_VCPUS=0
declare -i DYNAMIC_AVAILABLE_MEMORY=0
declare -i MAX_DYNAMIC_VCPUS=0      # Largest dynamic scenario vCPU requirement
declare -i MAX_DYNAMIC_MEMORY=0     # Largest dynamic scenario memory requirement

# Current resource usage by dynamic VMs (tracked during execution)
declare -i current_vcpus=0
declare -i current_memory=0


# Logging function
log() {
    local timestamp
    timestamp="$(date '+%Y-%m-%d %H:%M:%S')"
    # Write to stderr (for console) and append to log file
    # Using stderr prevents log messages from being captured by command substitution
    echo "[${timestamp}] $*" >> "${SCHEDULER_LOG}"
    echo "[${timestamp}] $*" >&2
}

# Initialize scheduler state directory
init_scheduler() {
    # Create the base state directory first (before logging, since log writes to it)
    mkdir -p "${SCHEDULER_STATE_DIR}"

    log "Initializing scheduler state directory"
    mkdir -p "${VM_REGISTRY}" "${SCENARIO_QUEUE}" "${SCENARIO_STATUS}" "${LOCK_DIR}"

    # Clean up any stale state from previous runs
    rm -rf "${VM_REGISTRY:?}"/* "${SCENARIO_QUEUE:?}"/* "${SCENARIO_STATUS:?}"/* "${LOCK_DIR:?}"/*

    # Reset counters
    current_vcpus=0
    current_memory=0
    STATIC_TOTAL_VCPUS=0
    STATIC_TOTAL_MEMORY=0
    DYNAMIC_AVAILABLE_VCPUS=0
    DYNAMIC_AVAILABLE_MEMORY=0
    MAX_DYNAMIC_VCPUS=0
    MAX_DYNAMIC_MEMORY=0

    log "Scheduler state initialized"
    log "  Host total: vcpus=${HOST_TOTAL_VCPUS}, memory=${HOST_TOTAL_MEMORY}MB"
    log "  System reserved: vcpus=${SYSTEM_RESERVED_VCPUS}, memory=${SYSTEM_RESERVED_MEMORY}MB"
    log "  Available for VMs: vcpus=${HOST_AVAILABLE_VCPUS}, memory=${HOST_AVAILABLE_MEMORY}MB"
}

# Default VM resources when not specified in launch_vm
DEFAULT_VM_VCPUS=2
DEFAULT_VM_MEMORY=4096

# Parse static scenario to extract VM resource requirements from launch_vm calls
# Returns: vcpus memory (space-separated)
parse_static_scenario_resources() {
    local scenario_script="$1"

    # Extract launch_vm call and parse parameters
    local launch_vm_line
    launch_vm_line=$(grep -E '^\s*launch_vm' "${scenario_script}" 2>/dev/null | head -1) || true

    local vcpus="${DEFAULT_VM_VCPUS}"
    local memory="${DEFAULT_VM_MEMORY}"

    if [ -n "${launch_vm_line}" ]; then
        # Parse --vm_vcpus parameter
        if [[ "${launch_vm_line}" =~ --vm_vcpus[[:space:]]+([0-9]+) ]]; then
            vcpus="${BASH_REMATCH[1]}"
        fi
        # Parse --vm_memory parameter
        if [[ "${launch_vm_line}" =~ --vm_memory[[:space:]]+([0-9]+) ]]; then
            memory="${BASH_REMATCH[1]}"
        fi
    fi

    echo "${vcpus} ${memory}"
}

# Calculate total resources needed by all static scenarios
calculate_static_requirements() {
    local -a static_scenarios=("$@")

    STATIC_TOTAL_VCPUS=0
    STATIC_TOTAL_MEMORY=0

    for scenario_script in "${static_scenarios[@]}"; do
        local resources
        resources=$(parse_static_scenario_resources "${scenario_script}")
        local vcpus memory
        read -r vcpus memory <<< "${resources}"

        STATIC_TOTAL_VCPUS=$((STATIC_TOTAL_VCPUS + vcpus))
        STATIC_TOTAL_MEMORY=$((STATIC_TOTAL_MEMORY + memory))

        local scenario_name
        scenario_name=$(basename "${scenario_script}" .sh)
        log "  Static ${scenario_name}: vcpus=${vcpus}, memory=${memory}MB"
    done

    log "Static total: vcpus=${STATIC_TOTAL_VCPUS}, memory=${STATIC_TOTAL_MEMORY}MB"
}

# Calculate max resources needed by any single dynamic scenario
calculate_max_dynamic_requirements() {
    local -a dynamic_scenarios=("$@")

    MAX_DYNAMIC_VCPUS=0
    MAX_DYNAMIC_MEMORY=0

    for scenario_script in "${dynamic_scenarios[@]}"; do
        local scenario_name
        scenario_name=$(basename "${scenario_script}" .sh)

        # Get requirements from dynamic_schedule_requirements function
        local req_file="${SCENARIO_STATUS}/${scenario_name}/requirements"
        mkdir -p "$(dirname "${req_file}")"

        if ! get_scenario_requirements "${scenario_script}" "${req_file}"; then
            log "WARNING: Failed to get requirements for ${scenario_name}"
            continue
        fi

        local vcpus memory
        vcpus=$(get_req_value "${req_file}" "min_vcpus" "${DEFAULT_VM_VCPUS}")
        memory=$(get_req_value "${req_file}" "min_memory" "${DEFAULT_VM_MEMORY}")

        log "  Dynamic ${scenario_name}: vcpus=${vcpus}, memory=${memory}MB"

        if [ "${vcpus}" -gt "${MAX_DYNAMIC_VCPUS}" ]; then
            MAX_DYNAMIC_VCPUS="${vcpus}"
        fi
        if [ "${memory}" -gt "${MAX_DYNAMIC_MEMORY}" ]; then
            MAX_DYNAMIC_MEMORY="${memory}"
        fi
    done

    log "Max dynamic scenario: vcpus=${MAX_DYNAMIC_VCPUS}, memory=${MAX_DYNAMIC_MEMORY}MB"
}

# Acquire a lock for thread-safe operations
acquire_lock() {
    local lock_name="$1"
    local lock_file="${LOCK_DIR}/${lock_name}.lock"

    while ! mkdir "${lock_file}" 2>/dev/null; do
        sleep 0.1
    done
}

# Release a lock
release_lock() {
    local lock_name="$1"
    local lock_file="${LOCK_DIR}/${lock_name}.lock"
    rmdir "${lock_file}" 2>/dev/null || true
}

# Create VMs for all static scenarios (boot phase only)
create_static_vms() {
    local -a static_scenarios=("$@")

    if [ ${#static_scenarios[@]} -eq 0 ]; then
        log "No static VMs to create"
        return 0
    fi

    log "Creating ${#static_scenarios[@]} static VMs in parallel"

    local static_create_log="${SCHEDULER_STATE_DIR}/static_create_jobs.txt"

    # Show progress if interactive
    local progress=""
    if [ -t 0 ]; then
        progress="--progress"
    fi

    # Run scenario.sh create (boot phase only) for all static scenarios
    if ! parallel \
        ${progress} \
        --results "${SCENARIO_INFO_DIR}/{/.}/boot.log" \
        --joblog "${static_create_log}" \
        --delay 5 \
        bash -x "${SCRIPTDIR}/scenario.sh" create ::: "${static_scenarios[@]}"; then
        log "ERROR: Some static VMs failed to create"
        cat "${static_create_log}"
        return 1
    fi

    cat "${static_create_log}"
    log "All static VMs created successfully"
    return 0
}

# Run tests on static VMs (after creation)
run_static_tests() {
    local -a static_scenarios=("$@")

    if [ ${#static_scenarios[@]} -eq 0 ]; then
        log "No static tests to run"
        return 0
    fi

    log "Running ${#static_scenarios[@]} static scenario tests in parallel"

    local static_run_log="${SCHEDULER_STATE_DIR}/static_run_jobs.txt"

    # Show progress if interactive
    local progress=""
    if [ -t 0 ]; then
        progress="--progress"
    fi

    # Run scenario.sh run for all static scenarios
    local result=0
    if ! parallel \
        ${progress} \
        --results "${SCENARIO_INFO_DIR}/{/.}/run.log" \
        --joblog "${static_run_log}" \
        --delay 2 \
        bash -x "${SCRIPTDIR}/scenario.sh" run ::: "${static_scenarios[@]}"; then
        result=1
    fi

    cat "${static_run_log}"
    return ${result}
}

# Generate a unique VM name for dynamic scenarios
# Uses a neutral naming scheme: dynamic-vm-NNN
# Uses mkdir for atomic allocation - only one process can create a directory
generate_dynamic_vm_name() {
    local count=1
    local vm_name
    while true; do
        vm_name="dynamic-vm-$(printf '%03d' "${count}")"
        # mkdir is atomic on POSIX - only one process succeeds
        if mkdir "${VM_REGISTRY}/${vm_name}" 2>/dev/null; then
            echo "${vm_name}"
            return 0
        fi
        ((count++))
        # Safety limit
        if [ ${count} -gt 999 ]; then
            echo "ERROR: Too many VMs" >&2
            return 1
        fi
    done
}

# Check if a scenario has the dynamic_schedule_requirements function (opt-in check)
scenario_is_dynamic() {
    local scenario_script="$1"

    # Source the scenario in a subshell and check for the function
    if bash -c "source '${scenario_script}' 2>/dev/null && type dynamic_schedule_requirements &>/dev/null"; then
        return 0
    fi
    return 1
}

# Get VM requirements from a scenario script
get_scenario_requirements() {
    local scenario_script="$1"
    local output_file="$2"

    # Source the scenario and call dynamic_schedule_requirements
    # shellcheck disable=SC1090
    (
        source "${scenario_script}"
        if type dynamic_schedule_requirements &>/dev/null; then
            dynamic_schedule_requirements > "${output_file}"
        else
            # This shouldn't happen if scenario_is_dynamic was checked first
            echo "ERROR: dynamic_schedule_requirements not found" >&2
            return 1
        fi
    )
}

# Parse a value from requirements file
get_req_value() {
    local req_file="$1"
    local key="$2"
    local default="${3:-}"

    local value
    value=$(grep "^${key}=" "${req_file}" 2>/dev/null | cut -d= -f2 || true)
    if [ -z "${value}" ]; then
        echo "${default}"
    else
        echo "${value}"
    fi
}

# Check if boot image is compatible (superset matching)
boot_image_compatible() {
    local vm_image="$1"
    local req_image="$2"

    # Exact match always works
    [ "${vm_image}" = "${req_image}" ] && return 0

    # Special images require exact match (cannot substitute)
    case "${req_image}" in
        *-fips|*-tuned|*-isolated|*-ai-model-serving)
            return 1  # Must be exact
            ;;
    esac

    # Check if VM image is a superset of required image
    # Hierarchy: source < source-optionals, brew-lrel < brew-lrel-optional
    case "${req_image}" in
        *-source)
            # source-optionals is superset of source
            [[ "${vm_image}" == "${req_image}-optionals" ]] && return 0
            ;;
        *-brew-lrel)
            # brew-lrel-optional is superset of brew-lrel
            [[ "${vm_image}" == "${req_image}-optional" ]] && return 0
            ;;
    esac

    # ai-model-serving includes qemu-guest-agent, so can run isolated scenarios
    if [[ "${req_image}" == *-isolated ]] && [[ "${vm_image}" == *-ai-model-serving ]]; then
        return 0
    fi

    return 1  # Not compatible
}

# Check if VM satisfies scenario requirements
vm_satisfies_requirements() {
    local vm_name="$1"
    local scenario_reqs="$2"

    local vm_state="${VM_REGISTRY}/${vm_name}/state"
    [ -f "${vm_state}" ] || return 1

    # Get VM capabilities
    local vm_vcpus vm_memory vm_disksize vm_networks vm_fips vm_boot_image
    vm_vcpus=$(get_req_value "${vm_state}" "vcpus" "2")
    vm_memory=$(get_req_value "${vm_state}" "memory" "4096")
    vm_disksize=$(get_req_value "${vm_state}" "disksize" "20")
    vm_networks=$(get_req_value "${vm_state}" "networks" "default")
    vm_fips=$(get_req_value "${vm_state}" "fips" "false")
    vm_boot_image=$(get_req_value "${vm_state}" "boot_image" "")

    # Get scenario requirements
    local req_vcpus req_memory req_disksize req_networks req_fips req_boot_image
    req_vcpus=$(get_req_value "${scenario_reqs}" "min_vcpus" "2")
    req_memory=$(get_req_value "${scenario_reqs}" "min_memory" "4096")
    req_disksize=$(get_req_value "${scenario_reqs}" "min_disksize" "20")
    req_networks=$(get_req_value "${scenario_reqs}" "networks" "default")
    req_fips=$(get_req_value "${scenario_reqs}" "fips" "false")
    req_boot_image=$(get_req_value "${scenario_reqs}" "boot_image" "")

    # Check minimums
    [ "${vm_vcpus}" -ge "${req_vcpus}" ] || return 1
    [ "${vm_memory}" -ge "${req_memory}" ] || return 1
    [ "${vm_disksize}" -ge "${req_disksize}" ] || return 1

    # Check networks (VM must have all required networks)
    for net in ${req_networks//,/ }; do
        echo ",${vm_networks}," | grep -q ",${net}," || return 1
    done

    # Check FIPS (if required, VM must have it)
    if [ "${req_fips}" = "true" ] && [ "${vm_fips}" != "true" ]; then
        return 1
    fi

    # Check boot image compatibility
    if ! boot_image_compatible "${vm_boot_image}" "${req_boot_image}"; then
        return 1
    fi

    return 0  # VM is compatible
}

# Find a free VM that satisfies scenario requirements
find_compatible_vm() {
    local scenario_reqs="$1"

    for vm_dir in "${VM_REGISTRY}"/*; do
        [ -d "${vm_dir}" ] || continue
        local vm_name
        vm_name=$(basename "${vm_dir}")
        local vm_state="${vm_dir}/state"
        [ -f "${vm_state}" ] || continue

        local vm_status
        vm_status=$(get_req_value "${vm_state}" "status" "unknown")

        if [ "${vm_status}" = "available" ]; then
            if vm_satisfies_requirements "${vm_name}" "${scenario_reqs}"; then
                echo "${vm_name}"
                return 0
            fi
        fi
    done
    return 1
}

# Check if resources are available for new VM
can_allocate() {
    local vcpus="$1"
    local memory="$2"

    # Recalculate current usage from active dynamic VMs
    recalculate_resource_usage

    # Check against dynamic available resources (after system + static reservation)
    [ $((current_vcpus + vcpus)) -le ${DYNAMIC_AVAILABLE_VCPUS} ] && \
    [ $((current_memory + memory)) -le ${DYNAMIC_AVAILABLE_MEMORY} ]
}

# Recalculate current resource usage from active VMs
recalculate_resource_usage() {
    current_vcpus=0
    current_memory=0

    for vm_dir in "${VM_REGISTRY}"/*; do
        [ -d "${vm_dir}" ] || continue
        local vm_state="${vm_dir}/state"
        [ -f "${vm_state}" ] || continue

        local vm_status
        vm_status=$(get_req_value "${vm_state}" "status" "unknown")

        if [ "${vm_status}" = "in_use" ] || [ "${vm_status}" = "available" ]; then
            local vm_vcpus vm_memory
            vm_vcpus=$(get_req_value "${vm_state}" "vcpus" "0")
            vm_memory=$(get_req_value "${vm_state}" "memory" "0")
            current_vcpus=$((current_vcpus + vm_vcpus))
            current_memory=$((current_memory + vm_memory))
        fi
    done
}

# Register a new VM in the registry
register_vm() {
    local vm_name="$1"
    local scenario_reqs="$2"
    local scenario_name="$3"

    local vm_dir="${VM_REGISTRY}/${vm_name}"
    mkdir -p "${vm_dir}"

    # Copy requirements as VM state with status
    cp "${scenario_reqs}" "${vm_dir}/state"

    # Add vcpus/memory/disksize from min_* values and set status
    local vcpus memory disksize networks fips boot_image
    vcpus=$(get_req_value "${scenario_reqs}" "min_vcpus" "2")
    memory=$(get_req_value "${scenario_reqs}" "min_memory" "4096")
    disksize=$(get_req_value "${scenario_reqs}" "min_disksize" "20")
    networks=$(get_req_value "${scenario_reqs}" "networks" "default")
    fips=$(get_req_value "${scenario_reqs}" "fips" "false")
    boot_image=$(get_req_value "${scenario_reqs}" "boot_image" "")

    cat > "${vm_dir}/state" <<EOF
vcpus=${vcpus}
memory=${memory}
disksize=${disksize}
networks=${networks}
fips=${fips}
boot_image=${boot_image}
status=in_use
current_scenario=${scenario_name}
EOF

    # Initialize scenario history
    echo "$(date -Iseconds) CREATED for ${scenario_name}" >> "${vm_dir}/scenario_history.log"

    log "Registered VM ${vm_name} for scenario ${scenario_name}"
}

# Assign an existing VM to a scenario
assign_vm_to_scenario() {
    local vm_name="$1"
    local scenario_name="$2"

    local vm_dir="${VM_REGISTRY}/${vm_name}"
    local vm_state="${vm_dir}/state"

    # Update status and current scenario
    sed -i "s/^status=.*/status=in_use/" "${vm_state}"
    sed -i "s/^current_scenario=.*/current_scenario=${scenario_name}/" "${vm_state}"

    # Record in scenario history
    echo "$(date -Iseconds) START ${scenario_name}" >> "${vm_dir}/scenario_history.log"

    # Mark the scenario as running in the queue (prevents duplicate dispatch)
    local queue_file="${SCENARIO_QUEUE}/${scenario_name}"
    if [ -f "${queue_file}" ]; then
        sed -i "s/^status=.*/status=running/" "${queue_file}"
        echo "started_at=$(date -Iseconds)" >> "${queue_file}"
    fi

    # Create assignment file for scenario
    mkdir -p "${SCENARIO_STATUS}/${scenario_name}"
    echo "${vm_name}" > "${SCENARIO_STATUS}/${scenario_name}/vm_assignment"
    echo "true" > "${SCENARIO_STATUS}/${scenario_name}/vm_reused"

    log "Assigned VM ${vm_name} to scenario ${scenario_name} (reuse)"
}

# Mark VM as available after scenario completes
release_vm() {
    local vm_name="$1"
    local scenario_name="$2"
    local result="$3"

    local vm_dir="${VM_REGISTRY}/${vm_name}"
    local vm_state="${vm_dir}/state"

    # Record in scenario history
    echo "$(date -Iseconds) END ${scenario_name} ${result}" >> "${vm_dir}/scenario_history.log"

    # Update status to available
    sed -i "s/^status=.*/status=available/" "${vm_state}"
    sed -i "s/^current_scenario=.*/current_scenario=/" "${vm_state}"

    log "Released VM ${vm_name} from scenario ${scenario_name} (${result})"
}

# Destroy a VM and reclaim resources
destroy_vm() {
    local vm_name="$1"
    local reason="${2:-no compatible scenarios}"

    local vm_dir="${VM_REGISTRY}/${vm_name}"

    # Record destruction in history
    echo "$(date -Iseconds) DESTROYED (${reason})" >> "${vm_dir}/scenario_history.log"

    # Mark as destroyed
    sed -i "s/^status=.*/status=destroyed/" "${vm_dir}/state" 2>/dev/null || \
        echo "status=destroyed" >> "${vm_dir}/state"

    log "Destroying libvirt VM ${vm_name}: ${reason}"

    # Actually destroy the libvirt VM and clean up resources
    if sudo virsh dumpxml "${vm_name}" &>/dev/null; then
        if ! sudo virsh dominfo "${vm_name}" 2>/dev/null | grep '^State' | grep -q 'shut off'; then
            sudo virsh destroy --graceful "${vm_name}" 2>/dev/null || true
        fi
        if ! sudo virsh dominfo "${vm_name}" 2>/dev/null | grep '^State' | grep -q 'shut off'; then
            sudo virsh destroy "${vm_name}" 2>/dev/null || true
        fi
        sudo virsh undefine --nvram "${vm_name}" 2>/dev/null || true
    fi

    # Clean up storage pool
    local vm_pool_name="${VM_POOL_BASENAME}-${vm_name}"
    if sudo virsh pool-info "${vm_pool_name}" &>/dev/null; then
        sudo virsh pool-destroy "${vm_pool_name}" 2>/dev/null || true
        sudo virsh pool-undefine "${vm_pool_name}" 2>/dev/null || true
    fi

    # Remove pool directory
    rm -rf "${VM_DISK_BASEDIR:?}/${vm_pool_name}" 2>/dev/null || true

    log "Destroyed VM ${vm_name}"
}

# Queue a scenario for execution
queue_scenario() {
    local scenario_script="$1"
    local scenario_name="$2"
    local req_file="$3"

    local queue_file="${SCENARIO_QUEUE}/${scenario_name}"
    cat > "${queue_file}" <<EOF
script=${scenario_script}
requirements=${req_file}
status=queued
queued_at=$(date -Iseconds)
EOF

    log "Queued scenario ${scenario_name}"
}

# Get list of queued scenarios
get_queued_scenarios() {
    for queue_file in "${SCENARIO_QUEUE}"/*; do
        [ -f "${queue_file}" ] || continue
        local status
        status=$(get_req_value "${queue_file}" "status" "")
        if [ "${status}" = "queued" ]; then
            basename "${queue_file}"
        fi
    done
}

# Mark scenario as running
mark_scenario_running() {
    local scenario_name="$1"
    local queue_file="${SCENARIO_QUEUE}/${scenario_name}"

    sed -i "s/^status=.*/status=running/" "${queue_file}"
    echo "started_at=$(date -Iseconds)" >> "${queue_file}"
}

# Mark scenario as completed
mark_scenario_completed() {
    local scenario_name="$1"
    local result="$2"
    local queue_file="${SCENARIO_QUEUE}/${scenario_name}"

    sed -i "s/^status=.*/status=completed/" "${queue_file}"
    echo "completed_at=$(date -Iseconds)" >> "${queue_file}"
    echo "result=${result}" >> "${queue_file}"
}

# Check if there are pending scenarios
has_pending_scenarios() {
    for queue_file in "${SCENARIO_QUEUE}"/*; do
        [ -f "${queue_file}" ] || continue
        local status
        status=$(get_req_value "${queue_file}" "status" "")
        if [ "${status}" = "queued" ]; then
            return 0
        fi
    done
    return 1
}

# Run a scenario on a VM
run_scenario_on_vm() {
    local scenario_script="$1"
    local scenario_name="$2"
    local vm_name="$3"
    local is_new_vm="$4"

    log "Starting scenario ${scenario_name} on VM ${vm_name} (new_vm=${is_new_vm})"

    mark_scenario_running "${scenario_name}"

    # Set up environment for scheduler-aware scenario execution
    export SCHEDULER_ENABLED=true
    export SCHEDULER_SCENARIO_NAME="${scenario_name}"
    export SCHEDULER_STATE_DIR="${SCHEDULER_STATE_DIR}"

    # Always pass the VM name - scheduler controls naming for dynamic scenarios
    export SCHEDULER_VM_NAME="${vm_name}"
    if [ "${is_new_vm}" = "false" ]; then
        export SCHEDULER_IS_NEW_VM="false"
    else
        export SCHEDULER_IS_NEW_VM="true"
    fi

    local result="SUCCESS"
    local exit_code=0

    # Set up log directory for scenario execution
    local scenario_log_dir="${SCENARIO_INFO_DIR}/${scenario_name}"
    mkdir -p "${scenario_log_dir}"

    # Get scenario-specific timeouts if specified, otherwise use defaults
    local req_file="${SCENARIO_STATUS}/${scenario_name}/requirements"
    local create_timeout="${VM_CREATE_TIMEOUT}"
    local test_timeout="${VM_TEST_TIMEOUT}"
    if [ -f "${req_file}" ]; then
        local custom_create_timeout
        custom_create_timeout=$(get_req_value "${req_file}" "create_timeout" "")
        if [ -n "${custom_create_timeout}" ]; then
            create_timeout="${custom_create_timeout}"
        fi

        local custom_test_timeout
        custom_test_timeout=$(get_req_value "${req_file}" "test_timeout" "")
        if [ -n "${custom_test_timeout}" ]; then
            test_timeout="${custom_test_timeout}"
        fi
    fi

    # Phase 1: Create VM (only for new VMs)
    if [ "${is_new_vm}" = "true" ]; then
        local create_log="${scenario_log_dir}/create.log"
        log "Creating VM ${vm_name} (timeout: ${create_timeout}s) - logging to ${create_log}"

        # Link creation log to VM directory
        local vm_dir="${VM_REGISTRY}/${vm_name}"
        mkdir -p "${vm_dir}"
        ln -sf "${create_log}" "${vm_dir}/creation_log"

        local create_exit=0
        timeout --signal=TERM --kill-after=60 "${create_timeout}" \
            bash -x "${SCRIPTDIR}/scenario.sh" create "${scenario_script}" &> "${create_log}" || create_exit=$?

        if [ ${create_exit} -ne 0 ]; then
            result="FAILED"
            exit_code=1
            if [ ${create_exit} -eq 124 ]; then
                log "VM creation TIMED OUT for ${scenario_name} after ${create_timeout}s - see ${create_log}"
            else
                log "VM creation failed for ${scenario_name} (exit ${create_exit}) - see ${create_log}"
            fi
        fi
    fi

    # Phase 2: Run tests (only if creation succeeded or VM was reused)
    if [ "${exit_code}" -eq 0 ]; then
        local run_log="${scenario_log_dir}/run.log"
        log "Running tests for ${scenario_name} (timeout: ${test_timeout}s) - logging to ${run_log}"

        local run_exit=0
        timeout --signal=TERM --kill-after=60 "${test_timeout}" \
            bash -x "${SCRIPTDIR}/scenario.sh" run "${scenario_script}" &> "${run_log}" || run_exit=$?

        if [ ${run_exit} -ne 0 ]; then
            result="FAILED"
            exit_code=1
            if [ ${run_exit} -eq 124 ]; then
                log "Tests TIMED OUT for ${scenario_name} after ${test_timeout}s - see ${run_log}"
            else
                log "Tests failed for ${scenario_name} (exit ${run_exit}) - see ${run_log}"
            fi
        fi
    fi

    mark_scenario_completed "${scenario_name}" "${result}"

    # Handle VM after scenario completion
    acquire_lock "vm_dispatch"

    release_vm "${vm_name}" "${scenario_name}" "${result}"

    # Check if any queued scenario can use this VM
    local next_scenario=""
    for queued in $(get_queued_scenarios); do
        local queue_file="${SCENARIO_QUEUE}/${queued}"
        local req_file
        req_file=$(get_req_value "${queue_file}" "requirements" "")

        if [ -n "${req_file}" ] && [ -f "${req_file}" ]; then
            if vm_satisfies_requirements "${vm_name}" "${req_file}"; then
                next_scenario="${queued}"
                break
            fi
        fi
    done

    if [ -n "${next_scenario}" ]; then
        # Reuse VM for next compatible scenario
        local queue_file="${SCENARIO_QUEUE}/${next_scenario}"
        local next_script next_reqs
        next_script=$(get_req_value "${queue_file}" "script" "")
        next_reqs=$(get_req_value "${queue_file}" "requirements" "")

        assign_vm_to_scenario "${vm_name}" "${next_scenario}"

        release_lock "vm_dispatch"

        # Run the next scenario (recursive call)
        run_scenario_on_vm "${next_script}" "${next_scenario}" "${vm_name}" "false"
    else
        # No compatible scenario waiting - destroy VM
        destroy_vm "${vm_name}"
        release_lock "vm_dispatch"
    fi

    return ${exit_code}
}

# Create a new VM for a scenario
create_vm_for_scenario() {
    local scenario_script="$1"
    local scenario_name="$2"
    local req_file="$3"

    # Generate a neutral VM name (not tied to any scenario)
    local vm_name
    vm_name=$(generate_dynamic_vm_name)

    # Register the VM
    register_vm "${vm_name}" "${req_file}" "${scenario_name}"

    # Create assignment file for scenario
    mkdir -p "${SCENARIO_STATUS}/${scenario_name}"
    echo "${vm_name}" > "${SCENARIO_STATUS}/${scenario_name}/vm_assignment"
    echo "false" > "${SCENARIO_STATUS}/${scenario_name}/vm_reused"

    echo "${vm_name}"
}

# Main dispatch loop for dynamic scenarios
dispatch_dynamic_scenarios() {
    local -a pids=()
    local -a pid_scenarios=()
    local overall_result=0

    while has_pending_scenarios || [ ${#pids[@]} -gt 0 ]; do
        # Try to dispatch queued scenarios
        acquire_lock "vm_dispatch"

        for scenario_name in $(get_queued_scenarios); do
            local queue_file="${SCENARIO_QUEUE}/${scenario_name}"
            local scenario_script req_file
            scenario_script=$(get_req_value "${queue_file}" "script" "")
            req_file=$(get_req_value "${queue_file}" "requirements" "")

            if [ -z "${scenario_script}" ] || [ -z "${req_file}" ]; then
                continue
            fi

            local min_vcpus min_memory
            min_vcpus=$(get_req_value "${req_file}" "min_vcpus" "2")
            min_memory=$(get_req_value "${req_file}" "min_memory" "4096")

            # Try to find compatible free VM
            local vm_name=""
            if vm_name=$(find_compatible_vm "${req_file}"); then
                # Reuse existing VM
                assign_vm_to_scenario "${vm_name}" "${scenario_name}"
                log "Reusing VM ${vm_name} for ${scenario_name}"

                # Start scenario in background
                run_scenario_on_vm "${scenario_script}" "${scenario_name}" "${vm_name}" "false" &
                local pid=$!
                pids+=("${pid}")
                pid_scenarios+=("${scenario_name}")

            elif can_allocate "${min_vcpus}" "${min_memory}"; then
                # Create new VM
                vm_name=$(create_vm_for_scenario "${scenario_script}" "${scenario_name}" "${req_file}")
                log "Creating new VM ${vm_name} for ${scenario_name}"

                # Start scenario in background
                run_scenario_on_vm "${scenario_script}" "${scenario_name}" "${vm_name}" "true" &
                local pid=$!
                pids+=("${pid}")
                pid_scenarios+=("${scenario_name}")

            else
                log "Resources exhausted, keeping ${scenario_name} queued (need: vcpus=${min_vcpus}, mem=${min_memory})"
            fi
        done

        release_lock "vm_dispatch"

        # If we have running jobs, wait for any one to finish
        if [ ${#pids[@]} -gt 0 ]; then
            # Wait for any background job to complete (bash 4.3+)
            # Returns immediately when any child exits
            wait -n -p finished_pid 2>/dev/null || true

            # Find which scenario finished and collect its exit status
            local -a new_pids=()
            local -a new_pid_scenarios=()
            for i in "${!pids[@]}"; do
                local pid="${pids[$i]}"
                local scenario="${pid_scenarios[$i]}"
                if ! kill -0 "${pid}" 2>/dev/null; then
                    # Process finished - collect exit status
                    wait "${pid}" || overall_result=1
                    log "Scenario ${scenario} finished (pid ${pid})"
                else
                    new_pids+=("${pid}")
                    new_pid_scenarios+=("${scenario}")
                fi
            done
            pids=("${new_pids[@]+"${new_pids[@]}"}")
            pid_scenarios=("${new_pid_scenarios[@]+"${new_pid_scenarios[@]}"}")
        fi
    done

    return ${overall_result}
}

# Main orchestration function
orchestrate() {
    local scenario_dir="$1"

    if [ ! -d "${scenario_dir}" ]; then
        echo "ERROR: Scenario directory not found: ${scenario_dir}" >&2
        exit 1
    fi

    init_scheduler

    log "Starting orchestration for scenarios in ${scenario_dir}"

    # PHASE 1: Classify scenarios
    log "=== PHASE 1: Classifying scenarios ==="
    local -a dynamic_scenarios=()
    local -a static_scenarios=()

    for scenario_script in "${scenario_dir}"/*.sh; do
        [ -f "${scenario_script}" ] || continue

        if scenario_is_dynamic "${scenario_script}"; then
            dynamic_scenarios+=("${scenario_script}")
            log "  Dynamic: $(basename "${scenario_script}")"
        else
            static_scenarios+=("${scenario_script}")
            log "  Static: $(basename "${scenario_script}")"
        fi
    done

    log "Found ${#dynamic_scenarios[@]} dynamic scenarios and ${#static_scenarios[@]} static scenarios"

    # PHASE 2: Calculate and validate resource requirements BEFORE creating any VMs
    log ""
    log "=== PHASE 2: Resource Planning and Validation ==="

    # Calculate static requirements
    if [ ${#static_scenarios[@]} -gt 0 ]; then
        log "Calculating static scenario requirements..."
        calculate_static_requirements "${static_scenarios[@]}"
    else
        log "No static scenarios"
        STATIC_TOTAL_VCPUS=0
        STATIC_TOTAL_MEMORY=0
    fi

    # Calculate dynamic requirements and find max
    if [ ${#dynamic_scenarios[@]} -gt 0 ]; then
        log "Calculating dynamic scenario requirements..."
        calculate_max_dynamic_requirements "${dynamic_scenarios[@]}"
    else
        log "No dynamic scenarios"
        MAX_DYNAMIC_VCPUS=0
        MAX_DYNAMIC_MEMORY=0
    fi

    # Calculate available resources for dynamic scheduling
    DYNAMIC_AVAILABLE_VCPUS=$((HOST_AVAILABLE_VCPUS - STATIC_TOTAL_VCPUS))
    DYNAMIC_AVAILABLE_MEMORY=$((HOST_AVAILABLE_MEMORY - STATIC_TOTAL_MEMORY))

    log ""
    log "=== Resource Summary ==="
    log "  Host total:        vcpus=${HOST_TOTAL_VCPUS}, memory=${HOST_TOTAL_MEMORY}MB"
    log "  System reserved:   vcpus=${SYSTEM_RESERVED_VCPUS}, memory=${SYSTEM_RESERVED_MEMORY}MB"
    log "  Available for VMs: vcpus=${HOST_AVAILABLE_VCPUS}, memory=${HOST_AVAILABLE_MEMORY}MB"
    log "  Static requires:   vcpus=${STATIC_TOTAL_VCPUS}, memory=${STATIC_TOTAL_MEMORY}MB"
    log "  Dynamic available: vcpus=${DYNAMIC_AVAILABLE_VCPUS}, memory=${DYNAMIC_AVAILABLE_MEMORY}MB"
    log "  Max dynamic needs: vcpus=${MAX_DYNAMIC_VCPUS}, memory=${MAX_DYNAMIC_MEMORY}MB"
    log ""

    # Validate static scenarios fit
    if [ ${STATIC_TOTAL_VCPUS} -gt ${HOST_AVAILABLE_VCPUS} ]; then
        log "ERROR: Static scenarios require more vCPUs than available"
        log "  Required: ${STATIC_TOTAL_VCPUS}, Available: ${HOST_AVAILABLE_VCPUS}"
        return 1
    fi
    if [ ${STATIC_TOTAL_MEMORY} -gt ${HOST_AVAILABLE_MEMORY} ]; then
        log "ERROR: Static scenarios require more memory than available"
        log "  Required: ${STATIC_TOTAL_MEMORY}MB, Available: ${HOST_AVAILABLE_MEMORY}MB"
        return 1
    fi

    # Validate at least one dynamic scenario can run (if there are any)
    if [ ${#dynamic_scenarios[@]} -gt 0 ]; then
        if [ ${MAX_DYNAMIC_VCPUS} -gt ${DYNAMIC_AVAILABLE_VCPUS} ]; then
            log "ERROR: Largest dynamic scenario requires more vCPUs than available after static allocation"
            log "  Required: ${MAX_DYNAMIC_VCPUS}, Available: ${DYNAMIC_AVAILABLE_VCPUS}"
            return 1
        fi
        if [ ${MAX_DYNAMIC_MEMORY} -gt ${DYNAMIC_AVAILABLE_MEMORY} ]; then
            log "ERROR: Largest dynamic scenario requires more memory than available after static allocation"
            log "  Required: ${MAX_DYNAMIC_MEMORY}MB, Available: ${DYNAMIC_AVAILABLE_MEMORY}MB"
            return 1
        fi
    fi

    log "Resource validation PASSED - all scenarios can run"
    log ""

    # PHASE 3: Create static VMs
    if [ ${#static_scenarios[@]} -gt 0 ]; then
        log "=== PHASE 3: Creating static VMs ==="
        if ! create_static_vms "${static_scenarios[@]}"; then
            log "ERROR: Failed to create static VMs, aborting"
            return 1
        fi
    fi

    # Queue dynamic scenarios (requirements already calculated in phase 2)
    for scenario_script in "${dynamic_scenarios[@]}"; do
        local scenario_name
        scenario_name=$(basename "${scenario_script}" .sh)
        local req_file="${SCENARIO_STATUS}/${scenario_name}/requirements"

        queue_scenario "${scenario_script}" "${scenario_name}" "${req_file}"
    done

    # PHASE 4: Run static tests and dynamic scenarios in parallel
    log "=== PHASE 4: Running tests ==="

    local overall_result=0
    local static_pid=""
    local dynamic_result=0
    local static_result=0

    # Start static scenario tests in background
    if [ ${#static_scenarios[@]} -gt 0 ]; then
        log "Starting static scenario tests in background"
        run_static_tests "${static_scenarios[@]}" &
        static_pid=$!
        log "Static tests running in background (pid ${static_pid})"
    fi

    # Run dynamic scenarios with scheduler (foreground)
    if [ ${#dynamic_scenarios[@]} -gt 0 ]; then
        log "Dispatching dynamic scenarios"
        if ! dispatch_dynamic_scenarios; then
            dynamic_result=1
        fi
    fi

    # Wait for static tests to complete
    if [ -n "${static_pid}" ]; then
        log "Waiting for static tests to complete (pid ${static_pid})"
        if ! wait "${static_pid}"; then
            static_result=1
        fi
        log "Static tests completed"
    fi

    # Combine results
    if [ ${dynamic_result} -ne 0 ] || [ ${static_result} -ne 0 ]; then
        overall_result=1
    fi

    log "Orchestration complete (dynamic=${dynamic_result}, static=${static_result})"
    return ${overall_result}
}

# Show current scheduler status
show_status() {
    echo "=== Scheduler Status ==="
    echo "State directory: ${SCHEDULER_STATE_DIR}"
    echo ""

    echo "=== Resource Configuration ==="
    echo "Host total:        vcpus=${HOST_TOTAL_VCPUS}, memory=${HOST_TOTAL_MEMORY}MB"
    echo "System reserved:   vcpus=${SYSTEM_RESERVED_VCPUS}, memory=${SYSTEM_RESERVED_MEMORY}MB"
    echo "Available for VMs: vcpus=${HOST_AVAILABLE_VCPUS}, memory=${HOST_AVAILABLE_MEMORY}MB"
    echo ""

    echo "=== Resource Allocation ==="
    echo "Static requires:   vcpus=${STATIC_TOTAL_VCPUS}, memory=${STATIC_TOTAL_MEMORY}MB"
    echo "Dynamic available: vcpus=${DYNAMIC_AVAILABLE_VCPUS}, memory=${DYNAMIC_AVAILABLE_MEMORY}MB"
    echo "Max dynamic needs: vcpus=${MAX_DYNAMIC_VCPUS}, memory=${MAX_DYNAMIC_MEMORY}MB"
    echo ""

    echo "=== Dynamic Scheduler Usage ==="
    recalculate_resource_usage
    echo "Dynamic VMs using: vcpus=${current_vcpus}, memory=${current_memory}MB"
    echo ""

    echo "=== VMs ==="
    for vm_dir in "${VM_REGISTRY}"/*; do
        [ -d "${vm_dir}" ] || continue
        local vm_name
        vm_name=$(basename "${vm_dir}")
        local vm_state="${vm_dir}/state"
        if [ -f "${vm_state}" ]; then
            echo "${vm_name}:"
            cat "${vm_state}" | sed 's/^/  /'
        fi
    done
    echo ""

    echo "=== Scenarios ==="
    for queue_file in "${SCENARIO_QUEUE}"/*; do
        [ -f "${queue_file}" ] || continue
        local scenario_name
        scenario_name=$(basename "${queue_file}")
        echo "${scenario_name}:"
        cat "${queue_file}" | sed 's/^/  /'
    done
}

# Usage information
usage() {
    cat <<EOF
Usage: $(basename "$0") <command> [args]

Commands:
    orchestrate <scenario_dir>  Schedule and run all scenarios in directory
    status                      Show current scheduler state

Environment Variables:
    HOST_TOTAL_VCPUS       Total host vCPUs (default: 48)
    HOST_TOTAL_MEMORY      Total host memory in MB (default: 98304)
    SYSTEM_RESERVED_VCPUS  vCPUs reserved for host OS (default: 2)
    SYSTEM_RESERVED_MEMORY Memory reserved for host OS in MB (default: 4096)
    SCHEDULER_ENABLED      Enable scheduler mode (default: false)

Execution Flow:
    1. Classify scenarios (dynamic vs static)
    2. Calculate resource requirements for ALL scenarios
       - Parse static scenarios for VM requirements
       - Parse dynamic scenarios for VM requirements
    3. Validate resources BEFORE creating any VMs:
       - System reserved + static + max_dynamic <= host_total
    4. Create static VMs (in parallel)
    5. Run static tests and dynamic scenarios concurrently
EOF
}

# Main entry point
if [ $# -lt 1 ]; then
    usage
    exit 1
fi

command="$1"
shift

case "${command}" in
    orchestrate)
        if [ $# -lt 1 ]; then
            echo "ERROR: orchestrate requires scenario directory argument" >&2
            usage
            exit 1
        fi
        orchestrate "$1"
        ;;
    status)
        show_status
        ;;
    *)
        echo "ERROR: Unknown command: ${command}" >&2
        usage
        exit 1
        ;;
esac
