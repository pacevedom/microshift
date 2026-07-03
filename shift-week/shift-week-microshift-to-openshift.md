# Shift Week: MicroShift to OpenShift Transitions

## Objective

Evaluate the feasibility of migrating a user application from MicroShift to Single Node OpenShift (SNO), document what transfers cleanly, what requires manual intervention, and what gaps exist.

## Why migrate applications, not the platform?

MicroShift and SNO cannot be converted into each other. They are fundamentally different platform topologies built on different operating system foundations:

- **MicroShift** runs as an RPM on RHEL or RHEL for Edge. The Kubernetes control plane is a single binary embedded in the OS. There are no operators managing the cluster — components like OVN-K, LVMS, and CoreDNS are built-in and configured via `/etc/microshift/config.yaml`. The OS is managed by rpm-ostree or image-based updates.
- **SNO** is a full OpenShift installation on RHCOS (Red Hat CoreOS). The control plane runs as pods managed by a set of cluster operators (CVO, MCO, etc.). Every infrastructure component — networking, storage, ingress, monitoring — is an operator that reconciles its own state. The OS is managed by the Machine Config Operator.

There is no upgrade path from one to the other. You cannot install OpenShift operators on top of MicroShift's embedded components, and you cannot strip SNO down to MicroShift's topology. The operating system, the control plane architecture, and the lifecycle management model are all different.

This means when a workload outgrows MicroShift (needs more operators, more observability, multi-tenancy, or simply needs to move from edge to datacenter), the **application** must be migrated — extracted from one platform and deployed on the other. The platform stays behind.

MicroShift → SNO is an "upward" transition: SNO is a superset of MicroShift in API surface and platform capabilities. Everything the application uses on MicroShift exists on SNO, making this direction favorable.

## What kind of application are we migrating?

Not all applications require the same migration effort. The complexity depends on three dimensions:

**State**: does the application store data that must survive the migration?

| Type | Example | Migration impact |
|---|---|---|
| Stateless | Web frontend, API gateway | Redeploy from source (Helm, GitOps, pipeline). No data to migrate. |
| Stateful (reconstructable) | App with DB that can be seeded, cache that rebuilds | Redeploy + let the app reconstruct its state. |
| Stateful (persistent) | App with PVCs holding user data, logs, uploads | Must migrate both manifests and PVC data. This is the hard case. |

**Complexity**: how many Kubernetes resource types does the application touch?

| Level | Resources involved | Migration impact |
|---|---|---|
| Simple | Deployment, Service, ConfigMap, Secret | Velero handles it with minimal adjustments. |
| Medium | Above + PVCs, Routes, NetworkPolicies, RBAC | Velero handles it, but StorageClass and Route hostname need fixing. |
| Complex | Above + CRDs, CRs, ClusterRoles, multi-namespace | Velero handles it, but operators that manage CRDs must be installed on the target first. |

**Packaging**: how is the application deployed today?

| Packaging | Migration approach |
|---|---|
| Raw manifests (no tooling) | Velero for everything — the only option that works without prior packaging. This is what the experiment demonstrates. |
| Helm chart | `helm install` with target-specific values for manifests + Velero only for PVC data. Avoids most Velero gotchas. |
| GitOps (ArgoCD/Flux) | Sync from Git repo to target cluster + Velero for PVC data. Manifests are already environment-aware. |
| Operator-managed | Install operator on target + apply CR. Some operators handle data migration internally. |

The experiment's sample application is designed to touch every resource type category while remaining small enough to demo: **stateful, complex, raw manifests**. It has a PVC with persistent data, a custom CRD with a CR instance, cluster-scoped RBAC, and no packaging. A real production application would have more CRDs, more cross-namespace dependencies, operator-managed resources, and larger datasets — but the migration challenges (StorageClass mismatches, pod CIDR conflicts, CRD ordering, stale runtime state) are the same regardless of scale.

## Tooling

