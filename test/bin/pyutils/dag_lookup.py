#!/usr/bin/env python3
"""
DAG lookup tool for MicroShift test images.

Given an image name, displays its parent(s) and children in the dependency graph.
Can also display the full DAG or filter by layer.

Usage:
    dag_lookup.py <image_name>           # Show parent and children
    dag_lookup.py --all                  # Show full DAG
    dag_lookup.py --layer layer1-base    # Show DAG filtered by layer
    dag_lookup.py --ancestors <name>     # Show all ancestors (recursive)
    dag_lookup.py --descendants <name>   # Show all descendants (recursive)
"""

import argparse
import os
import re
import sys
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Dict, List, Optional, Set

SCRIPTDIR = os.path.dirname(os.path.abspath(__file__))
TESTDIR = os.path.abspath(os.path.join(SCRIPTDIR, "..", ".."))  # pyutils -> bin -> test


class BuildType(Enum):
    """Type of build artifact."""
    OSTREE = "ostree"
    CONTAINERFILE = "containerfile"
    IMAGE_BOOTC = "image-bootc"
    ALIAS = "alias"
    IMAGE_INSTALLER = "image-installer"


@dataclass
class BuildNode:
    """Represents a single build task in the DAG."""
    name: str
    file_path: str
    build_type: BuildType
    layer: str
    parent: Optional[str] = None


def _get_blueprint_name(blueprint_path: str) -> Optional[str]:
    """Extract the 'name' field from a TOML blueprint file."""
    try:
        with open(blueprint_path, 'r') as f:
            for line in f:
                match = re.match(r'^name\s*=\s*["\'](.+)["\']', line)
                if match:
                    return match.group(1)
    except Exception:
        pass
    return None


def _extract_ostree_parent(blueprint_path: str, blueprint_dir: str) -> Optional[str]:
    """Extract parent from OSTree blueprint (.toml file)."""
    try:
        with open(blueprint_path, 'r') as f:
            for line in f:
                match = re.match(r'^#\s*parent\s*=\s*(.+)', line)
                if match:
                    parent = match.group(1).strip().strip('"\'')
                    # Skip if still contains template syntax
                    if '{{' in parent:
                        return f"(template: {parent})"
                    return parent
    except Exception:
        pass

    # Derive from filename prefix
    base = Path(blueprint_path).stem
    if '-' in base:
        prefix = base.split('-')[0]
        for toml_file in Path(blueprint_dir).rglob(f"{prefix}.toml"):
            name = _get_blueprint_name(str(toml_file))
            if name:
                return name
    return None


def _extract_containerfile_parent(containerfile_path: str) -> Optional[str]:
    """Extract parent from Containerfile (FROM localhost/...)."""
    try:
        with open(containerfile_path, 'r') as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith('#'):
                    continue
                match = re.match(r'^FROM\s+localhost/([^:\s]+)', line, re.IGNORECASE)
                if match:
                    return match.group(1)
                if line.upper().startswith('FROM'):
                    return None
    except Exception:
        pass
    return None


def _extract_image_bootc_parent(image_bootc_path: str) -> Optional[str]:
    """Extract parent from .image-bootc file."""
    try:
        with open(image_bootc_path, 'r') as f:
            content = f.read()
        match = re.search(r'localhost/([^:\s]+)', content)
        if match:
            return match.group(1)
    except Exception:
        pass
    return None


def _extract_alias_target(alias_path: str) -> Optional[str]:
    """Extract target from .alias file."""
    try:
        with open(alias_path, 'r') as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith('#'):
                    return line
    except Exception:
        pass
    return None


