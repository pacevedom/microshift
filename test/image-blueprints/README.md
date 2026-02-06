# OSTree Image Blueprints

This directory contains OSTree image blueprints organized by layers. Each layer
represents a different build context (base, presubmit, periodic, release).

## Build System

The image build system uses **DAG-based scheduling** to maximize parallelism.
Instead of building images sequentially in groups, the scheduler:

1. Parses all blueprints to extract parent-child relationships
2. Builds a dependency graph (DAG)
3. Starts builds as soon as their parent completes
4. Maximizes concurrent builds while respecting dependencies

### Parent Relationships

Parent dependencies are defined in two ways:

1. **Explicit directive** in the blueprint file:
   ```toml
   # parent = rhel-9.6-microshift-4.17
   name = "rhel-9.6-microshift-4.18"
   ```

2. **Filename prefix convention**:
   - `rhel96-microshift-source.toml` derives parent from `rhel96.toml`

## Layer Descriptions

### layer1-base (Cached)

Base layer artifacts that are independent of current source code.

| Blueprint | Description |
|-----------|-------------|
| `rhel96.toml` | RHEL 9.6 OS-only base image |
| `rhel96-microshift-yminus2.toml` | RHEL 9.6 with MicroShift y-2 packages |
| `rhel96-microshift-previous-minor.toml` | RHEL 9.6 with MicroShift y-1 packages |
| `rhel96-crel.toml` | RHEL 9.6 current release |

### layer2-presubmit (Not Cached)

Artifacts that depend on current sources, used by presubmit and periodic jobs.

| Blueprint | Description |
|-----------|-------------|
| `*-source*.toml` | Current source-based images |

### layer3-periodic (Not Cached)

Artifacts that depend on current sources, used only by periodic jobs.

| Blueprint | Description |
|-----------|-------------|
| Various | Periodic-only test images |

### layer4-release (Cached)

Release artifacts that depend on Brew RPM packages (VPN required).

| Blueprint | Description |
|-----------|-------------|
| `*-brew*.toml` | Brew-based release images |
| `*.image-installer` | ISO installer images |

## Running Builds

```bash
# Build a specific layer
./test/bin/build_images.sh -l test/image-blueprints/layer1-base

# Build all layers
./test/bin/build_images.sh

# Force rebuild of existing images
./test/bin/build_images.sh -f

# Build only source images
./test/bin/build_images.sh -s

# Disable DAG scheduling (legacy mode)
USE_DAG=false ./test/bin/build_images.sh
```
