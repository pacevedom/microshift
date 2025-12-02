#!/usr/bin/env python3
"""
Test runner for MicroShift scenarios.

This runner manages VM lifecycle and executes test scenarios based on
configuration files.
"""

import argparse
import logging
import os
import re
import subprocess
import tempfile
import threading
import time
import yaml
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Any
from queue import Queue
from enum import Enum

TESTDIR=os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ROOTDIR=os.path.dirname(TESTDIR)
IMAGEDIR=os.path.join(ROOTDIR, "_output", "test-images")
VMDIR=os.path.join(IMAGEDIR, "scenario-info")

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)


class VMStatus(Enum):
    """VM status enumeration."""
    AVAILABLE = "available"
    BUSY = "busy"
    CREATING = "creating"
    DESTROYING = "destroying"
    DESTROYED = "destroyed"


@dataclass
class VMConfig:
    """VM configuration requirements."""
    cpu: int = 2
    memory: int = 4096  # in MB
    network: List[str] = None
    optionals: bool = False
    fips: bool = False

    def __post_init__(self):
        if self.packages is None:
            self.packages = []


@dataclass
class Scenario:
    """Test scenario configuration."""
    name: str
    description: str
    suites: List[str]
    variables: Dict[str, Any]
    vm_config: VMConfig
    is_upgrade: bool = False


@dataclass
class Launcher:
    """Launcher configuration."""
    name: str
    description: str
    variables: Dict[str, Any]
    base_image: str
    kickstart_file: str


@dataclass
class VM:
    """VM instance representation."""
    vm_id: str
    name: str
    status: VMStatus = VMStatus.CREATING
    lock: threading.Lock = None
    scenario_script_path: Optional[str] = None
    launcher: Optional['Launcher'] = None
    ip_address: Optional[str] = None
    config: VMConfig = None

    def __post_init__(self):
        if self.lock is None:
            self.lock = threading.Lock()


    def matches_requirements(self, required_config: VMConfig) -> bool:
        """Check if this VM matches the required configuration."""

        ok_cpu = self.config.cpu >= required_config.cpu
        ok_memory = self.config.memory >= required_config.memory
        if required_config.network is None:
            ok_network = True
        elif self.config.network is None:
            ok_network = False
        else:
            ok_network = all(net in self.config.network for net in required_config.network)

        ok_fips = self.config.fips == required_config.fips
        ok_baseimage = self.launcher.base_image == required_config.base_image
        ok_optionals = self.config.optionals == required_config.optionals

        return all(ok_cpu, ok_memory, ok_network, ok_fips, ok_baseimage, ok_optionals)

    def get_vm_ip(self) -> Optional[str]:
        """
        Get the VM IP address from the scenario property file.

        Returns:
            VM IP address or None if not found
        """
        if not self.name:
            logger.warning(f"VM {self.vm_id} has no name, cannot get IP")
            return None

        # VM name in scenario is "host1"
        vmname = "host1"
        #TODO dont need this. must be different path.
        ip_file = os.path.join(
            VMDIR,
            self.name,
            "vms",
            vmname,
            "ip"
        )

        if not os.path.exists(ip_file):
            logger.warning(f"IP file not found at {ip_file}")
            return None

        try:
            with open(ip_file, 'r') as f:
                ip = f.read().strip()
            if not ip:
                logger.warning(f"IP file {ip_file} is empty")
                return None
            return ip
        except Exception as e:
            logger.error(f"Error reading IP file {ip_file}: {e}")
            return None

    def generate_scenario_script(self) -> str:
        """
        Generate a temporary scenario script file for VM creation.

        Returns:
            Path to the generated scenario script file
        """
        if not self.launcher:
            raise ValueError("Launcher must be set before generating scenario script")

        os.makedirs(os.path.join(VMDIR, self.name), exist_ok=True)
        scenario_script_path = os.path.join(VMDIR, f"{self.name}.sh")
        fd = os.open(scenario_script_path, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o600)

        try:
            vmhostname = "host1"
            launch_args = [
                f"--vmname {vmhostname}",
                f"--vm_vcpus {self.config.cpu}",
                f"--vm_memory {self.config.memory}",
            ]

            #TODO rework this one.
            boot_blueprint = self.launcher.variables.get("BOOT_BLUEPRINT")
            if boot_blueprint:
                launch_args.append(f"--boot_blueprint {boot_blueprint}")

            #TODO this might be a list of networks. Take driver too as well. if not present, add it.
            if self.config.network:
                launch_args.append(f"--network {self.config.network}")
            else:
                launch_args.append("--network default")

            fips_enabled = self.launcher.variables.get("FIPS_MODE", "false").lower() == "true"
            fips_kickstart = "true" if fips_enabled else "false"
            if fips_enabled:
                launch_args.append("--fips")

            #TODO this could have additional arugments, look them up. i have ipv6, fips and the rest is just template and base image. so those could well be in the file for the launcher too.
            # and get defaults.
            scenario_create_vms = (
                f"scenario_create_vms() {{\n"
                f"    prepare_kickstart {vmhostname} {self.launcher.kickstart_file} {self.launcher.base_image} {fips_kickstart}\n"
                f"    launch_vm {' '.join(launch_args)}\n"
                f"}}\n\n"
            )

            scenario_remove_vms = (
                f"scenario_remove_vms() {{\n"
                f"    remove_vm {vmhostname}\n"
                f"}}\n\n"
            )

            script_content = (
                "#!/bin/bash\n"
                "#\n"
                "# Auto-generated scenario script for VM creation\n"
                "# This script is generated by the test runner\n"
                "#\n\n"
                "# Sourced from scenario.sh and uses functions defined there.\n\n"
                f"{scenario_create_vms}"
                f"{scenario_remove_vms}"
            )

            with os.fdopen(fd, 'w') as f:
                f.write(script_content)
            # Make the script executable
            os.chmod(scenario_script_path, 0o755)

            logger.debug(f"Generated scenario script at: {scenario_script_path}")
            return scenario_script_path

        except Exception as e:
                raise RuntimeError(f"Failed to generate scenario script: {e}") from e



