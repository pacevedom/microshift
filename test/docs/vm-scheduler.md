# Dynamic VM Scheduler for MicroShift CI Testing

## Overview

The Dynamic VM Scheduler is a resource-aware orchestration system for MicroShift scenario testing. It manages VM resources for parallel scenario execution, implementing VM reuse for compatible scenarios and queuing when host resources are exhausted.

## Problem Statement

Previously, MicroShift test scenarios created VMs in a 1:1 relationship with scenarios. All scenarios launched in parallel via GNU parallel, each creating its own VM. This approach had several limitations:

### Resource Exhaustion

When running many scenarios in parallel, the host could run out of resources:

```
Scenario A: needs 4 vCPUs, 8GB RAM  ─┐
Scenario B: needs 4 vCPUs, 8GB RAM  ─┼─► Total: 16 vCPUs, 32GB RAM
Scenario C: needs 4 vCPUs, 8GB RAM  ─┤    Host has: 12 vCPUs, 24GB RAM
Scenario D: needs 4 vCPUs, 8GB RAM  ─┘    Result: FAILURE
```

With GNU parallel, all scenarios start simultaneously. When the host runs out of resources, VMs fail to create, tests fail, and CI jobs fail unpredictably.

### No VM Reuse

Consider these scenarios with identical VM requirements:

```
el96-src@configuration.sh  → Creates VM (2 vCPU, 4GB) → Runs 5 min  → Destroys VM
el96-src@standard-suite1.sh → Creates VM (2 vCPU, 4GB) → Runs 10 min → Destroys VM
el96-src@standard-suite2.sh → Creates VM (2 vCPU, 4GB) → Runs 8 min  → Destroys VM
```

Each scenario spends ~5-10 minutes creating a VM that could have been reused. With VM reuse:

```
el96-src@configuration.sh  → Creates VM → Runs 5 min  ─┐
el96-src@standard-suite1.sh → Reuses VM → Runs 10 min  ├─► Same VM, no boot overhead
el96-src@standard-suite2.sh → Reuses VM → Runs 8 min  ─┘
```

### No Upfront Validation

Resource problems were discovered at runtime, after potentially hours of execution:

```
- Start 20 scenarios
- 15 scenarios running, 5 queued by parallel
- Host OOM, scenario 16 fails to create VM
- More failures cascade
Result: Wasted time before discovering resource issue
```

## Solution

The Dynamic VM Scheduler addresses these issues:

1. **Resource-aware scheduling**: Tracks host vCPU and memory usage in real-time, queues scenarios when resources are exhausted, dispatches them when resources become available
2. **VM reuse**: Scenarios with compatible requirements share VMs, eliminating redundant boot cycles
3. **Upfront validation**: Calculates all resource requirements before creating any VMs, failing fast if resources are insufficient
4. **Opt-in mechanism**: Scenarios explicitly opt into dynamic scheduling via a function; others run in legacy mode for backward compatibility

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                     ci_phase_boot_and_test.sh                    │
│                                                                  │
│   SCHEDULER_ENABLED=false  │  SCHEDULER_ENABLED=true             │
│   ────────────────────────┼──────────────────────────────────    │
│   GNU parallel             │  vm_scheduler.sh orchestrate        │
│   (legacy behavior)        │  (resource-aware scheduling)        │
└─────────────────────────────┬────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│                      vm_scheduler.sh                             │
│                                                                  │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │ Phase 1:        │  │ Phase 2:        │  │ Phase 3:        │  │
│  │ Classify        │─▶│ Calculate &     │─▶│ Create Static   │  │
│  │ Scenarios       │  │ Validate        │  │ VMs             │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
│           │                                         │            │
│           ▼                                         ▼            │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │ Phase 4: Run Tests                                          ││
│  │  ┌──────────────────┐    ┌──────────────────────────────┐   ││
│  │  │ Static Tests     │    │ Dynamic Scheduler            │   ││
│  │  │ (GNU parallel)   │    │ - Find compatible VM         │   ││
│  │  │                  │    │ - Or create new VM           │   ││
│  │  │                  │    │ - Or queue if no resources   │   ││
│  │  └──────────────────┘    └──────────────────────────────┘   ││
│  └─────────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              ▼                               ▼
┌──────────────────────────────┐  ┌──────────────────────────────┐
│   VM Registry                │  │  Scenario Queue              │
│   ${IMAGEDIR}/scheduler-     │  │  ${IMAGEDIR}/scheduler-      │
│   state/vms/                 │  │  state/queue/                │
│                              │  │                              │
│   dynamic-vm-001/            │  │  el96-src@config/            │
│   ├── state                  │  │  ├── script=path/to/file.sh  │
│   │   ├── vcpus=2            │  │  ├── requirements=path/req   │
│   │   ├── memory=4096        │  │  ├── status=queued|running   │
│   │   ├── status=in_use      │  │  └── queued_at=timestamp     │
│   │   └── current_scenario=  │  │                              │
│   └── scenario_history.log   │  │  el96-src@standard1/         │
│                              │  │  └── ...                     │
│   dynamic-vm-002/            │  │                              │
│   └── ...                    │  │                              │
└──────────────────────────────┘  └──────────────────────────────┘
```

### Component Details

#### vm_scheduler.sh

The main orchestrator responsible for:

- **Scenario classification**: Determines which scenarios are dynamic (have `dynamic_schedule_requirements()`) vs static
- **Resource calculation**: Parses VM requirements from all scenarios before execution
- **Validation**: Ensures resources are sufficient for all scenarios
- **Static VM management**: Creates static VMs in parallel via GNU parallel
- **Dynamic dispatch**: Manages the dispatch loop for dynamic scenarios with VM reuse

#### VM Registry

A file-based registry tracking all dynamic VMs:

```
${IMAGEDIR}/scheduler-state/vms/
├── dynamic-vm-001/
│   ├── state                    # Current VM state (vcpus, memory, status, etc.)
│   ├── scenario_history.log     # All scenarios that ran on this VM
│   └── creation_log -> ...      # Symlink to creation log
├── dynamic-vm-002/
│   └── ...
```

The `state` file contains:
```
vcpus=2
memory=4096
disksize=20
networks=default
fips=false
boot_image=rhel96-bootc-source
status=in_use|available|destroyed
current_scenario=el96-src@configuration
```

#### Scenario Queue

Tracks pending and running dynamic scenarios:

```
${IMAGEDIR}/scheduler-state/queue/
├── el96-src@configuration
│   ├── script=/path/to/scenario.sh
│   ├── requirements=/path/to/requirements
│   ├── status=queued|running|completed
│   ├── queued_at=2024-01-15T10:30:00
│   └── result=SUCCESS|FAILED
```

## Scenario Classification

### Dynamic Scenarios

Scenarios opt into dynamic scheduling by defining the `dynamic_schedule_requirements()` function:

```bash
#!/bin/bash

