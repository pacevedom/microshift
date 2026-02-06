# Bootc Image Blueprints

This directory contains bootc container image blueprints organized by layers.
Each layer represents a different build context (base, presubmit, periodic, etc.).

## Build System

The image build system uses **DAG-based scheduling** to maximize parallelism.
Instead of building images sequentially in groups, the scheduler:

1. Parses all blueprints to extract parent-child relationships
2. Builds a dependency graph (DAG)
3. Starts builds as soon as their parent completes
4. Maximizes concurrent builds while respecting dependencies

### Parent Relationships

Parent dependencies are extracted from the build files:

1. **Containerfiles**: Parsed from the `FROM` directive
   ```dockerfile
   FROM localhost/rhel96-test-agent:latest
   ```
   The `localhost/` prefix indicates a local dependency.

2. **.image-bootc files**: Parsed from the file content
   ```
   localhost/rhel96-bootc-source:latest
   ```

External images (e.g., `registry.redhat.io/...`) have no local parent dependency.

## File Types

| Extension | Description |
|-----------|-------------|
| `.containerfile` | Podman container build file |
| `.image-bootc` | Reference to image for bootc-image-builder (ISO creation) |
| `.container-encapsulate` | RPM-ostree container encapsulation target |
| `.template` | Gomplate template (processed before builds) |

## Layer Descriptions

### layer1-base (Cached)

Base layer artifacts independent of current source code.

| Blueprint | Description |
|-----------|-------------|
| `rhel96-test-agent.containerfile` | Test agent base image |
| `rhel96-bootc.image-bootc` | Base bootc ISO |

### layer2-presubmit (Not Cached)

Artifacts that depend on current sources, used by presubmit and periodic jobs.

| Blueprint | Description |
|-----------|-------------|
| `rhel96-bootc-source.containerfile` | Source-based bootc image |
| `rhel96-bootc-source-optionals.containerfile` | With optional packages |

### layer3-periodic (Not Cached)

Artifacts that depend on current sources, used only by periodic jobs.

| Blueprint | Description |
|-----------|-------------|
| `*-isolated.containerfile` | Isolated network testing |
| `*-gitops.containerfile` | GitOps testing |

### layer4-upstream (Not Cached)

CentOS-based artifacts for upstream testing.

| Blueprint | Description |
|-----------|-------------|
| `cos9-*.containerfile` | CentOS 9 based images |
| `cos10-*.containerfile` | CentOS 10 based images |

### layer5-release (Cached)

Release artifacts that depend on Brew RPM packages (VPN required).

| Blueprint | Description |
|-----------|-------------|
| `*-brew*.containerfile` | Brew-based release images |

## Running Builds

```bash
# Build a specific layer
./test/bin/build_bootc_images.sh -l test/image-blueprints-bootc/layer1-base

# Build specific type only
./test/bin/build_bootc_images.sh -l test/image-blueprints-bootc/layer2-presubmit -b containerfile

# Force rebuild of existing images
./test/bin/build_bootc_images.sh -l test/image-blueprints-bootc/layer1-base -f

# Dry run (show what would be built)
./test/bin/build_bootc_images.sh -l test/image-blueprints-bootc/layer1-base -d
```

## Dependency Chain Example

```
registry.redhat.io/rhel9/rhel-9.6-bootc:9.6  (external, no local dep)
    |
    v
rhel96-test-agent.containerfile  (root node)
    |
    v
rhel96-bootc-source.containerfile (parent: rhel96-test-agent)
    |
    v
rhel96-bootc-source-optionals.containerfile (parent: rhel96-bootc-source)
```

With DAG scheduling, `rhel96-bootc-source-optionals` starts building immediately
when `rhel96-bootc-source` completes, without waiting for other unrelated images.