class VMManager:
    """Manages VM lifecycle and pool."""

    def __init__(self, total_cpus: int = 4, total_memory: int = 8192):
        """
        Initialize VM manager.

        Args:
            total_cpus: Total number of CPUs available for all running VMs
            total_memory: Total memory available for all running VMs (in MB)
        """
        self.total_cpus = total_cpus
        self.total_memory = total_memory  # in MB
        self.vms: Dict[str, VM] = {}
        self.vm_lock = threading.Lock()
        self.available_vms: Queue = Queue()
        self.available_cpus = 0
        self.available_memory = 0

    def find_available_vm(self, required_config: VMConfig) -> Optional[VM]:
        """
        Find an available VM matching the requirements.

        Args:
            required_config: Required VM configuration

        Returns:
            Available VM or None if not found
        """
        for vm in self.vms.values():
            if (vm.status == VMStatus.AVAILABLE and
                    vm.matches_requirements(required_config)):
                return vm
        return None

    def wait_for_available_vm(self, required_config: VMConfig, launcher: Launcher) -> VM:
        """
        Wait for an available VM matching the requirements.

        Args:
            required_config: Required VM configuration
            launcher: Launcher configuration to use for creating new VMs

        Returns:
            Available VM
        """
        while True:
            with self.vm_lock:
                vm = self.find_available_vm(required_config)
                if vm:
                    return vm
                logger.info(f"No available VM found, checking if we can create one")
                if self.can_create_vm(required_config):
                    return self.create_vm(required_config, launcher)
            logger.info("No available VM found, waiting...")
            time.sleep(5)
        return None

    def can_create_vm(self, required_config: VMConfig) -> bool:
        """Check if we can create a new VM (under limit)."""
        used_cpus = sum(vm.config.cpu for vm in self.vms.values()
                        if vm.status != VMStatus.DESTROYING)
        used_memory = sum(vm.config.memory for vm in self.vms.values()
                          if vm.status != VMStatus.DESTROYING)
        available_cpus = self.total_cpus - used_cpus
        available_memory = self.total_memory - used_memory
        enough_cpus = available_cpus >= required_config.cpu
        enough_memory = available_memory >= required_config.memory
        return enough_cpus and enough_memory

    def create_vm(self, vm_config: VMConfig, launcher: Launcher) -> VM:
        """
        Create a new VM using the specified launcher.

        Args:
            vm_config: VM configuration requirements
            launcher: Launcher configuration to use

        Returns:
            Created VM instance
        """
        vm_id = f"{int(time.time())}{len(self.vms)}"
        vm = VM(
            vm_id=vm_id,
            name=f"{launcher.name}{vm_id}",
            config=vm_config,
            status=VMStatus.CREATING,
            launcher=launcher
        )

        logger.info(f"Creating VM {vm_id}")

        try:
            scenario_script = vm.generate_scenario_script()
            vm.scenario_script_path = scenario_script
            logger.info(f"Scenario script path: {scenario_script}")
            scenario_sh_path = os.path.join(TESTDIR, "bin", "scenario.sh")
            if not os.path.exists(scenario_sh_path):
                raise FileNotFoundError(
                    f"scenario.sh not found at {scenario_sh_path}"
                )

            # Call scenario.sh create with the generated script
            logger.info(f"Creating virtual machine {vm.name} with script: {scenario_script}")

            result = subprocess.run(
                [scenario_sh_path, "create", scenario_script],
                capture_output=True,
                text=True,
                check=False
            )

            if result.returncode != 0:
                error_msg = (
                    f"Failed to create VM {vm_id}."
                    f"Return code: {result.returncode}\n"
                    f"STDOUT: {result.stdout}\n"
                    f"STDERR: {result.stderr}"
                )
                logger.error(error_msg)                # Clean up the script on failure
                raise RuntimeError(error_msg)

            vm_ip = vm.get_vm_ip()
            if not vm_ip:
                raise RuntimeError(f"Could not get IP address for VM {vm.vm_id}")
            vm.ip_address = vm_ip
            vm.status = VMStatus.BUSY
            self.vms[vm_id] = vm
            logger.info(f"VM {vm.vm_id}:{vm.name} created successfully with IP: {vm.ip_address}. Ready for tests")
            return vm

        except Exception as e:
            logger.error(f"Error creating VM {vm_id}: {e}")
            raise

    #TODO should i use the top level lock instead?
    def mark_vm_busy(self, vm: VM):
        """Mark a VM as busy."""
        with vm.lock:
            vm.status = VMStatus.BUSY
            logger.info(f"VM {vm.vm_id} marked as busy")

    def mark_vm_available(self, vm: VM):
        """Mark a VM as available."""
        with vm.lock:
            vm.status = VMStatus.AVAILABLE
            logger.info(f"VM {vm.vm_id} marked as available")

    def destroy_vm(self, vm: VM):
        """
        Destroy a VM using the cleanup script.

        Args:
            vm: VM instance to destroy
        """
        with vm.lock:
            vm.status = VMStatus.DESTROYING
            logger.info(f"Destroying VM {vm.vm_id}")

        try:
            if not vm.scenario_script_path:
                logger.warning(
                    f"VM {vm.vm_id} has no scenario script path, "
                    "skipping cleanup"
                )
                return

            if not os.path.exists(vm.scenario_script_path):
                logger.warning(
                    f"Scenario script {vm.scenario_script_path} not found, "
                    "skipping cleanup"
                )
                return

            scenario_sh_path = os.path.join(TESTDIR, "bin", "scenario.sh")
            if not os.path.exists(scenario_sh_path):
                logger.error(
                    f"scenario.sh not found at {scenario_sh_path}, "
                    "cannot cleanup VM"
                )
                return
            logger.info(
                f"Calling scenario.sh cleanup with script: "
                f"{vm.scenario_script_path}"
            )

            result = subprocess.run(
                [scenario_sh_path, "cleanup", vm.scenario_script_path],
                capture_output=True,
                text=True,
                check=False
            )
            if result.returncode != 0:
                logger.warning(
                    f"Cleanup for VM {vm.vm_id} returned non-zero exit code: "
                    f"{result.returncode}\n"
                    f"STDOUT: {result.stdout}\n"
                    f"STDERR: {result.stderr}"
                )
            else:
                logger.info(f"VM {vm.vm_id} cleaned up successfully")

        except Exception as e:
            logger.error(f"Error destroying VM {vm.vm_id}: {e}")

        finally:
            with self.vm_lock:
                if vm.vm_id in self.vms:
                    del self.vms[vm.vm_id]
            logger.info(f"VM {vm.vm_id} destroyed")