# Sourced from scenario.sh and uses functions defined there.

# Opt-in to dynamic VM scheduling by declaring requirements
dynamic_schedule_requirements() {
    cat <<EOF
min_vcpus=2
min_memory=4096
min_disksize=20
networks=default
boot_image=rhel96-bootc-source
fips=false
EOF
}

scenario_create_vms() {
    prepare_kickstart host1 kickstart-bootc.ks.template rhel96-bootc-source
    launch_vm --boot_blueprint rhel96-bootc
}

scenario_remove_vms() {
    remove_vm host1
}

scenario_run_tests() {
    run_tests host1 suites/configuration/
}
```

**Behavior of dynamic scenarios:**
- **VM sharing**: Can run on a VM created by another scenario if compatible
- **Queuing**: Wait in queue if no compatible VM available and resources exhausted
- **Lifecycle**: VM is not destroyed after scenario completes; released back to pool

### Static Scenarios

Scenarios without `dynamic_schedule_requirements()` run in static/legacy mode:

```bash
#!/bin/bash

# No dynamic_schedule_requirements() function = static mode

scenario_create_vms() {
    prepare_kickstart host1 kickstart-bootc.ks.template rhel96-bootc-source
    launch_vm --boot_blueprint rhel96-bootc
}

scenario_remove_vms() {
    remove_vm host1
}

scenario_run_tests() {
    run_tests host1 suites/my-tests/
}
```

**Behavior of static scenarios:**
- **Own VM**: Create their own dedicated VM
- **Parallel execution**: Run via GNU parallel as before
- **Lifecycle**: VM is destroyed after scenario completes

## Resource Model

### Resource Hierarchy

```
┌─────────────────────────────────────────────────────────────────┐
│                    HOST_TOTAL_VCPUS / HOST_TOTAL_MEMORY         │
│                    (auto-detected from system)                  │
├─────────────────────────────────────────────────────────────────┤
│  SYSTEM_RESERVED     │           HOST_AVAILABLE                 │
│  (for host OS,       │  ┌─────────────────────────────────────┐ │
│   hypervisor,        │  │ STATIC_TOTAL    │ DYNAMIC_AVAILABLE │ │
│   other processes)   │  │ (sum of all     │ (for dynamic      │ │
│                      │  │  static VMs)    │  scenarios)       │ │
│  Default: 2 vCPU     │  │                 │                   │ │
│           4GB RAM    │  │                 │                   │ │
└──────────────────────┴──┴─────────────────┴───────────────────┴─┘
```

### Calculation Example

```
Host system: 48 vCPUs, 96GB RAM

Configuration:
  HOST_TOTAL_VCPUS = 48
  HOST_TOTAL_MEMORY = 98304 (96GB in MB)
  SYSTEM_RESERVED_VCPUS = 4
  SYSTEM_RESERVED_MEMORY = 8192 (8GB)

