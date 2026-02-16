# DAG-Based Image Build Scheduler

## Overview

This document describes the implementation of a Directed Acyclic Graph (DAG) based scheduler for building MicroShift test images. The new scheduler replaces the previous sequential group-based approach, enabling builds to start as soon as their parent dependencies complete rather than waiting for entire groups to finish.

## Problem Statement

### Previous Architecture

The original image building system organized builds into `layer*/group*/` directories:

```
image-blueprints-bootc/
  layer1-base/
    group1/
      rhel96-test-agent.containerfile
    group2/
      rhel96-bootc-crel.containerfile
      rhel96-bootc-prel.containerfile
    group3/
      rhel96-bootc-crel-optionals.containerfile
```

**Limitations:**
- All builds within a group ran in parallel
- The system waited for ALL builds in a group to complete before starting the next group
- A fast-completing parent couldn't trigger its child until sibling builds finished
- Artificial bottlenecks when build times varied significantly within a group

### Example of Inefficiency

```
Group 1: [test-agent: 2min] ─────────────────────────────────┐
                                                              │ WAIT
Group 2: [crel: 5min] [prel: 3min] [yminus2: 3min] ──────────┤
                       ↑                                      │
                       └── Could start after test-agent       │
                           but waits for entire group 1       │
                                                              ↓
Group 3: [crel-optionals: 4min] ─────────────────────────────
```

## Solution: DAG-Based Scheduling

### Core Concept

Each image declares its parent dependency (via `FROM localhost/...`). The scheduler builds a dependency graph and starts each build as soon as its parent completes.

```
test-agent (2min)
    │
    ├──→ crel (5min) ──→ crel-optionals (4min)
    │              └──→ crel-isolated (3min)
    │
    ├──→ prel (3min)
    │
    └──→ source (4min) ──→ source-optionals (3min)
                     └──→ source-isolated (5min)
```

**Benefits:**
- `crel` starts immediately when `test-agent` completes (not waiting for group)
- `crel-optionals` and `crel-isolated` start in parallel when `crel` completes
- Maximum parallelism achieved based on actual dependencies

## Implementation Details

### New Files

| File | Purpose |
|------|---------|
| `test/bin/pyutils/dag_scheduler.py` | Core DAG scheduler module |
| `test/bin/pyutils/dag_build_ostree.py` | OSTree build orchestrator using DAG |

### Modified Files

| File | Changes |
|------|---------|
| `test/bin/pyutils/build_bootc_images.py` | Added `process_with_dag()` function |
| `test/bin/build_images.sh` | Integrated DAG scheduler for OSTree builds |
| `test/image-blueprints-bootc/README.md` | Updated documentation |
| `test/image-blueprints/README.md` | Updated documentation |

### Directory Structure Change

Flattened the group directories:

**Before:**
```
layer1-base/
  group1/
    rhel96-test-agent.containerfile
  group2/
    rhel96-bootc-crel.containerfile
```

**After:**
```
layer1-base/
  rhel96-test-agent.containerfile
  rhel96-bootc-crel.containerfile
```

The layer distinction is preserved for CI job filtering (presubmit vs periodic).

### Key Components

#### BuildNode

Represents a single build task:

```python
@dataclass
class BuildNode:
    name: str                    # Unique identifier (image name)
    file_path: str               # Path to containerfile
    build_type: BuildType        # CONTAINERFILE, IMAGE_BOOTC, etc.
    layer: str                   # Layer for CI filtering
    parent: Optional[str]        # Parent node name
    status: BuildStatus          # PENDING, RUNNING, COMPLETED, FAILED
```

#### DAGScheduler

Thread-safe scheduler managing build execution:

```python
class DAGScheduler:
    def add_node(node: BuildNode) -> None
    def get_ready_nodes() -> List[BuildNode]  # Nodes ready to build
    def mark_completed(name: str, success: bool) -> List[BuildNode]  # Returns newly ready children
    def has_pending() -> bool
```

#### Dependency Extraction

Parents are automatically extracted from containerfiles:

```python
def extract_containerfile_parent(path: str) -> Optional[str]:
    # Parses "FROM localhost/rhel96-test-agent:latest"
    # Returns "rhel96-test-agent"
```