class TestRunner:
    """Main test runner orchestrator."""

    def __init__(self, scenarios_file: str, launchers_file: str, total_cpus: int = 4, total_memory: int = 8192):
        """
        Initialize test runner.

        Args:
            scenarios_file: Path to scenarios configuration file
            launchers_file: Path to launchers configuration file
            max_vms: Maximum number of VMs that can run simultaneously
        """
        self.scenarios_file = scenarios_file
        self.launchers_file = launchers_file
        self.vm_manager = VMManager(total_cpus=total_cpus, total_memory=total_memory)
        self.scenarios: List[Scenario] = []
        self.launchers: Dict[str, Launcher] = {}
        self.test_results: Dict[str, bool] = {}
        self.results_lock = threading.Lock()

    def load_scenarios(self) -> List[Scenario]:
        """Load scenarios from configuration file."""
        logger.info(f"Loading scenarios from {self.scenarios_file}")
        with open(self.scenarios_file, 'r') as f:
            data = yaml.safe_load(f) or {}

        scenarios = []
        for scenario_data in data.get('scenarios', []):
            vm_config_data = scenario_data.get('config', {})
            vm_config = VMConfig(
                cpu=vm_config_data.get('cpu', 2),
                memory=vm_config_data.get('memory', 4096),
                network=vm_config_data.get('network', None),
                optionals=config_data.get('optionals', False),
                fips=config_data.get('fips', False)
                base_image=config_data.get('base_image', None)
            )

            scenario = Scenario(
                name=scenario_data['name'],
                description=scenario_data.get('description', ''),
                suites=scenario_data.get('suites', []),
                variables=scenario_data.get('variables', {}),
                vm_config=vm_config,
                is_upgrade=scenario_data.get('is_upgrade', False)
            )
            scenarios.append(scenario)

        logger.info(f"Loaded {len(scenarios)} scenarios")
        return scenarios

    def load_launchers(self) -> Dict[str, Launcher]:
        """Load launchers from configuration file."""
        logger.info(f"Loading launchers from {self.launchers_file}")
        with open(self.launchers_file, 'r') as f:
            data = yaml.safe_load(f) or {}

        launchers = {}
        for launcher_data in data.get('launchers', []):
            launcher = Launcher(
                name=launcher_data['name'],
                description=launcher_data.get('description', ''),
                variables=launcher_data.get('variables', {}),
                base_image=launcher_data['base_image'],
                kickstart_file=launcher_data['kickstart_file']
            )
            launchers[launcher.name] = launcher

        logger.info(f"Loaded {len(launchers)} launchers")
        return launchers


    def select_launcher(self, scenario: Scenario) -> Launcher:
        """
        Select an appropriate launcher for a scenario.

        Args:
            scenario: Scenario to select launcher for

        Returns:
            Selected launcher
        """
        #TODO this is all wrong. a launcher should be selected based on the config of the scenario.
        # For now, use a default launcher. In the future, this could be
        # based on scenario requirements or configuration.
        default_launcher_name = "el96-src@standard-vm"

        if default_launcher_name in self.launchers:
            return self.launchers[default_launcher_name]

        # Fallback to first available launcher
        if self.launchers:
            return next(iter(self.launchers.values()))

        raise ValueError("No launchers available")

    def generate_test_script(self, scenario: Scenario) -> str:
        """
        Generate a new test execution script with only the scenario_run_tests function.

        Args:
            scenario: The scenario configuration object

        Returns:
            Path to the generated test script file
        """
        vmhostname = "host1"

        # Generate scenario_run_tests function
        suites_args = ' '.join(scenario.suites)
        scenario_run_tests = (
            f"scenario_run_tests() {{\n"
            f"    run_tests {vmhostname} {suites_args}\n"
            f"}}\n"
        )

        test_script_path = os.path.join(VMDIR, f"{scenario.name}.sh")

        script_content = (
            "#!/bin/bash\n"
            "#\n"
            "# Auto-generated test script for scenario execution\n"
            "# This script is generated by the test runner\n"
            "#\n\n"
            "# Sourced from scenario.sh and uses functions defined there.\n\n"
            f"{scenario_run_tests}"
        )

        # Write the new test script
        with open(test_script_path, 'w') as f:
            f.write(script_content)

        # Make the script executable
        os.chmod(test_script_path, 0o755)

        logger.debug(f"Generated test script at: {test_script_path}")
        return test_script_path

    def run_test(self, scenario: Scenario) -> bool:
        """
        Run a single test scenario.

        Args:
            scenario: Scenario to run

        Returns:
            True if test passed, False otherwise
        """
        logger.info(f"Starting test: {scenario.name}")

        # Select a launcher for this scenario
        launcher = self.select_launcher(scenario)

        # Find or create a VM
        #TODO launchers should be part of the vm manager.
        vm = self.vm_manager.wait_for_available_vm(scenario.vm_config, launcher)
        logger.info(f"VM {vm.vm_id} found for test: {scenario.name}")

        # Mark VM as busy
        self.vm_manager.mark_vm_busy(vm)

        test_script_path = None
        try:
            scenario_sh_path = os.path.join(TESTDIR, "bin", "scenario.sh")
            if not os.path.exists(scenario_sh_path):
                raise FileNotFoundError(
                    f"scenario.sh not found at {scenario_sh_path}"
                )

            # Generate a new test script with all required functions
            if not scenario.suites:
                logger.warning(f"Scenario {scenario.name} has no test suites defined")
                test_passed = False
                return test_passed

            test_script_path = self.generate_test_script(scenario)

            logger.info(
                f"Calling scenario.sh run with script: {test_script_path} "
                f"and RUN_HOST_OVERRIDE={vm.ip_address}"
            )
            result = subprocess.run(
                [scenario_sh_path, "run", test_script_path, vm.ip_address],
                capture_output=True,
                text=True,
                check=False,
                # stdout=None,  # Stream to sys.stdout
                # stderr=None   # Stream to sys.stderr
            )
            if result.returncode != 0:
                error_msg = (
                    f"Test execution failed for scenario {scenario.name}. "
                    f"Return code: {result.returncode}\n"
                    f"STDOUT: {result.stdout}\n"
                    f"STDERR: {result.stderr}"
                )
                logger.error(error_msg)
                test_passed = False
            else:
                logger.info(f"Test {scenario.name} completed successfully")
                test_passed = True

            logger.info(f"Test {scenario.name} completed: {'PASSED' if test_passed else 'FAILED'}")
            return test_passed

        except Exception as e:
            logger.error(f"Test {scenario.name} failed with error: {e}")
            return False

        finally:
            # Clean up the test script file
            if test_script_path and os.path.exists(test_script_path):
                try:
                    os.remove(test_script_path)
                    logger.debug(f"Cleaned up test script: {test_script_path}")
                except Exception as e:
                    logger.warning(f"Failed to clean up test script {test_script_path}: {e}")

            # Handle VM cleanup
            if scenario.is_upgrade:
                logger.info(f"Test {scenario.name} is an upgrade test, destroying VM")
                self.vm_manager.destroy_vm(vm)
            else:
                logger.info(f"Test {scenario.name} is not an upgrade test, keeping VM")
                self.vm_manager.mark_vm_available(vm)

    def run_scenario_worker(self, scenario: Scenario):
        """Worker thread to run a single scenario."""
        try:
            result = self.run_test(scenario)
            with self.results_lock:
                self.test_results[scenario.name] = result
        except Exception as e:
            logger.error(f"Error running scenario {scenario.name}: {e}")
            with self.results_lock:
                self.test_results[scenario.name] = False

    def run(self):
        """Run all scenarios."""
        # Load configurations
        self.scenarios = self.load_scenarios()
        self.launchers = self.load_launchers()

        if not self.scenarios:
            logger.warning("No scenarios to run")
            return

        if not self.launchers:
            logger.error("No launchers available")
            return

        logger.info(f"Starting test run with {len(self.scenarios)} scenarios")

        # Run scenarios (can be parallelized in the future)
        threads = []
        for scenario in self.scenarios:
            thread = threading.Thread(
                target=self.run_scenario_worker,
                args=(scenario,),
                name=f"test-{scenario.name}"
            )
            thread.start()
            threads.append(thread)

        # Wait for all tests to complete
        for thread in threads:
            thread.join()

        # Print summary
        logger.info("=" * 60)
        logger.info("Test Run Summary")
        logger.info("=" * 60)
        passed = sum(1 for result in self.test_results.values() if result)
        failed = len(self.test_results) - passed
        logger.info(f"Total: {len(self.test_results)}")
        logger.info(f"Passed: {passed}")
        logger.info(f"Failed: {failed}")

        for scenario_name, result in self.test_results.items():
            status = "PASSED" if result else "FAILED"
            logger.info(f"  {scenario_name}: {status}")

        # Cleanup any remaining VMs
        logger.info("Cleaning up remaining VMs...")
        with self.vm_manager.vm_lock:
            vms_to_destroy = [
                vm for vm in self.vm_manager.vms.values()
                if vm.status != VMStatus.DESTROYED
            ]
        for vm in vms_to_destroy:
            self.vm_manager.destroy_vm(vm)

        logger.info("Test run completed")


