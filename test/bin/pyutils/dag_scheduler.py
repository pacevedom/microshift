#!/usr/bin/env python3
"""
DAG-based build scheduler for MicroShift test images.

This module provides a dependency-graph-based scheduler that starts builds
as soon as their parent dependencies complete, maximizing parallelism.
"""

import os
import re
import threading
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Callable, Dict, List, Optional, Set

import common


class BuildStatus(Enum):
    """Status of a build node in the DAG."""
    PENDING = "pending"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"
    SKIPPED = "skipped"
    BLOCKED = "blocked"  # Parent failed, cannot build


class BuildType(Enum):
    """Type of build artifact."""
    OSTREE = "ostree"
    CONTAINERFILE = "containerfile"
    IMAGE_BOOTC = "image-bootc"
    CONTAINER_ENCAPSULATE = "container-encapsulate"


@dataclass
class BuildNode:
    """Represents a single build task in the DAG."""
    name: str                           # Unique identifier (blueprint/image name)
    file_path: str                      # Path to the blueprint/containerfile
    build_type: BuildType               # Type of build
    layer: str                          # Layer name for CI filtering
    parent: Optional[str] = None        # Parent node name (None for root nodes)
    status: BuildStatus = BuildStatus.PENDING
    build_id: Optional[str] = None      # Build ID once submitted (for OSTree)
    build_command: Optional[str] = None # Command to restart on failure
    error_message: Optional[str] = None # Error message if failed