class DAGLookup:
    """Provides lookup functionality for image dependency DAG."""

    def __init__(self):
        self.nodes: Dict[str, BuildNode] = {}
        self.children_map: Dict[str, List[str]] = {}  # parent -> [children]

    def discover_all(self) -> None:
        """Discover all blueprints from both ostree and bootc directories."""
        # Discover ostree blueprints
        ostree_dir = os.path.join(TESTDIR, "image-blueprints")
        self._discover_ostree(ostree_dir)

        # Discover bootc blueprints
        bootc_dir = os.path.join(TESTDIR, "image-blueprints-bootc")
        self._discover_bootc(bootc_dir)

        # Build children map
        self._build_children_map()

    def _discover_ostree(self, base_dir: str) -> None:
        """Discover OSTree blueprints (.toml files)."""
        if not os.path.isdir(base_dir):
            return

        for layer in self._find_layers(base_dir):
            layer_path = os.path.join(base_dir, layer)

            # Find .toml files (both flat and grouped)
            for toml_file in list(Path(layer_path).glob("*.toml")) + list(Path(layer_path).glob("*/*.toml")):
                name = _get_blueprint_name(str(toml_file))
                if not name:
                    name = toml_file.stem

                parent = _extract_ostree_parent(str(toml_file), base_dir)

                node = BuildNode(
                    name=name,
                    file_path=str(toml_file),
                    build_type=BuildType.OSTREE,
                    layer=layer,
                    parent=parent
                )
                if name not in self.nodes:
                    self.nodes[name] = node

            # Find .alias files
            for alias_file in list(Path(layer_path).glob("*.alias")) + list(Path(layer_path).glob("*/*.alias")):
                name = alias_file.stem
                target = _extract_alias_target(str(alias_file))

                node = BuildNode(
                    name=name,
                    file_path=str(alias_file),
                    build_type=BuildType.ALIAS,
                    layer=layer,
                    parent=target
                )
                if name not in self.nodes:
                    self.nodes[name] = node

            # Find .image-installer files
            for installer_file in list(Path(layer_path).glob("*.image-installer")) + list(Path(layer_path).glob("*/*.image-installer")):
                name = installer_file.stem

                node = BuildNode(
                    name=f"{name}-installer",
                    file_path=str(installer_file),
                    build_type=BuildType.IMAGE_INSTALLER,
                    layer=layer,
                    parent=None  # Installers depend on edge-commit, but that's implicit
                )
                if name not in self.nodes:
                    self.nodes[f"{name}-installer"] = node

    def _discover_bootc(self, base_dir: str) -> None:
        """Discover bootc blueprints (.containerfile, .image-bootc)."""
        if not os.path.isdir(base_dir):
            return

        for layer in self._find_layers(base_dir):
            layer_path = os.path.join(base_dir, layer)

            # Find .containerfile files
            for cf_file in list(Path(layer_path).glob("*.containerfile")) + list(Path(layer_path).glob("*/*.containerfile")):
                name = cf_file.stem
                parent = _extract_containerfile_parent(str(cf_file))

                node = BuildNode(
                    name=name,
                    file_path=str(cf_file),
                    build_type=BuildType.CONTAINERFILE,
                    layer=layer,
                    parent=parent
                )
                if name not in self.nodes:
                    self.nodes[name] = node

            # Find .image-bootc files
            for bootc_file in list(Path(layer_path).glob("*.image-bootc")) + list(Path(layer_path).glob("*/*.image-bootc")):
                name = bootc_file.stem
                parent = _extract_image_bootc_parent(str(bootc_file))

                node = BuildNode(
                    name=name,
                    file_path=str(bootc_file),
                    build_type=BuildType.IMAGE_BOOTC,
                    layer=layer,
                    parent=parent
                )
                if name not in self.nodes:
                    self.nodes[name] = node

    def _find_layers(self, base_dir: str) -> List[str]:
        """Find all layer directories."""
        layers = []
        if os.path.isdir(base_dir):
            for entry in os.listdir(base_dir):
                if entry.startswith("layer") and os.path.isdir(os.path.join(base_dir, entry)):
                    layers.append(entry)
        return sorted(layers)

    def _build_children_map(self) -> None:
        """Build reverse mapping from parent to children."""
        self.children_map = {}
        for node in self.nodes.values():
            if node.parent:
                if node.parent not in self.children_map:
                    self.children_map[node.parent] = []
                self.children_map[node.parent].append(node.name)

    def get_node(self, name: str) -> Optional[BuildNode]:
        """Get a node by name."""
        return self.nodes.get(name)

    def get_children(self, name: str) -> List[str]:
        """Get direct children of a node."""
        return self.children_map.get(name, [])

    def get_parent(self, name: str) -> Optional[str]:
        """Get parent of a node."""
        node = self.nodes.get(name)
        return node.parent if node else None

    def get_ancestors(self, name: str) -> List[str]:
        """Get all ancestors (recursive) of a node."""
        ancestors = []
        current = name
        visited = set()

        while current:
            node = self.nodes.get(current)
            if not node or not node.parent:
                break
            if node.parent in visited:
                break  # Cycle detection
            visited.add(node.parent)
            ancestors.append(node.parent)
            current = node.parent

        return ancestors

    def get_descendants(self, name: str) -> List[str]:
        """Get all descendants (recursive) of a node."""
        descendants = []
        queue = self.get_children(name)
        visited = set()

        while queue:
            child = queue.pop(0)
            if child in visited:
                continue
            visited.add(child)
            descendants.append(child)
            queue.extend(self.get_children(child))

        return descendants

    def get_roots(self) -> List[str]:
        """Get all root nodes (no parent)."""
        return [name for name, node in self.nodes.items() if not node.parent]

    def get_leaves(self) -> List[str]:
        """Get all leaf nodes (no children)."""
        return [name for name in self.nodes.keys() if name not in self.children_map]

    def filter_by_layer(self, layer: str) -> List[BuildNode]:
        """Get all nodes in a specific layer."""
        return [node for node in self.nodes.values() if node.layer == layer]

    def search(self, pattern: str) -> List[str]:
        """Search for nodes matching a pattern (case-insensitive substring)."""
        pattern_lower = pattern.lower()
        return [name for name in self.nodes.keys() if pattern_lower in name.lower()]