### Velero
**[Velero](https://velero.io/)** is one famous migration tool. It backs up and restores Kubernetes resources (namespaced and cluster-scoped) AND persistent volume data via Kopia file-level copy. It handles everything needed for a one-off migration: Deployments, Services, Routes, CRDs, ClusterRoles, PVCs, and the data inside the volumes.

| What Velero migrates | How |
|---|---|
| Namespaced resources (Deployments, Services, Routes, etc.) | Backup + restore |
| Cluster-scoped resources (CRDs, ClusterRoles, Namespaces) | Backup + restore |
| PVC data | Kopia file-level copy via S3 |

### Crane

[Crane](https://github.com/migtools/crane) (Konveyor, CNCF Sandbox) extracts namespaced resource manifests and strips runtime metadata (`uid`, `resourceVersion`, `creationTimestamp`), producing clean redeployable YAML. However, it proved **insufficient as a migration tool** for several reasons:

1. **Cannot migrate PVC data** — exports PVC manifests but not the data inside the volumes.
2. **Cannot export cluster-scoped resources** — CRDs, ClusterRoles, ClusterRoleBindings, and Namespaces are invisible to Crane.
3. **Incompatible with Velero's FSB (File System Backup) restore** — Velero's Kopia file-level data restore is tied to specific pod names from the backup. Crane-created Deployments spawn pods with different ReplicaSet hashes and names, causing Velero's FSB restore to hang indefinitely waiting for pods that will never appear.
4. **Cannot run before Velero** — due to point 3, Crane manifests must be applied after the full Velero restore. At that point, all resources already exist on the target cluster, making Crane's output redundant for the migration itself.

**Crane remains useful for**: extracting clean YAML for GitOps onboarding or version control, independently of the migration process. It is not part of the migration playbook.

### Restore process

The correct sequence on the target cluster:

```
1. Velero full restore — creates everything (Namespaces, CRDs, ClusterRoles,
   Deployments, Services, Routes, PVCs, pods) and restores PVC data via
   Kopia FSB. The pods must match the backup names for FSB to work.

2. Delete pods — removes Velero's restored pods, which carry injected
   restore-wait init containers and stale runtime state. The Deployment
   controller recreates them cleanly with the restored data on the PVC.

3. Post-migration adjustments — fix Route hostnames, StorageClass names,
   or other target-specific values via oc patch / oc edit.
```

Approaches that failed during the experiment:
- **Crane before Velero**: FSB restore hangs — pod names don't match the backup.
- **Crane between Velero phases**: same problem — Velero can't find the Crane-created pods.
- **Velero with `--include-resources` filtering**: restricting to PVCs/PVs restores manifests but skips the FSB data, because the data restore is triggered by the Pod resource.

## MicroShift → SNO: what to know

**Transfers cleanly**: SCCs (identical on both — verified in source), NetworkPolicies, Routes (same OVN-K), namespaced RBAC, API compatibility (SNO has all MicroShift APIs and more).

**Requires adjustment**:
- **Pod CIDR mismatch**: MicroShift uses `10.42.0.0/16`, SNO typically uses `10.128.0.0/14`. Velero restores pods with OVN-K annotations containing the source cluster's IP addresses. OVN-K on the target rejects these IPs and the pod can't start, which also blocks the FSB data restore. **Fix**: use a Velero resource modifier to strip `k8s.ovn.org/pod-networks` annotations during restore (see Step 4).
- **StorageClass**: MicroShift uses `topolvm-provisioner` (LVMS). SNO's LVMS creates `lvms-vg1`. Create a `topolvm-provisioner` StorageClass alias on SNO, or install LVMS with matching names (see Step 3).
- **Route hostnames**: restored Routes carry the MicroShift domain. Patch with `oc patch route <name> -n <ns> --type=json -p '[{"op":"remove","path":"/spec/host"}]'` to let SNO auto-assign.
- **CRDs/Operators**: if CRDs come from an operator, install the operator via OperatorHub on SNO. Velero restores the CRD definition but not the operator that manages it.
- **Image pull secrets**: node-level pull secret location differs (`/etc/crio/openshift-pull-secret` vs `openshift-config`). Namespace-scoped secrets transfer fine via Velero.
- **Host-level deps**: hostPath mounts, firewall rules, SELinux labels, kernel modules — invisible to Velero.

## Velero Installation

**CLI**:
```bash
curl -LO https://github.com/vmware-tanzu/velero/releases/download/v1.15.0/velero-v1.15.0-linux-amd64.tar.gz
tar xzf velero-v1.15.0-linux-amd64.tar.gz
sudo mv velero-v1.15.0-linux-amd64/velero /usr/local/bin/
velero version --client-only
```

**AWS prerequisites**:

1. Create an S3 bucket:
   ```bash
   BUCKET=shift-week-velero-backups
   REGION=us-east-2
   aws s3api create-bucket --bucket $BUCKET --region $REGION \
     --create-bucket-configuration LocationConstraint=$REGION
   ```

2. IAM policy — attach to the user/role whose credentials Velero will use:
   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject",
                    "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
         "Resource": "arn:aws:s3:::shift-week-velero-backups/*"
       },
       {
         "Effect": "Allow",
         "Action": "s3:ListBucket",
         "Resource": "arn:aws:s3:::shift-week-velero-backups"
       }
     ]
   }
   ```

3. Credentials file (or reuse `~/.aws/credentials`):
   ```bash
   cat > credentials-velero <<EOF
   [default]
   aws_access_key_id=<KEY>
   aws_secret_access_key=<SECRET>
   EOF
   ```

**Server install** (run on each cluster that needs backup/restore):

On OpenShift/MicroShift, Velero's data mover pods (spawned per-backup in v1.17+) need privileged access to read CSI volume mounts on the host. This requires both `--privileged-node-agent` and a ConfigMap with `privilegedFsBackup: true`, referenced at install time via `--node-agent-configmap`. Images must be fully qualified (`docker.io/...`) because CRI-O on RHEL enforces short-name resolution.

```bash
BUCKET=shift-week-migration
REGION=eu-west-1

