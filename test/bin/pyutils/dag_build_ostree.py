#!/usr/bin/env python3
"""
DAG-based OSTree image build orchestrator.

This script replaces the sequential group-based processing in build_images.sh
with a DAG-based approach that starts builds as soon as their parents complete.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Dict, List, Optional, Set

import common
import dag_scheduler
from dag_scheduler import BuildNode, BuildStatus, BuildType, DAGScheduler


# Environment variables
SCRIPTDIR = os.path.dirname(os.path.abspath(__file__))
TESTDIR = os.path.abspath(os.path.join(SCRIPTDIR, ".."))
IMAGEDIR = common.get_env_var('IMAGEDIR')
LOGDIR = os.path.join(IMAGEDIR, "build-logs")
VM_DISK_BASEDIR = common.get_env_var('VM_DISK_BASEDIR')
GOMPLATE = common.get_env_var('GOMPLATE')
WEB_SERVER_PORT = common.get_env_var('WEB_SERVER_PORT', '8080')


class OstreeBuildOrchestrator:
    """Orchestrates OSTree builds using DAG scheduler."""

    def __init__(self, blueprint_dir: str, layers: List[str],
                 force_rebuild: bool = False, only_source: bool = False,
                 dry_run: bool = False):
        self.blueprint_dir = blueprint_dir
        self.layers = layers
        self.force_rebuild = force_rebuild
        self.only_source = only_source
        self.dry_run = dry_run

        self.scheduler = DAGScheduler()
        self.running_builds: Dict[str, BuildNode] = {}  # build_id -> node
        self.ip_addr = self._get_default_ip()
        self.rendered_dir = os.path.join(IMAGEDIR, "blueprints")

    def _get_default_ip(self) -> str:
        """Get the default IP address."""
        try:
            result = subprocess.run(
                ["hostname", "-I"],
                capture_output=True, text=True, check=True
            )
            return result.stdout.split()[0]
        except Exception:
            return "127.0.0.1"

    def _render_template(self, template_path: str) -> Optional[str]:
        """
        Render a single template file with gomplate.

        Returns path to rendered file, or None if rendering failed/empty.
        """
        output_path = os.path.join(self.rendered_dir, os.path.basename(template_path))

        try:
            result = subprocess.run(
                [GOMPLATE, "--file", template_path],
                capture_output=True, text=True, check=True
            )
            with open(output_path, 'w') as f:
                f.write(result.stdout)

            # Check if template resulted in empty file
            if os.path.getsize(output_path) == 0:
                common.print_msg(f"Warning: Template {template_path} resulted in empty file, skipping")
                os.remove(output_path)
                return None

            return output_path
        except subprocess.CalledProcessError as e:
            common.print_msg(f"Error rendering {template_path}: {e.stderr}")
            return None
        except Exception as e:
            common.print_msg(f"Error rendering {template_path}: {e}")
            return None

    def render_all_templates(self) -> None:
        """
        Render all blueprint templates before discovering dependencies.

        This must happen first because templates contain gomplate expressions
        like {{ .Env.YMINUS2_MINOR_VERSION }} that need to be resolved
        before we can extract parent relationships.
        """
        common.print_msg(f"Rendering all templates from {self.blueprint_dir}")
        os.makedirs(self.rendered_dir, exist_ok=True)

        rendered_count = 0
        skipped_count = 0

        for layer in self.layers:
            layer_path = os.path.join(self.blueprint_dir, layer)
            if not os.path.isdir(layer_path):
                common.print_msg(f"Warning: Layer directory '{layer_path}' not found")
                continue

            # Find all .toml files in the layer (flat structure)
            for toml_file in Path(layer_path).glob("*.toml"):
                result = self._render_template(str(toml_file))
                if result:
                    rendered_count += 1
                else:
                    skipped_count += 1

            # Also check for grouped structure (backwards compatibility)
            for toml_file in Path(layer_path).glob("*/*.toml"):
                result = self._render_template(str(toml_file))
                if result:
                    rendered_count += 1
                else:
                    skipped_count += 1

        common.print_msg(f"Rendered {rendered_count} templates, skipped {skipped_count}")

    def discover_blueprints(self) -> None:
        """
        Discover all OSTree blueprints from RENDERED templates and build the DAG.

        This reads from self.rendered_dir which contains already-expanded templates.
        """
        common.print_msg(f"Discovering blueprints from rendered templates in {self.rendered_dir}")

        # Discover from the rendered directory, not the source templates
        nodes = []
        for toml_file in Path(self.rendered_dir).glob("*.toml"):
            node = self._create_node_from_rendered(str(toml_file))
            if node:
                nodes.append(node)

        if self.only_source:
            # Filter to only source-related blueprints
            nodes = [n for n in nodes if 'source' in n.name.lower()]
            common.print_msg(f"Filtered to {len(nodes)} source blueprints")

        for node in nodes:
            self.scheduler.add_node(node)

        common.print_msg(f"Added {len(nodes)} blueprints to DAG")

        # Validate the DAG
        errors = self.scheduler.validate()
        for error in errors:
            common.print_msg(f"DAG Warning: {error}")

        dag_scheduler.print_dag(self.scheduler)

    def _create_node_from_rendered(self, rendered_path: str) -> Optional[BuildNode]:
        """Create a BuildNode from a rendered (not template) blueprint file."""
        # Extract blueprint name from file content
        name = self._get_blueprint_name(rendered_path)
        if not name:
            # Fall back to filename
            name = Path(rendered_path).stem

        # Extract parent dependency from rendered content
        parent = self._extract_parent_from_rendered(rendered_path)

        # Determine which layer this belongs to (from original path or filename pattern)
        layer = self._determine_layer(name)

        return BuildNode(
            name=name,
            file_path=rendered_path,
            build_type=BuildType.OSTREE,
            layer=layer,
            parent=parent
        )

    def _extract_parent_from_rendered(self, blueprint_path: str) -> Optional[str]:
        """
        Extract parent from a RENDERED blueprint file.

        This reads the already-expanded template, so values like
        {{ .Env.YMINUS2_MINOR_VERSION }} are already resolved.
        """
        with open(blueprint_path, 'r') as f:
            for line in f:
                match = re.match(r'^#\s*parent\s*=\s*(.+)', line)
                if match:
                    parent = match.group(1).strip().strip('"\'')
                    # Verify it doesn't still contain template syntax
                    if '{{' in parent or '}}' in parent:
                        common.print_msg(f"Warning: Parent still contains template syntax: {parent}")
                        return None
                    return parent

        # Fall back to filename prefix matching
        base = Path(blueprint_path).stem
        if '-' in base:
            prefix = base.split('-')[0]
            # Look for a rendered blueprint with this prefix
            for candidate in Path(self.rendered_dir).glob(f"{prefix}.toml"):
                candidate_name = self._get_blueprint_name(str(candidate))
                if candidate_name:
                    return candidate_name

        return None

    def _determine_layer(self, blueprint_name: str) -> str:
        """Determine which layer a blueprint belongs to based on naming patterns."""
        name_lower = blueprint_name.lower()

        if 'source' in name_lower:
            if 'isolated' in name_lower or 'tuned' in name_lower:
                return 'layer3-periodic'
            return 'layer2-presubmit'
        elif 'brew' in name_lower:
            return 'layer4-release'
        else:
            return 'layer1-base'

    def get_rendered_blueprint(self, node: BuildNode) -> Optional[str]:
        """
        Get the path to the already-rendered blueprint.

        Templates are pre-rendered in render_all_templates(), so this just
        returns the path. The node.file_path already points to the rendered file.
        """
        if os.path.exists(node.file_path):
            return node.file_path
        else:
            common.print_msg(f"Error: Rendered blueprint not found: {node.file_path}")
            return None

    def push_blueprint(self, blueprint_path: str, blueprint_name: str) -> bool:
        """Push a blueprint to composer."""
        common.print_msg(f"Pushing blueprint {blueprint_name}")

        if self.dry_run:
            common.print_msg(f"[DRY RUN] Would push {blueprint_name}")
            return True

        try:
            # Delete existing blueprint if present (exact match only)
            list_result = subprocess.run(
                ["sudo", "composer-cli", "blueprints", "list"],
                capture_output=True, text=True, check=True
            )
            # Check for exact match in the list (one blueprint per line)
            existing_blueprints = [line.strip() for line in list_result.stdout.splitlines()]
            if blueprint_name in existing_blueprints:
                common.print_msg(f"Deleting existing blueprint {blueprint_name}")
                subprocess.run(
                    ["sudo", "composer-cli", "blueprints", "delete", blueprint_name],
                    check=True
                )

            # Push new blueprint
            common.print_msg(f"Pushing blueprint from {blueprint_path}")
            subprocess.run(
                ["sudo", "composer-cli", "blueprints", "push", blueprint_path],
                check=True
            )
            return True
        except Exception as e:
            common.print_msg(f"Error pushing blueprint {blueprint_name}: {e}")
            return False

    def depsolve_blueprint(self, blueprint_name: str) -> bool:
        """Resolve dependencies for a blueprint."""
        common.print_msg(f"Resolving dependencies for {blueprint_name}")

        if self.dry_run:
            common.print_msg(f"[DRY RUN] Would depsolve {blueprint_name}")
            return True

        try:
            log_path = os.path.join(LOGDIR, f"{blueprint_name}-depsolve.log")
            with open(log_path, 'w') as log_file:
                subprocess.run(
                    ["sudo", "composer-cli", "blueprints", "depsolve", blueprint_name],
                    stdout=log_file, stderr=log_file, check=True
                )
            return True
        except Exception as e:
            common.print_msg(f"Error resolving dependencies for {blueprint_name}: {e}")
            return False

    def image_exists(self, blueprint_name: str) -> bool:
        """Check if an image already exists in the ostree repo."""
        try:
            result = subprocess.run(
                ["ostree", "summary", "--view", f"--repo={IMAGEDIR}/repo"],
                capture_output=True, text=True
            )
            return f" {blueprint_name}$" in result.stdout or f" {blueprint_name}\n" in result.stdout
        except Exception:
            return False

    def should_skip(self, node: BuildNode) -> bool:
        """Check if a build should be skipped."""
        if self.force_rebuild:
            common.print_msg(f"Forcing rebuild of {node.name}")
            return False

        if self.image_exists(node.name):
            common.print_msg(f"Image {node.name} already exists, skipping")
            return True

        return False

    def submit_build(self, node: BuildNode) -> Optional[str]:
        """
        Submit an OSTree build to composer.

        Returns the build ID on success, None on failure.
        """
        # Get the already-rendered blueprint (templates were pre-rendered)
        blueprint_path = self.get_rendered_blueprint(node)
        if not blueprint_path:
            return None

        # Get the blueprint name from the rendered file
        blueprint_name = self._get_blueprint_name(blueprint_path)
        if not blueprint_name:
            blueprint_name = node.name

        # Check if should skip
        if self.should_skip(node):
            self.scheduler.mark_skipped(node.name)
            return "SKIPPED"

        # Push the blueprint
        if not self.push_blueprint(blueprint_path, blueprint_name):
            return None

        # Depsolve
        if not self.depsolve_blueprint(blueprint_name):
            return None

        # Build parent args
        parent_args = []
        if node.parent:
            parent_args = [
                "--parent", node.parent,
                "--url", f"http://{self.ip_addr}:{WEB_SERVER_PORT}/repo"
            ]

        # Start the build
        build_cmd = [
            "sudo", "composer-cli", "compose", "start-ostree"
        ] + parent_args + [
            "--ref", blueprint_name, blueprint_name, "edge-commit"
        ]

        common.print_msg(f"Starting build: {' '.join(build_cmd)}")

        if self.dry_run:
            common.print_msg(f"[DRY RUN] Would start build for {blueprint_name}")
            return "DRY-RUN-ID"

        # Retry up to 3 times
        for attempt in range(3):
            try:
                result = subprocess.run(
                    build_cmd,
                    capture_output=True, text=True, check=True
                )
                # Extract build ID from output
                match = re.search(r'Compose\s+([a-f0-9-]+)', result.stdout)
                if match:
                    build_id = match.group(1)
                    common.print_msg(f"Build {blueprint_name} started with ID {build_id}")

                    # Record build metadata
                    build_meta_path = os.path.join(IMAGEDIR, "builds", f"{build_id}.build")
                    os.makedirs(os.path.dirname(build_meta_path), exist_ok=True)
                    with open(build_meta_path, 'w') as f:
                        f.write(f"{blueprint_name}-edge-commit")

                    return build_id
            except Exception as e:
                common.print_msg(f"Build attempt {attempt + 1} failed: {e}")
                time.sleep(15)

        common.print_msg(f"Failed to start build for {blueprint_name} after 3 attempts")
        return None

    def _get_blueprint_name(self, blueprint_path: str) -> Optional[str]:
        """Extract the name field from a blueprint file."""
        try:
            with open(blueprint_path, 'r') as f:
                for line in f:
                    match = re.match(r'^name\s*=\s*["\'](.+)["\']', line)
                    if match:
                        return match.group(1)
        except Exception:
            pass
        return None

    def poll_composer_status(self) -> List[dict]:
        """Poll composer-cli for build status."""
        try:
            result = subprocess.run(
                ["sudo", "composer-cli", "compose", "status", "--json"],
                capture_output=True, text=True, check=True
            )
            status = json.loads(result.stdout)

            # Flatten the nested status structure
            jobs = []
            for result_set in status:
                for state in result_set.get("body", {}):
                    for job in result_set["body"].get(state, []):
                        jobs.append(job)
            return jobs
        except Exception as e:
            common.print_msg(f"Error polling composer status: {e}")
            return []

    def download_build_result(self, build_id: str, node: BuildNode) -> bool:
        """Download and process build results."""
        common.print_msg(f"Downloading results for build {build_id}")

        builds_dir = os.path.join(IMAGEDIR, "builds")
        os.chdir(builds_dir)

        try:
            # Download logs
            subprocess.run(
                ["sudo", "composer-cli", "compose", "logs", build_id],
                check=True
            )

            # Fix ownership
            subprocess.run(
                ["sudo", "chown", f"{os.getenv('USER')}.", f"{build_id}-logs.tar"],
                check=True
            )

            # Extract logs
            subprocess.run(
                ["tar", "xf", f"{build_id}-logs.tar"],
                check=True
            )

            # Move log to unique name
            build_name = f"{node.name}-edge-commit"
            log_dest = os.path.join(LOGDIR, f"osbuild-{build_name}-{build_id}.log")
            if os.path.exists("logs/osbuild.log"):
                shutil.move("logs/osbuild.log", log_dest)

            # Download metadata and image
            subprocess.run(
                ["sudo", "composer-cli", "compose", "metadata", build_id],
                check=True
            )
            subprocess.run(
                ["sudo", "composer-cli", "compose", "image", build_id],
                check=True
            )

            # Fix ownership
            for f in Path(builds_dir).glob(f"{build_id}-*"):
                subprocess.run(["sudo", "chown", f"{os.getenv('USER')}.", str(f)])

            # Extract commit
            commit_file = f"{build_id}-commit.tar"
            if os.path.exists(commit_file):
                subprocess.run(
                    ["tar", "-C", IMAGEDIR, "-xf", commit_file],
                    check=True
                )
                common.print_msg(f"Unpacked {commit_file}")

            return True
        except Exception as e:
            common.print_msg(f"Error downloading build {build_id}: {e}")
            return False

    def monitor_loop(self) -> None:
        """
        Main monitoring loop.

        Polls composer-cli status and submits children when parents complete.
        """
        common.print_msg("Entering monitor loop")

        while self.running_builds or self.scheduler.has_pending():
            # Check for ready nodes to submit
            for node in self.scheduler.get_ready_nodes():
                common.print_msg(f"Submitting: {node.name}")
                build_id = self.submit_build(node)

                if build_id == "SKIPPED":
                    # Already handled by submit_build
                    continue
                elif build_id:
                    self.scheduler.mark_running(node.name, build_id)
                    self.running_builds[build_id] = node
                else:
                    self.scheduler.mark_completed(node.name, success=False,
                                                   error_message="Failed to submit build")

            if self.dry_run:
                # In dry run mode, mark all running as completed
                for build_id, node in list(self.running_builds.items()):
                    self.scheduler.mark_completed(node.name, success=True)
                    del self.running_builds[build_id]
                continue

            if not self.running_builds:
                break

            # Poll composer status
            for job in self.poll_composer_status():
                job_id = job.get("id")
                if job_id not in self.running_builds:
                    continue

                node = self.running_builds[job_id]
                status = job.get("queue_status")

                if status == "FINISHED":
                    common.print_msg(f"Build finished: {node.name}")
                    if self.download_build_result(job_id, node):
                        self.scheduler.mark_completed(node.name, success=True)
                    else:
                        self.scheduler.mark_completed(node.name, success=False,
                                                       error_message="Failed to download results")
                    del self.running_builds[job_id]

                elif status == "FAILED":
                    common.print_msg(f"Build failed: {node.name}")
                    self.scheduler.mark_completed(node.name, success=False,
                                                   error_message="Build failed")
                    del self.running_builds[job_id]

            time.sleep(10)

        # Update ostree summary
        if not self.dry_run:
            common.print_msg("Updating ostree summary")
            subprocess.run(
                ["ostree", "summary", "--update", f"--repo={IMAGEDIR}/repo"],
                check=False
            )

    def run(self) -> bool:
        """Main entry point."""
        try:
            # First, render all templates to resolve gomplate expressions
            # This must happen before discovering blueprints because parent
            # relationships may contain template variables like {{ .Env.XXX }}
            self.render_all_templates()

            # Now discover blueprints from the rendered files
            self.discover_blueprints()

            if not self.scheduler.nodes:
                common.print_msg("No blueprints to build")
                return True

            self.monitor_loop()

            # Report results
            summary = self.scheduler.get_summary()
            common.print_msg(f"Build summary: {summary}")

            failed = self.scheduler.get_failed_nodes()
            if failed:
                common.print_msg("Failed builds:")
                for node in failed:
                    common.print_msg(f"  - {node.name}: {node.error_message}")
                return False

            blocked = self.scheduler.get_blocked_nodes()
            if blocked:
                common.print_msg("Blocked builds:")
                for node in blocked:
                    common.print_msg(f"  - {node.name}: {node.error_message}")
                return False

            return True

        except Exception as e:
            common.print_msg(f"Error: {e}")
            return False


def main():
    parser = argparse.ArgumentParser(
        description="Build OSTree images using DAG-based scheduling."
    )
    parser.add_argument(
        "--blueprint-dir", "-b",
        required=True,
        help="Base directory containing layer directories"
    )
    parser.add_argument(
        "--layers", "-l",
        required=True,
        help="Comma-separated list of layer names to process"
    )
    parser.add_argument(
        "--force", "-f",
        action="store_true",
        help="Force rebuilding images that already exist"
    )
    parser.add_argument(
        "--only-source", "-s",
        action="store_true",
        help="Only build source-related images"
    )
    parser.add_argument(
        "--dry-run", "-d",
        action="store_true",
        help="Dry run - don't actually execute builds"
    )

    args = parser.parse_args()

    layers = [l.strip() for l in args.layers.split(",")]

    orchestrator = OstreeBuildOrchestrator(
        blueprint_dir=args.blueprint_dir,
        layers=layers,
        force_rebuild=args.force,
        only_source=args.only_source,
        dry_run=args.dry_run
    )

    success = orchestrator.run()
    sys.exit(0 if success else 1)


if __name__ == "__main__":
    main()