def format_node(node: BuildNode, indent: int = 0) -> str:
    """Format a node for display."""
    prefix = "  " * indent
    type_str = node.build_type.value if node.build_type else "unknown"
    return f"{prefix}{node.name} [{node.layer}] ({type_str})"


def print_tree(lookup: DAGLookup, name: str, indent: int = 0, visited: Set[str] = None) -> None:
    """Print a tree starting from a node."""
    if visited is None:
        visited = set()

    if name in visited:
        print(f"{'  ' * indent}{name} (circular reference)")
        return

    visited.add(name)
    node = lookup.get_node(name)
    if node:
        print(format_node(node, indent))
        for child in sorted(lookup.get_children(name)):
            print_tree(lookup, child, indent + 1, visited.copy())


def print_lookup_result(lookup: DAGLookup, name: str) -> None:
    """Print parent and children for a specific node."""
    node = lookup.get_node(name)

    if not node:
        # Try fuzzy search
        matches = lookup.search(name)
        if matches:
            print(f"Node '{name}' not found. Did you mean one of these?")
            for match in sorted(matches)[:10]:
                print(f"  - {match}")
        else:
            print(f"Node '{name}' not found.")
        return

    print(f"\n{'=' * 60}")
    print(f"Image: {node.name}")
    print(f"{'=' * 60}")
    print(f"  Layer:      {node.layer}")
    print(f"  Type:       {node.build_type.value}")
    print(f"  File:       {node.file_path}")

    # Parent
    print(f"\n  Parent:")
    if node.parent:
        parent_node = lookup.get_node(node.parent)
        if parent_node:
            print(f"    -> {format_node(parent_node)}")
        else:
            print(f"    -> {node.parent} (external/cached)")
    else:
        print(f"    (none - this is a root node)")

    # Direct children
    children = lookup.get_children(name)
    print(f"\n  Children ({len(children)}):")
    if children:
        for child in sorted(children):
            child_node = lookup.get_node(child)
            if child_node:
                print(f"    <- {format_node(child_node)}")
    else:
        print(f"    (none - this is a leaf node)")

    # Ancestors
    ancestors = lookup.get_ancestors(name)
    print(f"\n  All Ancestors ({len(ancestors)}):")
    if ancestors:
        for i, anc in enumerate(ancestors):
            print(f"    {'  ' * i}-> {anc}")
    else:
        print(f"    (none)")

    # Descendants
    descendants = lookup.get_descendants(name)
    print(f"\n  All Descendants ({len(descendants)}):")
    if descendants:
        for desc in sorted(descendants):
            desc_node = lookup.get_node(desc)
            layer = desc_node.layer if desc_node else "?"
            print(f"    <- {desc} [{layer}]")
    else:
        print(f"    (none)")

    print()


def print_full_dag(lookup: DAGLookup, filter_layer: Optional[str] = None) -> None:
    """Print the full DAG structure."""
    print(f"\n{'=' * 60}")
    print("Full DAG Structure")
    if filter_layer:
        print(f"(filtered to layer: {filter_layer})")
    print(f"{'=' * 60}\n")

    # Group by layer
    by_layer: Dict[str, List[BuildNode]] = {}
    for node in lookup.nodes.values():
        if filter_layer and node.layer != filter_layer:
            continue
        if node.layer not in by_layer:
            by_layer[node.layer] = []
        by_layer[node.layer].append(node)

    for layer in sorted(by_layer.keys()):
        print(f"\n{layer}:")
        print("-" * 40)
        nodes = sorted(by_layer[layer], key=lambda n: n.name)
        for node in nodes:
            children = lookup.get_children(node.name)
            parent_str = f"parent: {node.parent}" if node.parent else "root"
            children_str = f"children: {len(children)}" if children else "leaf"
            print(f"  {node.name}")
            print(f"      ({parent_str}, {children_str})")

    # Summary
    print(f"\n{'=' * 60}")
    print("Summary")
    print(f"{'=' * 60}")
    print(f"  Total nodes:  {len(lookup.nodes)}")
    print(f"  Root nodes:   {len(lookup.get_roots())}")
    print(f"  Leaf nodes:   {len(lookup.get_leaves())}")
    print(f"  Layers:       {len(by_layer)}")
    print()