# Create namespace and ConfigMap before install
oc create namespace velero

cat <<EOF | oc apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: node-agent-config
  namespace: velero
data:
  config: |
    {"privilegedFsBackup": true}
EOF

velero install \
  --provider aws \
  --image docker.io/velero/velero:v1.18.2 \
  --plugins docker.io/velero/velero-plugin-for-aws:v1.11.0 \
  --bucket $BUCKET \
  --secret-file ./credentials-velero \
  --backup-location-config region=$REGION \
  --use-node-agent \
  --privileged-node-agent \
  --uploader-type=kopia \
  --default-volumes-to-fs-backup \
  --node-agent-configmap=node-agent-config

oc adm policy add-scc-to-user privileged -z velero -n velero
oc adm policy add-scc-to-user privileged -z node-agent -n velero

# Verify
oc get pods -n velero
velero backup-location get
```

For MinIO or non-AWS S3, add `s3ForcePathStyle=true,s3Url=<endpoint>` to `--backup-location-config`.

## Sample Application

Minimal notes API in `shift-week/sample-app/` exercising all migration-relevant resource types. Python HTTP server that reads a NoteConfig CRD from the Kubernetes API to enforce `maxNotes` limits and `retentionDays` expiry on stored notes.

| Resource | File | Scope |
|---|---|---|
| Namespace | `namespace.yaml` | Cluster |
| CRD (`NoteConfig`) | `crd.yaml` | Cluster |
| ClusterRole + ClusterRoleBinding | `clusterrbac.yaml` | Cluster |
| ConfigMap | `configmap.yaml` | Namespace |
| Secret | `secret.yaml` | Namespace |
| PVC (LVMS) | `pvc.yaml` | Namespace |
| ServiceAccount | `serviceaccount.yaml` | Namespace |
| Role + RoleBinding | `rbac.yaml` | Namespace |
| NoteConfig CR | `noteconfig.yaml` | Namespace |
| Deployment | `deployment.yaml` | Namespace |
| Service | `service.yaml` | Namespace |
| Route (edge TLS) | `route.yaml` | Namespace |
| NetworkPolicy | `networkpolicy.yaml` | Namespace |

```bash
cd shift-week/sample-app
make build                    # build container image
make push                     # build + push to quay.io/pacevedo/shift-week-notes:latest
make deploy                   # apply all manifests, wait for pod ready
make test NODE_IP=<node-ip>   # GET, POST a note, GET again
make undeploy                 # tear it all down
```

## Migration Playbook

### Step 1: Seed test data on MicroShift

```bash
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
NODE_IP=<microshift-node-ip>

curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "note before migration - 1" https://$ROUTE
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "note before migration - 2" https://$ROUTE

# Save "before" snapshot for comparison
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool | tee before-migration.json
```

### Step 2: Back up with Velero

Install Velero on MicroShift (see [Velero Installation](#velero-installation)), then back up the namespace. Velero captures everything: namespaced resources, cluster-scoped resources (NoteConfig CRD, ClusterRole), and PVC data via Kopia.

```bash
velero backup create shift-week-backup --include-namespaces shift-week-demo --wait
velero backup describe shift-week-backup --details
```

Verify the resource list includes:
- `customresourcedefinitions` — the NoteConfig CRD
- `clusterroles` / `clusterrolebindings` — demo-app-cluster-role
- `noteconfigs` — the CR instance
- PVC data under "Pod Volume Backups"

Keep the app running during backup — Kopia can only back up volumes mounted by a running pod.

### Step 3: Prepare SNO

```bash
export KUBECONFIG=<path-to-sno-kubeconfig>
```

**Install LVMS** so the `topolvm-provisioner` StorageClass matches MicroShift. Without this, Velero restores the PVC with a StorageClass that doesn't exist on SNO and it stays `Pending`.

```bash
# Create the namespace and OperatorGroup (required by OLM before any operator installs)
oc create namespace openshift-storage

