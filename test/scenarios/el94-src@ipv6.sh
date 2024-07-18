#!/bin/bash

# Sourced from scenario.sh and uses functions defined there.

# Redefine network-related settings to use the dedicated network bridge
# shellcheck disable=SC2034  # used elsewhere
VM_BRIDGE_IP="$(get_vm_bridge_ip "${VM_IPV6_NETWORK}")"
# shellcheck disable=SC2034  # used elsewhere
WEB_SERVER_URL="http://[${VM_BRIDGE_IP}]:${WEB_SERVER_PORT}"
# shellcheck disable=SC2034  # used elsewhere
BOOTC_REGISTRY_URL="${VM_BRIDGE_IP}:5000"

scenario_create_vms() {
    prepare_kickstart host1 kickstart.ks.template rhel-9.4-microshift-source
    launch_vm host1 rhel-9.4 "${VM_IPV6_NETWORK}"
}

scenario_remove_vms() {
    remove_vm host1
}

scenario_run_tests() {
    run_tests host1 suites/ipv6/singlestack.robot
}
