#!/bin/bash

# Sourced from scenario.sh and uses functions defined there.

scenario_create_vms() {
    prepare_kickstart host1 kickstart.ks.template rhel-9.6-microshift-source
    launch_vm
}

scenario_remove_vms() {
    remove_vm host1
}

scenario_run_tests() {
    run_tests host1 \
        suites/standard1/containers-policy.robot \
        suites/standard1/etcd.robot \
        suites/standard1/hostname.robot \
        suites/standard1/networking-smoke.robot \
        suites/standard1/show-config.robot \
        suites/standard1/dns.robot
}