oc apply -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: openshift-storage-operatorgroup
  namespace: openshift-storage
spec:
  targetNamespaces:
  - openshift-storage
EOF

# Find the correct channel — it's versioned (stable-4.XX), not just "stable"
OCP_MINOR=$(oc get clusterversion version -o jsonpath='{.status.desired.version}' | grep -oP '^\d+\.\d+')
echo "Using channel: stable-${OCP_MINOR}"

# Install the LVMS operator
oc apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: lvms-operator
  namespace: openshift-storage
spec:
  channel: stable-${OCP_MINOR}
  name: lvms-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

# Wait for the operator pod
sleep 15
oc wait --for=condition=ready pod -l app.kubernetes.io/name=lvms-operator -n openshift-storage --timeout=120s

# Create the LVMCluster — this provisions the volume group and StorageClass
oc apply -f - <<EOF
apiVersion: lvm.topolvm.io/v1alpha1
kind: LVMCluster
metadata:
  name: lvmcluster
  namespace: openshift-storage
spec:
  storage:
    deviceClasses:
      - name: vg1
        thinPoolConfig:
          name: thin-pool-1
          sizePercent: 90
          overprovisionRatio: 10
EOF

# Verify the StorageClass exists
oc get storageclass
```

SNO's LVMS typically creates `lvms-vg1` instead of MicroShift's `topolvm-provisioner`. Create a StorageClass alias so Velero-restored PVCs bind without patching:

```bash
oc apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: topolvm-provisioner
provisioner: topolvm.io
parameters:
  csi.storage.k8s.io/fstype: xfs
  topolvm.io/device-class: vg1
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
EOF

# Verify both StorageClasses exist
oc get storageclass | grep topolvm
```

**Verify image pull**:
```bash
oc run test-pull --image=quay.io/pacevedo/shift-week-notes:latest --restart=Never -n default
oc delete pod test-pull -n default
```

### Step 4: Restore on SNO

Install Velero on SNO pointing to the same S3 bucket (see [Velero Installation](#velero-installation)).

MicroShift and SNO use different pod CIDRs (`10.42.0.0/16` vs `10.128.0.0/14`). Velero restores pods with OVN-K annotations containing the source cluster's IP addresses, which OVN-K on SNO rejects — the pod can't start and the FSB data restore deadlocks. The fix is a Velero [resource modifier](https://velero.io/docs/main/restore-resource-modifiers/) that strips the network annotations during restore:

```bash
# Create resource modifier to strip OVN-K annotations from pods
cat > /tmp/resource-modifiers.yaml <<'EOF'
version: v1
resourceModifierRules:
- conditions:
    groupResource: pods
  mergePatches:
  - patchData: |
      {
        "metadata": {
          "annotations": {
            "k8s.ovn.org/pod-networks": null
          }
        }
      }
EOF

kubectl create cm strip-network-annotations \
  --from-file=/tmp/resource-modifiers.yaml -n velero

# Full Velero restore with the resource modifier
velero restore create shift-week-restore \
  --from-backup shift-week-backup \
  --resource-modifier-configmap strip-network-annotations \
  --wait

# Verify the restore completed and includes Pod Volume Restores
velero restore describe shift-week-restore --details

# Delete pods — removes Velero's stale pods (with injected
# restore-wait init containers). The Deployment recreates them cleanly.
oc delete pod -l app=demo-app -n shift-week-demo
oc wait --for=condition=ready pod -l app=demo-app -n shift-week-demo --timeout=120s
```

### Step 5: Post-migration adjustments

Fix target-specific values that don't match the source cluster:

```bash
# Fix Route hostname — remove the old host to let SNO auto-assign
oc patch route demo-app -n shift-week-demo --type=json \
  -p '[{"op":"remove","path":"/spec/host"}]'

# If StorageClass differs, the PVC was already restored and bound by Velero.
# For future PVCs, update the default StorageClass on SNO.