def get_total_memory_mb() -> int:
    """
    Get total memory in MB from /proc/meminfo.

    Returns:
        Total memory in MB, or 0 if unable to determine
    """
    try:
        with open('/proc/meminfo', 'r') as f:
            meminfo = f.read()

        # Parse MemTotal
        for line in meminfo.split('\n'):
            if line.startswith('MemTotal:'):
                parts = line.split()
                if len(parts) >= 2:
                    # Values in /proc/meminfo are in KB, convert to MB
                    mem_total_kb = int(parts[1])
                    return mem_total_kb // 1024

        logger.warning("Could not find MemTotal in /proc/meminfo")
        return 0
    except FileNotFoundError:
        logger.warning("/proc/meminfo not found, cannot determine total memory")
        return 0
    except Exception as e:
        logger.warning(f"Error reading memory information: {e}")
        return 0


def main():
    """Main entry point."""
    parser = argparse.ArgumentParser(description='MicroShift test runner')
    parser.add_argument(
        '--scenarios',
        default='scenarios.yaml',
        help='Path to scenarios configuration file (default: scenarios.yaml)'
    )
    parser.add_argument(
        '--launchers',
        default='launchers.yaml',
        help='Path to launchers configuration file (default: launchers.yaml)'
    )
    parser.add_argument(
        '--reserved-cpus',
        type=int,
        default=4,
        help='Total number of CPUs reserved for the host (default: 4)'
    )
    parser.add_argument(
        '--reserved-memory',
        type=int,
        default=8192,
        help='Total memory reserved for the host (default: 8192 MB)'
    )
    parser.add_argument(
        '--verbose', '-v',
        action='store_true',
        help='Enable verbose logging'
    )

    args = parser.parse_args()

    if args.verbose:
        logging.getLogger().setLevel(logging.DEBUG)

    # Determine system resources
    cpu_count = os.cpu_count() or 1
    total_memory_mb = get_total_memory_mb()

    runner = TestRunner(
        scenarios_file=os.path.join(TESTDIR, 'runner', args.scenarios),
        launchers_file=os.path.join(TESTDIR, 'runner', args.launchers),
        total_cpus=cpu_count-args.reserved_cpus,
        total_memory=total_memory_mb-args.reserved_memory
    )
    #TODO all strings should be replaced with os.path.expandvars to handle env vars from common.sh. so this should be in its shell script to launch.
    runner.run()


if __name__ == '__main__':
    main()