def print_dependency_chain(lookup: DAGLookup, name: str, direction: str) -> None:
    """Print ancestor or descendant chain."""
    node = lookup.get_node(name)
    if not node:
        matches = lookup.search(name)
        if matches:
            print(f"Node '{name}' not found. Did you mean: {', '.join(matches[:5])}?")
        else:
            print(f"Node '{name}' not found.")
        return

    if direction == "ancestors":
        chain = lookup.get_ancestors(name)
        title = "Ancestors"
        arrow = "->"
    else:
        chain = lookup.get_descendants(name)
        title = "Descendants"
        arrow = "<-"

    print(f"\n{title} of {name}:")
    print("-" * 40)
    if chain:
        for item in chain:
            item_node = lookup.get_node(item)
            if item_node:
                print(f"  {arrow} {format_node(item_node)}")
            else:
                print(f"  {arrow} {item} (external)")
    else:
        print(f"  (none)")
    print()


def main():
    parser = argparse.ArgumentParser(
        description="Lookup image dependencies in the MicroShift test DAG.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  %(prog)s rhel-9.6-microshift-source      # Show info for specific image
  %(prog)s --all                           # Show full DAG
  %(prog)s --layer layer1-base             # Show only layer1-base images
  %(prog)s --ancestors rhel-9.6-microshift-source
  %(prog)s --descendants rhel-9.6
  %(prog)s --search source                 # Search for images containing 'source'
  %(prog)s --roots                         # Show all root nodes
  %(prog)s --leaves                        # Show all leaf nodes
        """
    )

    parser.add_argument(
        "image",
        nargs="?",
        help="Image name to lookup"
    )
    parser.add_argument(
        "--all", "-a",
        action="store_true",
        help="Show full DAG structure"
    )
    parser.add_argument(
        "--layer", "-l",
        help="Filter by layer name"
    )
    parser.add_argument(
        "--ancestors",
        metavar="IMAGE",
        help="Show all ancestors of an image"
    )
    parser.add_argument(
        "--descendants",
        metavar="IMAGE",
        help="Show all descendants of an image"
    )
    parser.add_argument(
        "--search", "-s",
        metavar="PATTERN",
        help="Search for images matching pattern"
    )
    parser.add_argument(
        "--roots",
        action="store_true",
        help="Show all root nodes (no parent)"
    )
    parser.add_argument(
        "--leaves",
        action="store_true",
        help="Show all leaf nodes (no children)"
    )
    parser.add_argument(
        "--tree", "-t",
        metavar="IMAGE",
        help="Show tree starting from an image"
    )

    args = parser.parse_args()

    # Initialize lookup
    lookup = DAGLookup()
    lookup.discover_all()

    if not lookup.nodes:
        print("No blueprints found. Make sure you're running from the test directory.")
        sys.exit(1)

    # Handle different modes
    if args.all or args.layer:
        print_full_dag(lookup, args.layer)
    elif args.ancestors:
        print_dependency_chain(lookup, args.ancestors, "ancestors")
    elif args.descendants:
        print_dependency_chain(lookup, args.descendants, "descendants")
    elif args.search:
        matches = lookup.search(args.search)
        print(f"\nImages matching '{args.search}':")
        print("-" * 40)
        for name in sorted(matches):
            node = lookup.get_node(name)
            print(f"  {format_node(node)}")
        print(f"\nTotal: {len(matches)} matches\n")
    elif args.roots:
        roots = lookup.get_roots()
        print(f"\nRoot nodes ({len(roots)}):")
        print("-" * 40)
        for name in sorted(roots):
            node = lookup.get_node(name)
            children_count = len(lookup.get_children(name))
            print(f"  {name} [{node.layer}] -> {children_count} children")
        print()
    elif args.leaves:
        leaves = lookup.get_leaves()
        print(f"\nLeaf nodes ({len(leaves)}):")
        print("-" * 40)
        for name in sorted(leaves):
            node = lookup.get_node(name)
            print(f"  {name} [{node.layer}]")
        print()
    elif args.tree:
        print(f"\nTree from {args.tree}:")
        print("-" * 40)
        print_tree(lookup, args.tree)
        print()
    elif args.image:
        print_lookup_result(lookup, args.image)
    else:
        parser.print_help()
        sys.exit(1)


if __name__ == "__main__":
    main()