Scenarios:
  Static scenarios (3):
    - el96-src@fips.sh:      4 vCPU, 8GB
    - el96-src@tuned.sh:     6 vCPU, 8GB
    - el96-src@multi-nic.sh: 4 vCPU, 8GB
    STATIC_TOTAL = 14 vCPU, 24GB

  Dynamic scenarios (4):
    - el96-src@configuration.sh:   2 vCPU, 4GB
    - el96-src@standard-suite1.sh: 2 vCPU, 4GB
    - el96-src@standard-suite2.sh: 2 vCPU, 4GB
    - el96-src@storage.sh:         4 vCPU, 8GB
    MAX_DYNAMIC = 4 vCPU, 8GB (largest scenario)

Calculation:
  HOST_AVAILABLE = HOST_TOTAL - SYSTEM_RESERVED
                 = (48 - 4) vCPU, (96 - 8) GB
                 = 44 vCPU, 88GB

  DYNAMIC_AVAILABLE = HOST_AVAILABLE - STATIC_TOTAL
                    = (44 - 14) vCPU, (88 - 24) GB
                    = 30 vCPU, 64GB

Validation:
  ✓ STATIC_TOTAL (14 vCPU) <= HOST_AVAILABLE (44 vCPU)
  ✓ MAX_DYNAMIC (4 vCPU)   <= DYNAMIC_AVAILABLE (30 vCPU)
  → Validation PASSED
```

### Validation Failures

The scheduler validates resources before creating any VMs:

**Static overflow:**
```
ERROR: Static scenarios require more vCPUs than available
  Required: 50, Available: 44
```

**Dynamic overflow:**
```
ERROR: Largest dynamic scenario requires more memory than available after static allocation
  Required: 32768MB, Available: 24576MB
```

## Scheduling Algorithm

### Dispatch Loop

```
┌─────────────────────────────────────────────────────────────────┐
│                     dispatch_dynamic_scenarios()                │
└─────────────────────────────────┬───────────────────────────────┘
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────┐
│ WHILE (pending_scenarios OR running_scenarios):                 │
│                                                                 │
│   FOR each queued scenario:                                     │
│     │                                                           │
│     ├─► Find compatible free VM?                                │
│     │   ├─ YES → Assign VM to scenario                          │
│     │   │        Run scenario on VM (background)                │
│     │   │                                                       │
│     │   └─ NO  → Resources available for new VM?                │
│     │            ├─ YES → Create new VM                         │
│     │            │        Register in VM registry               │
│     │            │        Run scenario on VM (background)       │
│     │            │                                              │
│     │            └─ NO  → Keep scenario queued                  │
│     │                     Log "resources exhausted"             │
│     │                                                           │
│   Wait for any background job to finish (wait -n)               │
│   │                                                             │
│   └─► On scenario completion:                                   │
│       ├─ Check queue for compatible scenario                    │
│       │  ├─ FOUND  → Assign VM, run next scenario               │
│       │  └─ NONE   → Destroy VM, reclaim resources              │
│       │                                                         │
│       └─ Wake up dispatch loop (freed resources may allow       │
│          queued scenarios to start)                             │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### VM Reuse Decision

When a scenario completes, the scheduler decides whether to reuse or destroy the VM:

```
Scenario A completes on VM dynamic-vm-001 (2 vCPU, 4GB, default network)

Queue contains:
  - Scenario B: needs 4 vCPU, 8GB     → NOT compatible (VM too small)
  - Scenario C: needs 2 vCPU, 4GB     → COMPATIBLE (exact match)
  - Scenario D: needs 1 vCPU, 2GB     → COMPATIBLE (VM exceeds requirements)

Decision: Assign VM to Scenario C (first compatible in queue)
```

If no compatible scenario is waiting:

```
Queue contains:
  - Scenario B: needs 4 vCPU, 8GB     → NOT compatible
  - Scenario E: needs FIPS VM         → NOT compatible

Decision: Destroy VM, reclaim resources
          Now Scenario B can potentially create a larger VM
```

## VM Compatibility

A VM is compatible with a scenario if it meets or exceeds ALL requirements.

### Compatibility Matrix

| Requirement | Check | Example |
|-------------|-------|---------|
| vCPUs | `vm_vcpus >= scenario_min_vcpus` | VM has 4, scenario needs 2 → OK |
| Memory | `vm_memory >= scenario_min_memory` | VM has 8GB, scenario needs 4GB → OK |
| Disk | `vm_disksize >= scenario_min_disksize` | VM has 30GB, scenario needs 20GB → OK |
| Networks | VM has superset of required networks | VM has [default,multus], scenario needs [default] → OK |
| FIPS | If scenario requires FIPS, VM must have it | Scenario needs FIPS, VM is standard → FAIL |
| Boot image | Superset matching (see below) | See boot image rules |

### Network Matching

```bash
# Scenario requires: networks=default
# VM has: networks=default,multus

Check: "default" in "default,multus" → YES, compatible

# Scenario requires: networks=default,multus
# VM has: networks=default

Check: "multus" in "default" → NO, NOT compatible
```