# Verify the Route got a new hostname
oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}'; echo
```

### Step 6: Validate

```bash
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
NODE_IP=<sno-node-ip>

# Check all resources are present
oc get deploy,svc,route,pvc,cm,secret,networkpolicy,noteconfigs -n shift-week-demo

# Check cluster-scoped resources
oc get crd noteconfigs.shift-week.example.com
oc get clusterrole demo-app-cluster-role

# Test the app — notes should be present from before migration
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool | tee after-migration.json

# Post a new note to verify full functionality
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "first note on SNO" https://$ROUTE
```

### Expected results

| Aspect | Expected |
|---|---|
| App config (title, message, token) | Identical — from ConfigMap and Secret |
| Notes | Present — PVC data restored, with createdAt/expiresAt timestamps |
| NoteConfig CR | Present — maxNotes and retentionDays enforced |
| Route | Working, new hostname after patching |
| NetworkPolicy | Working |
| RBAC / SCCs | Working — both namespaced and cluster-scoped |
| CRD | Present — NoteConfig CRD restored by Velero |

## Assessment

### Key finding

**Velero is sufficient for a one-off MicroShift → SNO migration.** It handles everything: namespaced resources, cluster-scoped resources (CRDs, ClusterRoles), and PVC data. The migration process is: full Velero backup on source, full Velero restore on target, delete pods to clean up stale runtime state, patch Route hostnames.

**Crane is not needed for migration.** It was evaluated during this experiment and found to be incompatible with Velero's FSB data restore due to pod name mismatches. Crane produces clean YAML suitable for GitOps onboarding, but this is a separate concern from migration. For post-migration manifest adjustments, `oc patch` and `oc edit` are simpler and sufficient.

### What Velero covers

| Layer | Velero | Manual |
|---|---|---|
| Namespaced resources (Deployments, Services, Routes, etc.) | Backup + restore | |
| Cluster-scoped resources (CRDs, ClusterRoles, Namespaces) | Backup + restore | Operators via OperatorHub |
| PVC data | Kopia FSB | |
| Route hostnames | | `oc patch` after restore |
| Pod CIDR mismatch | Resource modifier strips OVN annotations | |
| StorageClass differences | | StorageClass alias or LVMS on target |
| Host-level deps (firewall, SELinux, mounts) | | Fully manual |

### Lessons learned

1. **Velero alone is sufficient**: it handles namespaced resources, cluster-scoped resources, and PVC data. Crane adds no value to the migration workflow itself.
2. **Pod CIDR mismatch blocks FSB restore**: MicroShift uses `10.42.0.0/16`, SNO uses `10.128.0.0/14`. Velero restores pods with OVN-K annotations containing the source IP. OVN-K on the target rejects it, the pod can't start, and the FSB data restore deadlocks. **Fix**: use `--resource-modifier-configmap` to strip `k8s.ovn.org/pod-networks` annotations during restore.
3. **StorageClass names differ**: MicroShift's LVMS creates `topolvm-provisioner`, SNO's creates `lvms-vg1`. Velero-restored PVCs reference the source StorageClass. **Fix**: create a `topolvm-provisioner` StorageClass alias on SNO before the restore.
4. **Crane is incompatible with Velero FSB**: Crane-created Deployments spawn pods with different names than the backup. Velero's FSB data restore depends on matching pod names and hangs indefinitely if they don't match.
5. **Delete pods after Velero restore**: Velero-restored pods carry injected `restore-wait` init containers. Deleting them lets the Deployment recreate clean pods with the restored data already on the PVC.
6. **Never use `--include-resources` for FSB restores**: filtering to PVCs/PVs restores manifests but skips the Kopia file-level data, because the data restore is triggered by the Pod resource.
7. **Velero on MicroShift requires extra configuration**: fully qualified image names (`docker.io/...`), `privilegedFsBackup: true` in a ConfigMap referenced at install time via `--node-agent-configmap`, and privileged SCC grants. None of this is documented for MicroShift specifically.
8. **Stuck restores block new ones**: if a Velero restore gets stuck `InProgress (Deleting)`, it blocks subsequent restores. Fix by removing finalizers with `oc patch restore <name> -n velero --type=merge -p '{"metadata":{"finalizers":null}}'` and restarting the Velero pod.
9. **SCCs are identical**: MicroShift ships the full set of OpenShift SCCs. This is not a migration concern.
10. **The MicroShift → SNO direction is favorable**: SNO is a superset of MicroShift in API surface. The main practical traps are pod CIDR mismatch, StorageClass names, and Route hostnames — all solvable with pre-restore configuration.

## Migration Alternatives

This experiment used Velero for everything (manifests + data) because the sample app had no prior packaging. For larger or better-packaged applications, other approaches reduce the need for Velero's manifest handling and its associated gotchas (pod CIDR annotations, StorageClass names, stale runtime state).

### Comparison

| Method | Manifests | Data | Environment adaptation | Best for |
|---|---|---|---|---|
| **Velero** (this experiment) | Backup + restore | Kopia FSB | Resource modifiers, post-restore patches | Any app, one-off migration, no prior packaging |
| **Helm** | `helm install` with target values | Velero for PVC data | Values files per environment | Apps with existing Helm charts |
| **GitOps** (ArgoCD/Flux) | Sync from Git repo | Velero for PVC data | Kustomize overlays or Helm values per cluster | Apps already managed via GitOps |
| **Operator** | Install operator + apply CR | Operator handles data (if supported) | CR spec adapts to environment | Operator-managed apps |
| **CI/CD pipeline** | Pipeline deploys from scratch | App rebuilds state | Pipeline config per environment | Stateless or state-reconstructable apps |

### When to use each

**Velero for everything** — the app has no packaging (raw manifests, no chart, no Git repo). Velero is the only option that migrates both manifests and data without requiring the app to be packaged first. This is what the experiment demonstrated. The trade-offs are the gotchas documented in the lessons learned: pod CIDR mismatches, StorageClass naming, stale runtime state on restored pods.

**Helm for manifests + Velero for data** — the app is packaged as a Helm chart. Run `helm install` on SNO with target-specific values (`storageClass: lvms-vg1`, `route.host: auto`). This avoids all the manifest gotchas because the chart renders fresh manifests for the target environment — no stale pod annotations, no StorageClass mismatches, no Route hostname patches. Use Velero only for PVC data migration. This is the sweet spot for most medium-to-large applications.

**GitOps for manifests + Velero for data** — the app is already managed by ArgoCD or Flux pointing at a Git repo. Point the GitOps tool at the SNO cluster, apply the right overlay/values, and let it sync. Data migration is still Velero. The manifests are already version-controlled and environment-aware, so no migration tooling is needed for them.

**Operator-managed** — the app is deployed and managed by a Kubernetes operator. Install the operator on SNO via OperatorHub and apply the CR with target-specific config. The operator reconciles everything. Some operators (like database operators) have built-in backup/restore for data migration, eliminating the need for Velero entirely.

**CI/CD pipeline rebuild** — the app is stateless or can reconstruct its state (database seeds, API sync, event replay). Run the deployment pipeline against SNO. No migration tooling at all. Doesn't work for apps with large persistent state.

### Key insight

The more packaging and automation the app already has, the less Velero is needed for manifests. Velero's unique value is **PVC data migration** — for everything else, the app's existing deployment method (Helm, GitOps, operator, pipeline) handles environment adaptation better because it was designed for it. Velero was designed for backup/restore, not cross-environment deployment.

```
No packaging       → Velero does everything (demonstrated in this experiment)
Helm chart         → Helm for manifests + Velero for data
GitOps / Operator  → Existing tooling for manifests + Velero for data
Stateless app      → Just redeploy, no migration needed
```

## Appendix: Crane for GitOps

If the goal is to extract clean manifests for version control (separately from the migration), [Crane](https://github.com/migtools/crane) can be used independently:

```bash
crane export -n shift-week-demo
crane transform
crane apply
# Clean YAML in output/output.yaml — no runtime metadata, suitable for Git
```

Crane only exports namespaced resources. CRDs, ClusterRoles, and Namespaces must be exported manually with `oc get <resource> -o yaml`.

## References

- [Velero — Restore Resource Modifiers](https://velero.io/docs/main/restore-resource-modifiers/)
- [Velero — File System Backup](https://velero.io/docs/v1.15/file-system-backup/)
- [Velero — Node Agent ConfigMap](https://velero.io/docs/main/supported-configmaps/node-agent-configmap/)
- [Velero — AWS Plugin](https://github.com/vmware-tanzu/velero-plugin-for-aws)
- [OpenShift Velero Plugin](https://github.com/openshift/openshift-velero-plugin)
- [Crane — GitHub](https://github.com/migtools/crane)
- [Crane — Red Hat Cloud Experts](https://cloud.redhat.com/experts/redhat/crane/)