### Execution Flow

```python
# 1. Discover all blueprints
nodes = discover_bootc_blueprints(base_dir, layers)

# 2. Build the DAG
scheduler = build_dag_from_nodes(nodes)

# 3. Execute with ThreadPoolExecutor
with ThreadPoolExecutor(max_workers=8) as executor:
    # Submit root nodes (no parent)
    for node in scheduler.get_ready_nodes():
        futures[executor.submit(build, node)] = node

    # Process completions, submit children
    while futures:
        done, _ = wait(futures, return_when=FIRST_COMPLETED)
        for future in done:
            node = futures.pop(future)
            for child in scheduler.mark_completed(node.name, success=True):
                futures[executor.submit(build, child)] = child
```

## Parent Image Optimizations

In addition to the DAG scheduler, we optimized parent-child relationships to avoid redundant work.

### Changes Made

| Image | Before | After | Savings |
|-------|--------|-------|---------|
| `rhel96-bootc-crel-optionals` | FROM test-agent | FROM crel | Skip mandatory RPMs + firewall (~3 min) |
| `rhel100-bootc-crel-optionals` | FROM test-agent | FROM crel | Skip mandatory RPMs + firewall (~3 min) |
| `rhel96-bootc-crel-isolated` | FROM test-agent | FROM crel | Skip mandatory RPMs + firewall (~3 min) |
| `rhel100-bootc-crel-isolated` | FROM test-agent | FROM crel | Skip mandatory RPMs + firewall (~3 min) |

### Optimized Dependency Tree

```
test-agent
├── crel
│   ├── crel-optionals    ← Now inherits from crel (was test-agent)
│   └── crel-isolated     ← Now inherits from crel (was test-agent)
├── source
│   ├── source-optionals  ← Already optimized
│   └── source-isolated   ← Already optimized
└── brew-lrel-optional
    ├── brew-lrel-fips    ← Already optimized
    └── brew-lrel-tuned   ← Already optimized
```

## Performance Impact

### Estimated Improvements

| Scenario | Before | After |
|----------|--------|-------|
| Sequential group waits | Blocked on slowest sibling | Immediate child start |
| Parent image optimization | Reinstall same RPMs | Inherit layers |
| Parallel execution | Group-limited | DAG-limited |

### Example Timeline

```
10:30:24.213564157 === DAG Structure ===
10:30:24.213577985
layer1-base:
10:30:24.213597297   - rhel-9.6 (root)
10:30:24.213604927   - rhel-9.6-microshift-4.20 (parent: rhel-9.6)
10:30:24.213610410   - rhel-9.6-microshift-4.21 (parent: rhel-9.6-microshift-4.20)
10:30:24.213615179   - rhel-9.6-microshift-crel (parent: rhel-9.6-microshift-4.21)
10:30:24.213619947   - rhel-9.6-microshift-crel-optionals (parent: rhel-9.6-microshift-4.21)
10:30:24.213624715
layer2-presubmit:
10:30:24.213632583   - rhel-9.6-microshift-source (parent: rhel-9.6-microshift-crel)
10:30:24.213637113   - rhel-9.6-microshift-source-base (parent: rhel-9.6-microshift-crel)
10:30:24.213641643   - rhel-9.6-microshift-source-fake-next-minor (parent: rhel-9.6-microshift-crel)
10:30:24.213646411   - rhel-9.6-microshift-source-optionals (parent: rhel-9.6-microshift-crel-optionals)
10:30:24.213650941
layer3-periodic:
10:30:24.213657617   - rhel-9.6-microshift-source-isolated (parent: rhel-9.6)
10:30:24.213662624   - rhel-9.6-microshift-source-tuned (parent: rhel-9.6)
10:30:24.213667392
=== End DAG ===
```

Building ostree images
```
No cache.
    Without optimizations
        real	68m46.651s
        user	28m46.796s
        sys	4m14.663s

    With optimizations
        real	50m16.295s
        user	70m10.943s
        sys	7m58.411s

Cache.
    Without optimizations
        real	39m27.046s
        user	30m46.948s
        sys	5m47.311s

    With optimizations
        real	25m59.166s
        user	72m46.412s
        sys	9m7.986s
```