@dataclass
class DAGScheduler:
    """
    Manages build execution order based on dependency graph.

    Thread-safe for concurrent access from multiple workers.
    """
    nodes: Dict[str, BuildNode] = field(default_factory=dict)
    _lock: threading.Lock = field(default_factory=threading.Lock)

    def add_node(self, node: BuildNode) -> None:
        """Add a build node to the DAG."""
        with self._lock:
            if node.name in self.nodes:
                raise ValueError(f"Node '{node.name}' already exists in DAG")
            self.nodes[node.name] = node

    def get_node(self, name: str) -> Optional[BuildNode]:
        """Get a node by name."""
        with self._lock:
            return self.nodes.get(name)

    def get_ready_nodes(self) -> List[BuildNode]:
        """
        Return nodes that are ready to build.

        A node is ready if:
        - Status is PENDING
        - Parent is None, or parent status is COMPLETED or SKIPPED
        """
        with self._lock:
            ready = []
            for node in self.nodes.values():
                if node.status != BuildStatus.PENDING:
                    continue
                if node.parent is None:
                    ready.append(node)
                elif node.parent in self.nodes:
                    parent = self.nodes[node.parent]
                    if parent.status in (BuildStatus.COMPLETED, BuildStatus.SKIPPED):
                        ready.append(node)
                else:
                    # Parent not in graph - assume it's already built (cached)
                    ready.append(node)
            return ready

    def mark_running(self, name: str, build_id: Optional[str] = None,
                     build_command: Optional[str] = None) -> None:
        """Mark a node as currently building."""
        with self._lock:
            if name not in self.nodes:
                raise ValueError(f"Node '{name}' not found in DAG")
            node = self.nodes[name]
            node.status = BuildStatus.RUNNING
            node.build_id = build_id
            node.build_command = build_command

    def mark_completed(self, name: str, success: bool,
                       error_message: Optional[str] = None) -> List[BuildNode]:
        """
        Mark a node as completed (or failed) and return newly ready children.

        If a node fails, all its descendants are marked as BLOCKED.
        """
        with self._lock:
            if name not in self.nodes:
                raise ValueError(f"Node '{name}' not found in DAG")

            node = self.nodes[name]
            if success:
                node.status = BuildStatus.COMPLETED
            else:
                node.status = BuildStatus.FAILED
                node.error_message = error_message
                # Block all descendants
                self._block_descendants(name)

            # Find newly ready children
            return self._get_ready_children(name)

    def mark_skipped(self, name: str) -> List[BuildNode]:
        """Mark a node as skipped (already cached) and return newly ready children."""
        with self._lock:
            if name not in self.nodes:
                raise ValueError(f"Node '{name}' not found in DAG")
            self.nodes[name].status = BuildStatus.SKIPPED
            return self._get_ready_children(name)

    def _get_ready_children(self, parent_name: str) -> List[BuildNode]:
        """Get children of the given parent that are now ready to build."""
        # Must be called with lock held
        ready = []
        for node in self.nodes.values():
            if node.parent == parent_name and node.status == BuildStatus.PENDING:
                ready.append(node)
        return ready

    def _block_descendants(self, failed_name: str) -> None:
        """Recursively block all descendants of a failed node."""
        # Must be called with lock held
        for node in self.nodes.values():
            if node.parent == failed_name and node.status == BuildStatus.PENDING:
                node.status = BuildStatus.BLOCKED
                node.error_message = f"Parent '{failed_name}' failed"
                self._block_descendants(node.name)

    def has_pending(self) -> bool:
        """Check if there are pending or running builds."""
        with self._lock:
            for node in self.nodes.values():
                if node.status in (BuildStatus.PENDING, BuildStatus.RUNNING):
                    return True
            return False

    def get_running_nodes(self) -> List[BuildNode]:
        """Get all currently running nodes."""
        with self._lock:
            return [n for n in self.nodes.values() if n.status == BuildStatus.RUNNING]

    def get_failed_nodes(self) -> List[BuildNode]:
        """Get all failed nodes."""
        with self._lock:
            return [n for n in self.nodes.values() if n.status == BuildStatus.FAILED]

    def get_blocked_nodes(self) -> List[BuildNode]:
        """Get all blocked nodes."""
        with self._lock:
            return [n for n in self.nodes.values() if n.status == BuildStatus.BLOCKED]

    def get_summary(self) -> Dict[str, int]:
        """Get count of nodes in each status."""
        with self._lock:
            summary = {status.value: 0 for status in BuildStatus}
            for node in self.nodes.values():
                summary[node.status.value] += 1
            return summary

    def validate(self) -> List[str]:
        """
        Validate the DAG for issues.

        Returns a list of error messages (empty if valid).
        """
        errors = []
        with self._lock:
            # Check for missing parents
            for node in self.nodes.values():
                if node.parent and node.parent not in self.nodes:
                    errors.append(
                        f"Node '{node.name}' references unknown parent '{node.parent}'"
                    )

            # Check for cycles (simple DFS)
            visited = set()
            rec_stack = set()

            def has_cycle(name: str) -> bool:
                visited.add(name)
                rec_stack.add(name)

                node = self.nodes.get(name)
                if node and node.parent:
                    if node.parent not in visited:
                        if node.parent in self.nodes and has_cycle(node.parent):
                            return True
                    elif node.parent in rec_stack:
                        return True

                rec_stack.remove(name)
                return False

            for name in self.nodes:
                if name not in visited:
                    if has_cycle(name):
                        errors.append(f"Cycle detected involving node '{name}'")

        return errors


# =============================================================================
# Dependency Extraction Functions
# =============================================================================

def extract_ostree_parent(blueprint_path: str, blueprint_dir: str) -> Optional[str]:
    """
    Extract parent from OSTree blueprint (.toml file).

    1. Check for explicit '# parent = ...' directive
    2. Fall back to filename prefix matching (rhel96-source -> rhel96)
    """
    # Check for explicit parent directive
    with open(blueprint_path, 'r') as f:
        for line in f:
            match = re.match(r'^#\s*parent\s*=\s*(.+)', line)
            if match:
                return match.group(1).strip().strip('"\'')

    # Derive from filename prefix
    base = Path(blueprint_path).stem
    if '-' in base:
        prefix = base.split('-')[0]
        # Search for a blueprint with this prefix as name
        for toml_file in Path(blueprint_dir).rglob(f"{prefix}.toml"):
            # Read the blueprint name from the file
            try:
                name = _get_blueprint_name(str(toml_file))
                if name:
                    return name
            except Exception:
                pass

    return None


def _get_blueprint_name(blueprint_path: str) -> Optional[str]:
    """Extract the 'name' field from a TOML blueprint file."""
    with open(blueprint_path, 'r') as f:
        for line in f:
            match = re.match(r'^name\s*=\s*["\'](.+)["\']', line)
            if match:
                return match.group(1)
    return None


