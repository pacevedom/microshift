# Shift Week: MicroShift to OpenShift Transitions

## Objective

Evaluate the feasibility and document the process of migrating a user application running on a MicroShift cluster to a Single Node OpenShift (SNO) cluster. This includes identifying the right tooling, understanding what transfers cleanly, what requires manual intervention, and what gaps exist.

## Background

MicroShift is a minimal OpenShift-compatible Kubernetes distribution designed for edge and resource-constrained environments. SNO (Single Node OpenShift) is a full OpenShift installation running on a single node. Migrating from MicroShift to SNO is an "upward" transition — SNO is a superset of MicroShift in terms of API surface and platform capabilities.

The question is: given a running application on MicroShift, how much of the migration can be automated, and what requires human judgment?

## Tooling: Konveyor Crane

[Crane](https://github.com/migtools/crane) is a CLI migration tool from the Konveyor community (CNCF Sandbox project). It extracts live Kubernetes resource manifests from a running cluster, strips runtime-only metadata, and produces clean, redeployable YAML.

### Why not just `kubectl get -o yaml`?

Raw exported YAML includes runtime fields that make it non-redeployable:
- `metadata.uid`, `metadata.resourceVersion`, `metadata.creationTimestamp`
- `metadata.managedFields` (server-side apply bookkeeping)
- `status` sections
- Cluster-specific annotations

Crane automates the cleanup through a plugin system.

### Installation

```bash
git clone https://github.com/migtools/crane.git
cd crane
go build -o crane main.go
sudo mv crane /usr/local/bin/
```

Latest release: v0.10.0-alpha.1 (alpha quality — expect rough edges).

### The three-stage pipeline

**1. Export** — discovers and exports all resources from a namespace:
```bash
crane export -n <namespace>
```
Creates `export/resources/<namespace>/` with raw YAML for every namespaced resource.

**2. Transform** — runs plugins that generate JSONPatch operations to clean manifests:
```bash
crane transform
```
The built-in Kubernetes plugin strips `uid`, `resourceVersion`, `creationTimestamp`, `managedFields`. Additional plugins (e.g., OpenShift plugin) can handle platform-specific transformations.

**3. Apply** — merges exports with transforms to produce clean YAML:
```bash
crane apply
```
Output lands in `output/output.yaml`, ready for `kubectl apply -f` on the target cluster.

### Available plugins

| Plugin | Scope |
|---|---|
| Kubernetes (built-in) | Strips runtime metadata from all resources |
| OpenShift | Handles OCP-specific resource transformations |

The plugin ecosystem is small. Custom transforms require writing your own plugin.

### Crane limitations

| Limitation | Impact |
|---|---|
| Namespace-scoped only | Cluster-scoped resources (CRDs, ClusterRoles, ClusterRoleBindings) require manual export |
| No PV data migration | Exports PVC manifests, not the data inside the volumes |
| `new-namespace` flag incomplete | Renaming namespaces only updates `metadata.namespace`, not internal references |
| Alpha quality | Breaking changes expected between versions |
| Cold migration only | Workloads are recreated, not live-migrated. Expect downtime |

## Migration Layers

An application on MicroShift depends on three concentric layers. Each requires a different migration strategy.

```
+-------------------------------------------+
|  OS / Host                                |  Fully manual
|  (firewall, mounts, SELinux, kernel mods) |
|  +-------------------------------------+  |
|  |  Cluster Configuration              |  |  Manual export + recreate
|  |  (CRDs, StorageClasses,             |  |
|  |   ClusterRoles, Operators)          |  |
|  |  +-------------------------------+  |  |
|  |  |  Namespace Resources          |  |  |  Crane handles this
|  |  |  (Deployments, Services,      |  |  |
|  |  |   ConfigMaps, Secrets, PVCs,  |  |  |
|  |  |   Routes, NetworkPolicies)    |  |  |
|  |  +-------------------------------+  |  |
|  +-------------------------------------+  |
+-------------------------------------------+
```

### Layer 1: Namespace resources (Crane covers this)

Deployments, Services, ConfigMaps, Secrets, PVCs, ServiceAccounts, Roles, RoleBindings, NetworkPolicies, Routes, and any Custom Resource instances living in the namespace.

### Layer 2: Cluster-scoped resources (manual)

- **CRDs**: `crane export -n <namespace>` exports CR instances but NOT the CRD definitions themselves. Export manually:
  ```bash
  kubectl get crd <name> -o yaml | kubectl neat > crd-<name>.yaml
  ```
- **ClusterRoles / ClusterRoleBindings**: if the app's ServiceAccount depends on cluster-level RBAC.
- **StorageClasses**: must exist on the target before PVCs can bind.
- **Operators**: on SNO, install via OLM (OperatorHub) before applying CR instances.

### Layer 3: Host-level dependencies (fully manual)

- Host path mounts (directories must exist on the SNO node)
- Host ports / hostNetwork usage
- Firewall rules
- SELinux labels
- Kernel modules

None of this appears in Kubernetes manifests. Invisible to Crane.

## MicroShift to SNO: Specific Considerations

### What transfers cleanly

**Security Context Constraints (SCCs)**: MicroShift ships the full set of OpenShift SCCs — restricted, restricted-v2, restricted-v3, anyuid, privileged, hostaccess, hostmount-anyuid, hostnetwork, nonroot, nonroot-v2. These are the exact same definitions from `kube-apiserver-operator` assets that SNO uses. If an app works under a given SCC on MicroShift, it works on SNO with no changes.

**NetworkPolicies**: Portable across any CNI that supports them. Both MicroShift and SNO use OVN-Kubernetes.

**Routes**: Both platforms support OpenShift Routes natively. The manifests transfer as-is, though the hostname will need updating (see below).

**RBAC (namespaced)**: Roles and RoleBindings within the namespace work identically.

### What requires adjustment

**Storage classes**: MicroShift uses LVMS with StorageClass `topolvm-provisioner`. SNO options:
- Install LVMS on SNO via OLM (PVCs work as-is)
- Use `local-storage-operator` and update StorageClass in PVC manifests
- Use ODF if the SNO node has sufficient resources

**Ingress domain / Route hostnames**: MicroShift Routes use a configured domain. SNO uses `*.apps.<cluster-name>.<base-domain>`. Route manifests exported by Crane contain the old hostname. Options:
- Edit Route manifests to update the host
- Write a custom Crane transform plugin
- Recreate Routes on SNO with `oc expose` to auto-assign the correct domain

**CRDs and Operators**: On MicroShift, CRDs may have been installed manually or via `microshift-olm`. On SNO, install the corresponding operator through OperatorHub, which brings CRDs automatically. Install operators on SNO BEFORE applying Crane output.

**Optional MicroShift packages**:

| MicroShift package | SNO equivalent |
|---|---|
| `microshift-olm` | Built-in (OLM always present on SNO) |
| `microshift-multus` | Multus operator via OperatorHub |
| `microshift-networking` extras | Standard OVN-K (always present, more features) |

**Image pull secrets**: On MicroShift, the node-level pull secret lives at `/etc/crio/openshift-pull-secret`. On SNO, it's a cluster-wide pull secret in `openshift-config`. Namespace-scoped pull secrets (exported by Crane) work as-is. Node-level pull secrets require separate configuration.

**Persistent data**: Crane exports PVC manifests, not data. Handle separately via rsync, backup/restore, or application-level data migration.

### What is NOT a concern

- **API compatibility**: MicroShift exposes a subset of OpenShift APIs. SNO has all of them. Going from MicroShift to SNO means no missing APIs. (The reverse direction — SNO to MicroShift — would be harder.)
- **SCCs**: Identical between platforms (verified in MicroShift source at `assets/controllers/openshift-default-scc-manager/`).
- **NetworkPolicy support**: Both use OVN-K with full NetworkPolicy support.

## Sample Application

A minimal notes API is provided in `shift-week/sample-app/` to exercise every resource type relevant to the migration. It uses `python:3-alpine` with a ~30-line HTTP server that supports POST (save a note) and GET (list all notes), persisting data to a PVC-backed JSON file.

### Resource types covered

| Resource | File | Purpose |
|---|---|---|
| Namespace | `namespace.yaml` | `shift-week-demo` |
| ConfigMap | `configmap.yaml` | App title and message (env vars) |
| Secret | `secret.yaml` | API token (env var) |
| PVC | `pvc.yaml` | 1Gi on `topolvm-provisioner` (LVMS) |
| ServiceAccount | `serviceaccount.yaml` | `demo-app` |
| Role + RoleBinding | `rbac.yaml` | ConfigMap read access |
| Deployment | `deployment.yaml` | Single replica, image from `quay.io/pacevedo/shift-week-notes:latest` |
| Service | `service.yaml` | ClusterIP on port 8080 |
| Route | `route.yaml` | Edge TLS termination |
| NetworkPolicy | `networkpolicy.yaml` | Ingress from OpenShift router only |

### Build and deploy

```bash
cd shift-week/sample-app
make build          # build container image
make push           # build + push to quay.io/pacevedo/shift-week-notes:latest
make deploy         # apply all manifests, wait for pod ready
make test NODE_IP=<node-ip>   # GET, POST a note, GET again
make undeploy       # tear it all down
```

The `NODE_IP` variable is required for `make test` because `--connect-to` is used to bypass DNS resolution.

### API

```bash
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')

# GET all notes
curl -sk --connect-to "$ROUTE:443:<node-ip>:443" https://$ROUTE

# POST a note
curl -sk --connect-to "$ROUTE:443:<node-ip>:443" -X POST -d "my note" https://$ROUTE
```

GET response:
```json
{
  "app": "Shift Week Demo",
  "message": "Notes API running on MicroShift",
  "token": "shift-week-demo-token-2026",
  "notes": [
    {"id": 1, "text": "my note"}
  ]
}
```

## Migration Playbook

### Step 1: Seed test data on MicroShift

Before migrating, populate the app with notes that serve as evidence of pre-migration state:

```bash
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
NODE_IP=<microshift-node-ip>

curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "note before migration - 1" https://$ROUTE
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "note before migration - 2" https://$ROUTE
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "note before migration - 3" https://$ROUTE

# Save the "before" snapshot
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool | tee before-migration.json
```

### Step 2: Install Crane

```bash
git clone https://github.com/migtools/crane.git /tmp/crane
cd /tmp/crane
go build -o crane main.go
sudo mv crane /usr/local/bin/
crane --help
```

### Step 3: Export from MicroShift

Ensure `KUBECONFIG` points at the MicroShift cluster:

```bash
mkdir -p ~/shift-week-migration
cd ~/shift-week-migration
crane export -n shift-week-demo
```

Verify what got exported:

```bash
find export/ -type f
```

Expected: YAML files for Deployment, Service, Route, PVC, ConfigMap, Secret, ServiceAccount, Role, RoleBinding, NetworkPolicy, and any auto-generated resources (like SA token secrets).

### Step 4: Transform

```bash
crane transform
```

This generates JSONPatch operations that strip runtime metadata (`uid`, `resourceVersion`, `creationTimestamp`, `managedFields`).

Inspect what was generated:

```bash
find transform/ -type f
```

### Step 5: Generate clean manifests

```bash
crane apply
```

Clean, redeployable manifests land in `output/output.yaml`. Review them:

```bash
cat output/output.yaml
```

Check:
- Are all resources present?
- Is `metadata.uid`, `resourceVersion`, `creationTimestamp` gone?
- Does the PVC still reference `topolvm-provisioner`?
- What hostname does the Route have?

### Step 6: Identify gaps

Check what Crane did NOT capture — cluster-scoped resources:

```bash
# The namespace itself (Crane exports resources IN the namespace, not the namespace object)
oc get namespace shift-week-demo -o yaml

# CRDs the app uses (none for our sample app)
# oc get crd | grep <keyword>

# ClusterRoles / ClusterRoleBindings (none for our sample app)
```

For the sample app there are no cluster-scoped dependencies beyond the namespace itself.

### Step 7: Audit on MicroShift

Understand what the application depends on at every layer:

```bash
# List all resources in the namespace
kubectl api-resources --verbs=list --namespaced -o name | \
  xargs -I {} kubectl get {} -n shift-week-demo --no-headers 2>/dev/null

# Check for hostNetwork/hostPort usage
kubectl get pods -n shift-week-demo -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.hostNetwork}{"\t"}{range .spec.containers[*]}{.ports[*].hostPort}{end}{"\n"}{end}'

# Check service types (NodePort, LoadBalancer)
kubectl get svc -n shift-week-demo -o wide

# Check NetworkPolicies
kubectl get networkpolicy -n shift-week-demo

# Check Routes
kubectl get routes -n shift-week-demo

# Check PVCs and their StorageClasses
kubectl get pvc -n shift-week-demo

# Check MicroShift config
cat /etc/microshift/config.yaml

# Check firewall rules
sudo firewall-cmd --list-all
```

### Step 8: Prepare SNO

Switch to the SNO kubeconfig and verify prerequisites:

```bash
export KUBECONFIG=<path-to-sno-kubeconfig>

# Check available storage classes
oc get storageclass

# If LVMS is not available, either:
#   - Install LVMS operator via OperatorHub (then PVCs work as-is)
#   - Or note the available StorageClass name for manifest adjustment

# Verify the cluster can pull the app image
oc run test-pull --image=quay.io/pacevedo/shift-week-notes:latest --restart=Never -n default
oc get pod test-pull -n default
oc delete pod test-pull -n default
```

Preparation checklist:
1. Install required operators via OperatorHub (these bring CRDs)
2. Create required ClusterRoles / ClusterRoleBindings (if app-specific)
3. Set up storage (LVMS operator, local-storage-operator, or ODF)
4. Configure pull secrets for private registries
5. Prepare host-level requirements (directories, firewall ports)

### Step 9: Adjust manifests for SNO

Create a working copy of the Crane output:

```bash
cp output/output.yaml output/output-sno.yaml
```

Edit `output/output-sno.yaml` and fix:

**Storage class** — if SNO doesn't have LVMS, change `topolvm-provisioner` to the available StorageClass:
```bash
# Check what SNO offers
oc get storageclass
# Then update the PVC in output-sno.yaml accordingly
```

**Route hostname** — the exported Route contains the MicroShift hostname. Either:
- Remove the `spec.host` field entirely (let SNO auto-assign based on its ingress domain)
- Or set it to the correct SNO domain (`*.apps.<cluster-name>.<base-domain>`)

**Image pull** — `quay.io/pacevedo/shift-week-notes:latest` is public, so no pull secret needed. For private registries, add a pull secret to the namespace.

### Step 10: Apply on SNO

```bash
export KUBECONFIG=<path-to-sno-kubeconfig>

# Create the namespace
oc create namespace shift-week-demo

# Apply the adjusted manifests
oc apply -f output/output-sno.yaml
```

Watch it come up:

```bash
oc get pods -n shift-week-demo -w
```

If a pod is stuck, diagnose:

```bash
# PVC stuck in Pending → wrong StorageClass
oc get pvc -n shift-week-demo

# Pod stuck in ImagePullBackOff → registry auth issue
# Pod stuck in CrashLoopBackOff → check logs
oc describe pod -l app=demo-app -n shift-week-demo
oc logs -l app=demo-app -n shift-week-demo
```

### Step 11: Validate on SNO

```bash
# Check all resources are present
oc get deploy,svc,route,pvc,cm,secret,networkpolicy,sa -n shift-week-demo

# Test the app
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
NODE_IP=<sno-node-ip>

curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool
```

The notes list will be **empty** — Crane migrated the PVC manifest but not the data inside the volume. This is expected and demonstrates the data migration gap.

Verify the app is fully functional on SNO:

```bash
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "first note on SNO" https://$ROUTE
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool | tee after-migration.json
```

### Step 12: Compare results

Compare the "before" snapshot (Step 1) with the "after" (Step 11):

| Aspect | Expected result |
|---|---|
| App config (title, message, token) | Identical — came from ConfigMap and Secret |
| Notes | Empty on SNO — PVC data not migrated, only the manifest |
| Route | Working, but different hostname |
| NetworkPolicy | Working — same OVN-K on both platforms |
| RBAC | Working — same SCCs and roles |
| Service | Working — ClusterIP reachable internally |

This comparison is the core evidence: what Crane covers, what it doesn't, and what manual steps bridge the gap.

## Assessment

Crane covers approximately 40-50% of the migration work — the mechanical extraction and cleanup of namespaced manifests. The remaining effort is:

- **Platform preparation** (~30%): installing operators, storage, configuring the SNO cluster to support the app's requirements.
- **Manifest adjustments** (~10%): storage classes, route hostnames, node-specific references.
- **Data migration** (~10%): persistent volume data, if applicable.

The MicroShift → SNO direction is favorable because it's an upward transition in platform capability. The main practical traps are storage class mismatches and missing CRDs/operators on the target.

## References

- [Konveyor Crane — GitHub](https://github.com/migtools/crane)
- [Crane Go Package Docs](https://pkg.go.dev/github.com/konveyor/crane)
- [Migrate Kubernetes Applications with Crane — Red Hat Cloud Experts](https://cloud.redhat.com/experts/redhat/crane/)
- [IBM Developer — Migrate with Konveyor Crane](https://developer.ibm.com/tutorials/migrate-kubernetes-cluster-openshift-konveyor-crane/)
- [Crane Plugin Management Docs](https://crane-docs.konveyor.io/content/usage/plugin-management/)
- [MicroShift SCC Assets](https://github.com/openshift/microshift/tree/main/assets/controllers/openshift-default-scc-manager)