### Boot Image Compatibility

Boot images follow a hierarchy where "larger" images (with more packages) can satisfy scenarios needing "smaller" images:

**Flexible matching (superset works):**

```
Image Hierarchy:
  source ──────────► source-optionals
  brew-lrel ────────► brew-lrel-optional

Examples:
  - Scenario needs "rhel96-bootc-source"
  - VM has "rhel96-bootc-source-optionals"
  - Result: COMPATIBLE (optionals is superset of source)
```

**Exact match required (special configurations):**

| Image Type | Why Exact Match | Characteristics |
|------------|-----------------|-----------------|
| `*-fips` | FIPS mode requires kernel argument `fips=1` and crypto policy | Cannot be added at runtime |
| `*-tuned` | CPU manager policy, memory manager, requires reboots | Kernel parameters and system config |
| `*-isolated` | qemu-guest-agent for offline VM control | Package must be pre-installed |
| `*-ai-model-serving` | Embedded container images (~15GB) | Pre-loaded images |

**Special case:** `ai-model-serving` images include qemu-guest-agent, so they CAN run `isolated` scenarios.

### Compatibility Function

```bash
vm_satisfies_requirements() {
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
    boot_image_compatible "${vm_boot_image}" "${req_boot_image}" || return 1

    return 0  # VM is compatible
}
```

## Files Changed

### New Files

| File | Lines | Purpose |
|------|-------|---------|
| `test/bin/vm_scheduler.sh` | ~1185 | Main scheduler implementation |
| `test/docs/vm-scheduler.md` | - | This documentation |

### Modified Files

#### test/bin/scenario.sh

Added scheduler integration (~60 lines):

```bash
# New environment variables (lines 24-28)
SCHEDULER_ENABLED="${SCHEDULER_ENABLED:-false}"
SCHEDULER_VM_NAME="${SCHEDULER_VM_NAME:-}"
SCHEDULER_SCENARIO_NAME="${SCHEDULER_SCENARIO_NAME:-}"
SCHEDULER_IS_NEW_VM="${SCHEDULER_IS_NEW_VM:-true}"
SCHEDULER_STATE_DIR="${SCHEDULER_STATE_DIR:-${IMAGEDIR}/scheduler-state}"

# Modified full_vm_name() to return scheduler-assigned name (lines 49-67)
full_vm_name() {
    local -r base="${1}"
    if [ "${SCHEDULER_ENABLED}" = "true" ] && [ -n "${SCHEDULER_VM_NAME}" ]; then
        if [ "${base}" = "host1" ]; then
            echo "${SCHEDULER_VM_NAME}"
        else
            echo "${SCHEDULER_VM_NAME}-${base}"
        fi
        return
    fi
    # Legacy mode...
}

# New functions (lines 594-632)
get_scheduler_assigned_vm()       # Get VM name from scheduler
should_reuse_vm()                 # Check if VM should be reused
setup_vm_properties_from_existing() # Set up properties for reused VM

# Modified remove_vm() to release VM to scheduler (lines 1000-1014)
remove_vm() {
    if [ "${SCHEDULER_ENABLED}" = "true" ]; then
        echo "Scheduler mode: releasing VM to scheduler"
        rm -rf "${SCENARIO_INFO_DIR}/${SCENARIO}/vms/${vmname}"
        return 0
    fi
    # Original removal logic...
}
```

#### test/bin/ci_phase_boot_and_test.sh

Added scheduler configuration (~35 lines):

```bash
# Scheduler configuration (lines 80-96)
SCHEDULER_ENABLED="${SCHEDULER_ENABLED:-false}"
export SCHEDULER_ENABLED

# Auto-detect system resources
_SYSTEM_VCPUS=$(nproc 2>/dev/null || echo 48)
_SYSTEM_MEMORY_KB=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}')
_SYSTEM_MEMORY_MB=$((_SYSTEM_MEMORY_KB / 1024))

export HOST_TOTAL_VCPUS="${HOST_TOTAL_VCPUS:-${_SYSTEM_VCPUS}}"
export HOST_TOTAL_MEMORY="${HOST_TOTAL_MEMORY:-${_SYSTEM_MEMORY_MB}}"
export SYSTEM_RESERVED_VCPUS="${SYSTEM_RESERVED_VCPUS:-4}"
export SYSTEM_RESERVED_MEMORY="${SYSTEM_RESERVED_MEMORY:-8192}"

# Conditional execution (lines 100-128)
if [ "${SCHEDULER_ENABLED}" = "true" ]; then
    bash -x ./bin/vm_scheduler.sh orchestrate "${SCENARIOS_TO_RUN}"
else
    # Legacy GNU parallel execution
    parallel ... bash -x ./bin/scenario.sh create-and-run ...
fi
```

### Scenario Files (Opt-in)

The following scenarios have `dynamic_schedule_requirements()` added:

| Scenario | vCPUs | Memory | Networks | Boot Image |
|----------|-------|--------|----------|------------|
| `el96-src@configuration.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@standard-suite1.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@standard-suite2.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@router.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@backups.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@storage.sh` | 2 | 4096 | default | rhel96-bootc-source |
| `el96-src@auto-recovery.sh` | 2 | 4096 | default | rhel96-bootc-source |

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SCHEDULER_ENABLED` | `false` | Enable dynamic VM scheduler |
| `HOST_TOTAL_VCPUS` | `$(nproc)` | Total host vCPUs |
| `HOST_TOTAL_MEMORY` | auto-detected | Total host memory in MB |
| `SYSTEM_RESERVED_VCPUS` | `2` in scheduler, `4` in CI | vCPUs reserved for host OS |
| `SYSTEM_RESERVED_MEMORY` | `4096` in scheduler, `8192` in CI | Memory reserved for host OS (MB) |
| `VM_CREATE_TIMEOUT` | `600` | VM creation timeout in seconds (10 min) |
| `VM_TEST_TIMEOUT` | `3600` | Test execution timeout in seconds (60 min) |
| `LOCK_TIMEOUT` | `300` | Lock acquisition timeout in seconds (5 min) |

### Scenario-Level Configuration

Scenarios can override timeouts in their requirements:

```bash
dynamic_schedule_requirements() {
    cat <<EOF
min_vcpus=4
min_memory=8192
min_disksize=30
networks=default
boot_image=rhel96-bootc-source
fips=false
create_timeout=900
test_timeout=7200
EOF
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `min_vcpus` | integer | Yes | Minimum vCPUs required |
| `min_memory` | integer | Yes | Minimum memory in MB |
| `min_disksize` | integer | Yes | Minimum disk size in GB |
| `networks` | string | Yes | Comma-separated network names |
| `boot_image` | string | Yes | Boot image name |
| `fips` | boolean | Yes | Whether FIPS mode is required |
| `create_timeout` | integer | No | VM creation timeout in seconds |
| `test_timeout` | integer | No | Test execution timeout in seconds |

## Usage

### Enable Scheduler in CI

Set environment variables before running tests:

```bash
export SCHEDULER_ENABLED=true

# Optional: Override resource limits
export SYSTEM_RESERVED_VCPUS=4
export SYSTEM_RESERVED_MEMORY=8192

# Run tests
./test/bin/ci_phase_boot_and_test.sh
```

### Limit Dynamic Resources

To reserve more resources for the host or limit dynamic parallelism:

```bash
# Reserve 8 vCPUs and 16GB for system/static scenarios
export SYSTEM_RESERVED_VCPUS=8
export SYSTEM_RESERVED_MEMORY=16384

# This leaves fewer resources for dynamic scenarios,
# reducing parallelism but ensuring stability
```

### View Scheduler Status

During or after a run:

```bash
./test/bin/vm_scheduler.sh status
```

Output:
```
=== Scheduler Status ===
State directory: /path/to/scheduler-state

=== Resource Configuration ===
Host total:        vcpus=48, memory=98304MB
System reserved:   vcpus=4, memory=8192MB
Available for VMs: vcpus=44, memory=90112MB

=== Resource Allocation ===
Static requires:   vcpus=14, memory=24576MB
Dynamic available: vcpus=30, memory=65536MB
Max dynamic needs: vcpus=4, memory=8192MB

=== Dynamic Scheduler Usage ===
Dynamic VMs using: vcpus=8, memory=16384MB

=== VMs ===
dynamic-vm-001:
  vcpus=2
  memory=4096
  status=in_use
  current_scenario=el96-src@configuration

dynamic-vm-002:
  vcpus=2
  memory=4096
  status=available

=== Scenarios ===
el96-src@configuration:
  status=running
  started_at=2024-01-15T10:30:00

el96-src@standard-suite1:
  status=queued
  queued_at=2024-01-15T10:30:05
```

### Manual Orchestration

Run the scheduler directly:

```bash
export SCHEDULER_ENABLED=true
./test/bin/vm_scheduler.sh orchestrate ./test/scenarios-bootc/presubmits/
```

## Execution Flow

### Phase 1: Classification

```
Input: scenario_dir containing *.sh files

For each scenario:
  ├─ Source scenario file
  ├─ Check: type dynamic_schedule_requirements &>/dev/null
  │   ├─ YES → Add to dynamic_scenarios[]
  │   └─ NO  → Add to static_scenarios[]
  └─ Log classification

Output:
  dynamic_scenarios = [el96-src@config, el96-src@standard1, ...]
  static_scenarios  = [el96-src@fips, el96-src@tuned, ...]
```

### Phase 2: Resource Planning

```
For static scenarios:
  ├─ Parse launch_vm parameters from scenario file
  │   grep -E '^\s*launch_vm' | extract --vm_vcpus, --vm_memory
  ├─ Sum: STATIC_TOTAL_VCPUS, STATIC_TOTAL_MEMORY
  └─ Log each scenario's requirements

For dynamic scenarios:
  ├─ Call dynamic_schedule_requirements() function
  ├─ Parse output (min_vcpus, min_memory, etc.)
  ├─ Track MAX_DYNAMIC_VCPUS, MAX_DYNAMIC_MEMORY
  └─ Store requirements in ${SCENARIO_STATUS}/${name}/requirements

Calculate:
  DYNAMIC_AVAILABLE = HOST_AVAILABLE - STATIC_TOTAL

Validate:
  ├─ STATIC_TOTAL <= HOST_AVAILABLE
  └─ MAX_DYNAMIC <= DYNAMIC_AVAILABLE

  If validation fails → Exit with error (fail fast)
```

### Phase 3: Create Static VMs

```
Run via GNU parallel:
  parallel ... bash -x scenario.sh create ::: "${static_scenarios[@]}"

This creates all static VMs simultaneously.
Each VM is independent and tied to its scenario.
```

### Phase 4: Run Tests

```
┌─────────────────────────────────┐  ┌─────────────────────────────────┐
│ Static Tests (background)       │  │ Dynamic Scheduler (foreground)  │
│                                 │  │                                 │
│ parallel ...                    │  │ dispatch_dynamic_scenarios()    │
│   scenario.sh run               │  │                                 │
│   ::: "${static_scenarios[@]}"  │  │ - Queue all dynamic scenarios   │
│                                 │  │ - Dispatch loop with VM reuse   │
│                                 │  │ - Wait for completion           │
└─────────────────────────────────┘  └─────────────────────────────────┘
           │                                      │
           └──────────────┬───────────────────────┘
                          ▼
                  Wait for both to complete
                  Return combined result
```

## Logging and Debugging

### Log Locations

| Log | Location | Contents |
|-----|----------|----------|
| Scheduler log | `${IMAGEDIR}/scheduler-state/scheduler.log` | All scheduler decisions and state changes |
| VM creation | `${SCENARIO_INFO_DIR}/${scenario}/create.log` | VM creation output (kickstart, boot) |
| Test run | `${SCENARIO_INFO_DIR}/${scenario}/run.log` | Test execution output |
| VM history | `${IMAGEDIR}/scheduler-state/vms/${vm}/scenario_history.log` | All scenarios that ran on this VM |
| Boot/run combined | `${SCENARIO_INFO_DIR}/${scenario}/boot_and_run.log` | Legacy format (static scenarios) |

### Scheduler Log Format

```
[2024-01-15 10:30:00] Initializing scheduler state directory
[2024-01-15 10:30:00] Scheduler state initialized
[2024-01-15 10:30:00]   Host total: vcpus=48, memory=98304MB
[2024-01-15 10:30:00] Starting orchestration for scenarios in /path/to/scenarios
[2024-01-15 10:30:00] === PHASE 1: Classifying scenarios ===
[2024-01-15 10:30:00]   Dynamic: el96-src@configuration.sh
[2024-01-15 10:30:00]   Static: el96-src@fips.sh
[2024-01-15 10:30:01] === PHASE 2: Resource Planning and Validation ===
[2024-01-15 10:30:01] Resource validation PASSED - all scenarios can run
[2024-01-15 10:30:01] === PHASE 3: Creating static VMs ===
[2024-01-15 10:30:01] Creating 2 static VMs in parallel
[2024-01-15 10:35:00] All static VMs created successfully
[2024-01-15 10:35:00] === PHASE 4: Running tests ===
[2024-01-15 10:35:00] Queued scenario el96-src@configuration
[2024-01-15 10:35:00] Creating new VM dynamic-vm-001 for el96-src@configuration
[2024-01-15 10:40:00] Scenario el96-src@configuration finished (pid 12345)
[2024-01-15 10:40:00] Assigned VM dynamic-vm-001 to scenario el96-src@standard1 (reuse)
```

### VM History Log Format

```
2024-01-15T10:35:00+00:00 CREATED for el96-src@configuration
2024-01-15T10:35:05+00:00 START el96-src@configuration
2024-01-15T10:40:00+00:00 END el96-src@configuration SUCCESS
2024-01-15T10:40:05+00:00 START el96-src@standard-suite1
2024-01-15T10:50:00+00:00 END el96-src@standard-suite1 SUCCESS
2024-01-15T10:50:05+00:00 START el96-src@standard-suite2
2024-01-15T11:00:00+00:00 END el96-src@standard-suite2 FAILED
2024-01-15T11:00:05+00:00 DESTROYED (no compatible scenarios)
```

### Debugging Failed Scenarios

1. **Find which VM ran the scenario:**
   ```bash
   cat ${IMAGEDIR}/scheduler-state/scenarios/el96-src@configuration/vm_assignment
   # Output: dynamic-vm-001
   ```

2. **Check VM's scenario history:**
   ```bash
   cat ${IMAGEDIR}/scheduler-state/vms/dynamic-vm-001/scenario_history.log
   ```

3. **View VM creation log:**
   ```bash
   cat ${SCENARIO_INFO_DIR}/el96-src@configuration/create.log
   ```

4. **View test execution log:**
   ```bash
   cat ${SCENARIO_INFO_DIR}/el96-src@configuration/run.log
   ```

5. **Check if VM was reused:**
   ```bash
   cat ${IMAGEDIR}/scheduler-state/scenarios/el96-src@configuration/vm_reused
   # Output: true or false
   ```

### Debugging Resource Issues

```bash
# View current resource state
./test/bin/vm_scheduler.sh status

# Check scheduler decisions
grep -E "(exhausted|FAILED|ERROR)" ${IMAGEDIR}/scheduler-state/scheduler.log

# Check which scenarios are queued
ls ${IMAGEDIR}/scheduler-state/queue/

# Check active VMs
ls ${IMAGEDIR}/scheduler-state/vms/
```

## Troubleshooting

### Common Issues

#### "Static scenarios require more vCPUs than available"

**Cause:** Sum of all static scenario VM requirements exceeds host capacity after system reservation.

**Solution:**
- Reduce `SYSTEM_RESERVED_VCPUS`
- Convert some static scenarios to dynamic
- Run fewer static scenarios in parallel

#### "Largest dynamic scenario requires more memory than available"

**Cause:** After allocating for static scenarios, not enough resources remain for the largest dynamic scenario.

**Solution:**
- Reduce static scenario count
- Reduce `SYSTEM_RESERVED_MEMORY`
- Reduce requirements of the largest dynamic scenario

#### VM creation timeout

**Cause:** VM took longer than `VM_CREATE_TIMEOUT` (default 10 minutes) to boot.

**Solution:**
- Increase `VM_CREATE_TIMEOUT`
- Check VM creation logs for errors
- Verify kickstart template is correct

#### Scenarios stuck in queue

**Cause:** No compatible VMs available and no resources for new VMs.

**Solution:**
- Wait for running scenarios to complete
- Check if resource calculations are correct
- Verify scenarios have compatible requirements

### Viewing Active Libvirt VMs

```bash
# List all VMs
sudo virsh list --all

# Check VM resources
sudo virsh dominfo dynamic-vm-001

# View VM console (for debugging boot issues)
sudo virsh console dynamic-vm-001
```

## Performance Considerations

### VM Reuse Benefits

| Metric | Without Reuse | With Reuse | Savings |
|--------|---------------|------------|---------|
| VM boots | 7 | 1 | 6 boots (~30-60 min) |
| Total time | 7 × (boot + test) | 1 × boot + 7 × test | ~30-60 min |
| Resource churn | High | Low | Reduced I/O, memory pressure |

### Optimal Configuration

1. **Group similar scenarios:** Scenarios with identical requirements should be made dynamic to maximize reuse
2. **Balance static/dynamic:** Special scenarios (FIPS, tuned) must be static; standard scenarios should be dynamic
3. **Right-size reservations:** Don't over-reserve system resources, but leave enough for stability

### Parallelism Limits

Dynamic parallelism is limited by:
- Available resources (vCPUs, memory)
- Largest scenario's requirements
- Number of compatible scenarios in queue

Example:
```
DYNAMIC_AVAILABLE = 8 vCPUs, 16GB
Scenario requirements = 2 vCPU, 4GB each

Max parallel dynamic VMs = min(8/2, 16/4) = 4

If 10 dynamic scenarios: 4 run, 6 queue
```

## Backward Compatibility

The scheduler is designed for full backward compatibility:

| Mode | Behavior |
|------|----------|
| `SCHEDULER_ENABLED=false` | All scenarios run via GNU parallel (legacy) |
| `SCHEDULER_ENABLED=true`, no dynamic scenarios | Static scenarios run via parallel, scheduler is no-op |
| `SCHEDULER_ENABLED=true`, mixed | Static via parallel, dynamic via scheduler |
| Individual scenario | `scenario.sh create-and-run` works as before |

### Migration Path

1. **No changes required:** Existing scenarios continue to work
2. **Gradual opt-in:** Add `dynamic_schedule_requirements()` to scenarios one by one
3. **Test locally:** Run with `SCHEDULER_ENABLED=true` to verify
4. **Enable in CI:** Set environment variable in CI configuration

## Adding a New Dynamic Scenario

### Step 1: Add Requirements Function

```bash
#!/bin/bash

# Sourced from scenario.sh and uses functions defined there.

# Opt-in to dynamic VM scheduling by declaring requirements
dynamic_schedule_requirements() {
    cat <<EOF
min_vcpus=2
min_memory=4096
min_disksize=20
networks=default
boot_image=rhel96-bootc-source
fips=false
EOF
}

scenario_create_vms() {
    prepare_kickstart host1 kickstart-bootc.ks.template rhel96-bootc-source
    launch_vm --boot_blueprint rhel96-bootc
}

scenario_remove_vms() {
    remove_vm host1
}

scenario_run_tests() {
    run_tests host1 suites/my-tests/
}
```

### Step 2: Match Requirements to launch_vm

Ensure `dynamic_schedule_requirements()` matches your `launch_vm` parameters:

| launch_vm Parameter | Requirements Field |
|---------------------|-------------------|
| `--vm_vcpus N` | `min_vcpus=N` |
| `--vm_memory N` | `min_memory=N` |
| `--vm_disksize N` | `min_disksize=N` |
| Network configuration | `networks=...` |
| Boot blueprint | `boot_image=...` |
| FIPS mode | `fips=true/false` |

### Step 3: Test Locally

```bash
export SCHEDULER_ENABLED=true
./test/bin/vm_scheduler.sh orchestrate ./test/scenarios-bootc/presubmits/
```

### Step 4: Verify VM Reuse

Check if your scenario is reusing VMs:

```bash
cat ${IMAGEDIR}/scheduler-state/scenarios/your-scenario/vm_reused
# Should be "true" if reused, "false" if new VM created
```

## Security Considerations

The scheduler implements several security measures to ensure safe operation in CI environments.

### Locking Mechanism

The scheduler uses a robust file-based locking mechanism with multiple safety features:

```bash
# Lock acquisition with timeout and stale detection
acquire_lock() {
    local lock_name="$1"
    local lock_file="${LOCK_DIR}/${lock_name}.lock"
    local lock_timeout="${LOCK_TIMEOUT:-300}"  # 5 minute default
    local stale_timeout=60  # Consider lock stale after 60 seconds

    while true; do
        if mkdir "${lock_file}" 2>/dev/null; then
            # Record PID and timestamp for stale detection
            echo "$$" > "${lock_file}/pid"
            date +%s > "${lock_file}/timestamp"
            HELD_LOCKS+=("${lock_name}")
            return 0
        fi

        # Check if holder process is dead (stale lock)
        if [ -n "${lock_pid}" ] && ! kill -0 "${lock_pid}" 2>/dev/null; then
            rm -rf "${lock_file}"  # Remove stale lock
            continue
        fi

        # Timeout check
        if [ "${elapsed}" -gt "${lock_timeout}" ]; then
            return 1
        fi
        sleep 0.1
    done
}
```

**Features:**
- **Atomic acquisition**: Uses `mkdir` which is atomic on POSIX systems
- **Timeout**: Configurable via `LOCK_TIMEOUT` (default: 300 seconds)
- **Stale lock detection**: Checks if holding process is still alive
- **Cleanup on exit**: Trap handler releases all held locks on EXIT/INT/TERM

### Input Sanitization

Scenario names and other user-controllable inputs are sanitized before use in sed commands:

```bash
# Escape special characters for sed replacement strings
sed_escape() {
    local input="$1"
    printf '%s' "${input}" | sed -e 's/[\/&]/\\&/g' -e ':a;N;$!ba;s/\n/\\n/g'
}

# Usage in state updates
escaped_scenario_name=$(sed_escape "${scenario_name}")
sed -i "s/^current_scenario=.*/current_scenario=${escaped_scenario_name}/" "${vm_state}"
```

This prevents injection attacks if scenario names contain special characters like `/`, `&`, or `\`.

### Path Safety

All `rm -rf` operations use the `:?` bash extension to prevent accidental deletion if variables are empty:

```bash
rm -rf "${VM_REGISTRY:?}"/* "${SCENARIO_QUEUE:?}"/*
rm -rf "${VM_DISK_BASEDIR:?}/${vm_pool_name}"
```

### VM Name Generation

Dynamic VM names are generated using a controlled format that prevents injection:

```bash
vm_name="dynamic-vm-$(printf '%03d' "${count}")"
```

This ensures VM names only contain safe characters (`dynamic-vm-001`, `dynamic-vm-002`, etc.).

### Trusted Code Execution

Scenario scripts are sourced to extract requirements:

```bash
source "${scenario_script}"
dynamic_schedule_requirements > "${output_file}"
```

**Important**: This executes arbitrary code from scenario files. This is by design since:
- Scenario files are part of the repository (trusted code)
- They are not user-uploaded content
- The same scripts would be executed during test runs anyway

### Race Condition Protection

| Operation | Protection |
|-----------|------------|
| VM name allocation | Atomic `mkdir` - only one process succeeds |
| VM state access | Protected by `vm_dispatch` lock |
| Resource calculation | Protected by `vm_dispatch` lock |
| Queue file updates | Each scenario has separate file |

### Configuration Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOCK_TIMEOUT` | `300` | Maximum seconds to wait for lock acquisition |

## Future Enhancements

Potential improvements for future versions:

1. **Priority scheduling:** High-priority scenarios run first
2. **Resource estimation:** Learn VM requirements from historical data
3. **VM warm pool:** Keep idle VMs for faster scenario starts
4. **Cross-host scheduling:** Distribute scenarios across multiple hypervisors
5. **Cost optimization:** Prefer smaller VMs when possible