def extract_containerfile_parent(containerfile_path: str) -> Optional[str]:
    """
    Extract parent from Containerfile.

    Parses 'FROM localhost/...' line to get local dependency.
    Returns None if FROM references external registry.
    """
    with open(containerfile_path, 'r') as f:
        for line in f:
            line = line.strip()
            # Skip empty lines and comments
            if not line or line.startswith('#'):
                continue
            # Check for FROM directive
            match = re.match(r'^FROM\s+localhost/([^:]+)', line, re.IGNORECASE)
            if match:
                return match.group(1)
            # If first non-comment line is FROM but not localhost, no local dep
            if line.upper().startswith('FROM'):
                return None
    return None


def extract_image_bootc_parent(image_bootc_path: str) -> Optional[str]:
    """
    Extract parent from .image-bootc file.

    Check if content contains 'localhost/...' reference.
    The file may contain template conditionals, so search anywhere in content.
    """
    content = common.read_file_valid_lines(image_bootc_path)
    # Search for localhost/ anywhere in the content (may be wrapped in template conditionals)
    match = re.search(r'localhost/([^:\s]+)', content)
    if match:
        return match.group(1)
    return None


def extract_container_encapsulate_parent(encapsulate_path: str) -> Optional[str]:
    """
    Extract parent from .container-encapsulate file.

    These files reference ostree commits, not container images,
    so they typically don't have local container dependencies.
    """
    # Container encapsulate files reference ostree refs, not containers
    # They don't have local container dependencies in the traditional sense
    return None


def get_build_type_from_path(file_path: str) -> Optional[BuildType]:
    """Determine build type from file extension."""
    if file_path.endswith('.toml'):
        return BuildType.OSTREE
    elif file_path.endswith('.containerfile'):
        return BuildType.CONTAINERFILE
    elif file_path.endswith('.image-bootc'):
        return BuildType.IMAGE_BOOTC
    elif file_path.endswith('.container-encapsulate'):
        return BuildType.CONTAINER_ENCAPSULATE
    return None


def extract_parent(file_path: str, blueprint_dir: str = "") -> Optional[str]:
    """Extract parent dependency based on file type."""
    build_type = get_build_type_from_path(file_path)
    if build_type == BuildType.OSTREE:
        return extract_ostree_parent(file_path, blueprint_dir)
    elif build_type == BuildType.CONTAINERFILE:
        return extract_containerfile_parent(file_path)
    elif build_type == BuildType.IMAGE_BOOTC:
        return extract_image_bootc_parent(file_path)
    elif build_type == BuildType.CONTAINER_ENCAPSULATE:
        return extract_container_encapsulate_parent(file_path)
    return None


# =============================================================================
# Discovery Functions
# =============================================================================

def discover_bootc_blueprints(base_dir: str, layers: List[str]) -> List[BuildNode]:
    """
    Discover all bootc blueprints in the specified layers.

    Args:
        base_dir: Base directory containing layer directories
        layers: List of layer names to scan (e.g., ['layer1-base', 'layer2-presubmit'])

    Returns:
        List of BuildNode objects
    """
    nodes = []
    seen_names: Set[str] = set()  # Track seen node names to avoid duplicates

    for layer in layers:
        layer_path = os.path.join(base_dir, layer)
        if not os.path.isdir(layer_path):
            common.print_msg(f"Warning: Layer directory '{layer_path}' not found")
            continue

        # Scan for build files (supporting both flat and grouped structures)
        for pattern in ['*.containerfile', '*.image-bootc', '*.container-encapsulate']:
            # First try flat structure (layer/file) - prefer flat over grouped
            for file_path in Path(layer_path).glob(pattern):
                node = _create_bootc_node(str(file_path), layer, base_dir)
                if node:
                    if node.name not in seen_names:
                        nodes.append(node)
                        seen_names.add(node.name)
                    else:
                        common.print_msg(f"Skipping duplicate: {file_path} (already have {node.name})")

            # Then try grouped structure (layer/group*/file) - skip if already found
            for file_path in Path(layer_path).glob(f"*/{pattern}"):
                node = _create_bootc_node(str(file_path), layer, base_dir)
                if node:
                    if node.name not in seen_names:
                        nodes.append(node)
                        seen_names.add(node.name)
                    else:
                        common.print_msg(f"Skipping duplicate: {file_path} (already have {node.name})")

    return nodes


def _create_bootc_node(file_path: str, layer: str, base_dir: str) -> Optional[BuildNode]:
    """Create a BuildNode from a bootc blueprint file."""
    build_type = get_build_type_from_path(file_path)
    if not build_type:
        return None

    # Extract name from filename (without extension)
    name = Path(file_path).stem

    # Extract parent dependency
    parent = extract_parent(file_path, base_dir)

    return BuildNode(
        name=name,
        file_path=file_path,
        build_type=build_type,
        layer=layer,
        parent=parent
    )


def discover_ostree_blueprints(base_dir: str, layers: List[str]) -> List[BuildNode]:
    """
    Discover all OSTree blueprints in the specified layers.

    Args:
        base_dir: Base directory containing layer directories
        layers: List of layer names to scan

    Returns:
        List of BuildNode objects
    """
    nodes = []
    seen_names: Set[str] = set()  # Track seen node names to avoid duplicates

    for layer in layers:
        layer_path = os.path.join(base_dir, layer)
        if not os.path.isdir(layer_path):
            common.print_msg(f"Warning: Layer directory '{layer_path}' not found")
            continue

        # Scan for TOML files (supporting both flat and grouped structures)
        for pattern in ['*.toml']:
            # First try flat structure - prefer flat over grouped
            for file_path in Path(layer_path).glob(pattern):
                node = _create_ostree_node(str(file_path), layer, base_dir)
                if node:
                    if node.name not in seen_names:
                        nodes.append(node)
                        seen_names.add(node.name)
                    else:
                        common.print_msg(f"Skipping duplicate: {file_path} (already have {node.name})")

            # Then try grouped structure - skip if already found
            for file_path in Path(layer_path).glob(f"*/{pattern}"):
                node = _create_ostree_node(str(file_path), layer, base_dir)
                if node:
                    if node.name not in seen_names:
                        nodes.append(node)
                        seen_names.add(node.name)
                    else:
                        common.print_msg(f"Skipping duplicate: {file_path} (already have {node.name})")

    return nodes


def _create_ostree_node(file_path: str, layer: str, base_dir: str) -> Optional[BuildNode]:
    """Create a BuildNode from an OSTree blueprint file."""
    # Extract blueprint name from file content
    name = _get_blueprint_name(file_path)
    if not name:
        # Fall back to filename
        name = Path(file_path).stem

    # Extract parent dependency
    parent = extract_ostree_parent(file_path, base_dir)

    return BuildNode(
        name=name,
        file_path=file_path,
        build_type=BuildType.OSTREE,
        layer=layer,
        parent=parent
    )


def build_dag_from_nodes(nodes: List[BuildNode],
                         existing_images: Optional[Set[str]] = None) -> DAGScheduler:
    """
    Build a DAGScheduler from a list of nodes.

    Args:
        nodes: List of BuildNode objects
        existing_images: Set of image names that already exist (will be marked as SKIPPED)

    Returns:
        Configured DAGScheduler instance
    """
    scheduler = DAGScheduler()
    existing = existing_images or set()

    for node in nodes:
        scheduler.add_node(node)

    # Validate the graph
    errors = scheduler.validate()
    for error in errors:
        common.print_msg(f"DAG Warning: {error}")

    return scheduler


def print_dag(scheduler: DAGScheduler) -> None:
    """Print the DAG structure for debugging."""
    common.print_msg("=== DAG Structure ===")

    # Group by layer
    by_layer: Dict[str, List[BuildNode]] = {}
    for node in scheduler.nodes.values():
        if node.layer not in by_layer:
            by_layer[node.layer] = []
        by_layer[node.layer].append(node)

    for layer in sorted(by_layer.keys()):
        common.print_msg(f"\n{layer}:")
        for node in sorted(by_layer[layer], key=lambda n: n.name):
            parent_info = f" (parent: {node.parent})" if node.parent else " (root)"
            common.print_msg(f"  - {node.name}{parent_info}")

    common.print_msg("\n=== End DAG ===")
