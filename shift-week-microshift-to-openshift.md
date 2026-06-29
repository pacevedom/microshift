# Shift Week: MicroShift to OpenShift Transitions

## Objective

Evaluate the feasibility of migrating a user application from MicroShift to Single Node OpenShift (SNO), document what transfers cleanly, what requires manual intervention, and what gaps exist.

MicroShift → SNO is an "upward" transition: SNO is a superset of MicroShift in API surface and platform capabilities.

## Tooling

**[Crane](https://github.com/migtools/crane)** (Konveyor, CNCF Sandbox) — extracts live resource manifests, strips runtime metadata (`uid`, `resourceVersion`, `creationTimestamp`, `managedFields`), produces clean redeployable YAML. Namespace-scoped only. Does NOT migrate PVC data or cluster-scoped resources.

**[Velero](https://velero.io/)** — backs up and restores Kubernetes resources AND persistent volume data via CSI snapshots or file-level copy (Kopia). Captures everything including cluster-scoped resources. Requires S3-compatible object storage.

| Concern | Crane | Velero |
|---|---|---|
| Namespaced resource manifests | Exports + cleans metadata | Backs up + restores (keeps runtime metadata) |
| Cluster-scoped resources (CRDs, ClusterRoles) | Not supported | Supported |
| PVC data | Not supported | CSI snapshots or Kopia file-level copy |
| Clean YAML for GitOps | Yes — main value | No — restores with runtime cruft |

### Why both tools? Neither alone is sufficient.

**Velero alone** backs up everything (manifests, cluster-scoped resources, PVC data), but restores raw runtime state — including pods with injected init containers (`restore-wait`), stale node assignments, and old Route hostnames. In this experiment, a pure Velero restore produced pods that never started the application and Routes that returned 404.

**Crane alone** produces clean, adjustable manifests that deploy correctly on the target, but cannot migrate PVC data or cluster-scoped resources (CRDs, ClusterRoles, ClusterRoleBindings).

**Crane + Velero together**: Crane handles manifest quality (clean, adjustable YAML). Velero handles completeness (cluster-scoped resources, PVC data, anything Crane missed). With `--existing-resource-policy update`, Velero backfills what Crane can't export without overwriting what Crane already created cleanly.

### Restore ordering is critical

The sequence on the target cluster must be:

```
1. Velero restore — cluster-scoped resources only (CRDs, ClusterRoles)
   Safe because these are just definitions — no runtime state, no init containers.
   Must exist before step 2, as CR instances depend on their CRDs.

2. Crane apply — clean namespaced manifests
   Creates Deployments, Services, Routes, etc. with correct specs.
   The Deployment controller creates clean pods without Velero artifacts.

3. Velero full restore (--existing-resource-policy update)
   Restores PVC data via Kopia FSB (triggered by the Pod resource in the backup).
   Skips resources Crane already created (update policy).
   Backfills anything Crane missed (auto-generated ConfigMaps, EndpointSlices, etc.).

4. Delete pods — so they restart clean with restored data
   The Deployment recreates pods from the clean spec (step 2),
   and they mount the PVC with restored data (step 3).
```

**If Velero runs before Crane** (steps 2 and 3 reversed), Velero recreates raw pods from the backup with injected `restore-wait` init containers and stale runtime state. The Deployment sees a matching spec and does not trigger a new rollout, leaving broken pods running. This was observed during the experiment and caused the app to return 404 until the pods were manually deleted.

## MicroShift → SNO: what to know

**Transfers cleanly**: SCCs (identical on both — verified in source), NetworkPolicies, Routes (same OVN-K), namespaced RBAC, API compatibility (SNO has all MicroShift APIs and more).

**Requires adjustment**:
- **StorageClass**: MicroShift uses `topolvm-provisioner` (LVMS). Install LVMS on SNO via OLM, or change the StorageClass in PVC manifests.
- **Route hostnames**: exported Routes carry the MicroShift domain. Remove `spec.host` to let SNO auto-assign, or update manually.
- **CRDs/Operators**: install via OperatorHub on SNO before applying CR instances.
- **Image pull secrets**: node-level pull secret location differs (`/etc/crio/openshift-pull-secret` vs `openshift-config`). Namespace-scoped secrets transfer fine.
- **Host-level deps**: hostPath mounts, firewall rules, SELinux labels, kernel modules — invisible to both tools.

## Tool Installation

### Crane

```bash
git clone https://github.com/migtools/crane.git /tmp/crane
cd /tmp/crane && go build -o crane main.go && sudo mv crane /usr/local/bin/
crane --help
```

Latest release: v0.10.0-alpha.1 (alpha quality).

### Velero

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

Minimal notes API in `shift-week/sample-app/` exercising all migration-relevant resource types. Python HTTP server, no custom image build required beyond `python:3-alpine`.

| Resource | File |
|---|---|
| Namespace | `namespace.yaml` |
| ConfigMap | `configmap.yaml` |
| Secret | `secret.yaml` |
| PVC (LVMS) | `pvc.yaml` |
| ServiceAccount | `serviceaccount.yaml` |
| Role + RoleBinding | `rbac.yaml` |
| Deployment | `deployment.yaml` |
| Service | `service.yaml` |
| Route (edge TLS) | `route.yaml` |
| NetworkPolicy | `networkpolicy.yaml` |

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

### Step 2: Export manifests with Crane

```bash
mkdir -p ~/shift-week-migration && cd ~/shift-week-migration
crane export -n shift-week-demo
crane transform
crane apply
```

Review `output/output.yaml` — verify all resources are present and runtime metadata is stripped.

### Step 3: Back up PVC data with Velero

Install Velero on MicroShift (see [Tool Installation](#tool-installation)), then:

```bash
velero backup create shift-week-backup --include-namespaces shift-week-demo --wait
velero backup describe shift-week-backup --details
```

Kopia can only back up volumes mounted by a running pod — keep the app running during backup.

### Step 4: Prepare SNO

```bash
export KUBECONFIG=<path-to-sno-kubeconfig>

# Check storage — install LVMS via OperatorHub if needed, or note the available StorageClass
oc get storageclass

# Verify image pull works
oc run test-pull --image=quay.io/pacevedo/shift-week-notes:latest --restart=Never -n default
oc delete pod test-pull -n default
```

### Step 5: Adjust manifests for SNO

```bash
cp output/output.yaml output/output-sno.yaml
```

Edit `output/output-sno.yaml`:
- **StorageClass**: change `topolvm-provisioner` if SNO uses a different one
- **Route hostname**: remove `spec.host` to let SNO auto-assign

### Step 6: Apply on SNO

Install Velero on SNO pointing to the same S3 bucket (see [Tool Installation](#tool-installation)). Then follow the three-phase restore sequence (see [Restore ordering is critical](#restore-ordering-is-critical) for why this order matters):

```bash
# Phase 1: Restore cluster-scoped resources from Velero
# (CRDs, ClusterRoles — must exist before CR instances can be applied)
# For our sample app this is a no-op, but for apps with custom CRDs:
velero restore create shift-week-cluster-scoped \
  --from-backup shift-week-backup \
  --include-resources customresourcedefinitions,clusterroles,clusterrolebindings \
  --existing-resource-policy update \
  --wait

# Phase 2: Apply cleaned namespaced manifests from Crane
oc apply -f output/output-sno.yaml

# Phase 3: Full Velero restore for PVC data and anything Crane missed
# Do NOT use --include-resources here — FSB data restore is triggered by
# restoring the Pod resource. Filtering to only PVCs/PVs skips the data.
velero restore create shift-week-restore \
  --from-backup shift-week-backup \
  --existing-resource-policy update \
  --wait

# Verify the restore includes Pod Volume Restores
velero restore describe shift-week-restore --details

# Restart pods so they pick up restored data with clean specs
oc delete pod -l app=demo-app -n shift-week-demo
oc wait --for=condition=ready pod -l app=demo-app -n shift-week-demo --timeout=120s
```

### Step 7: Validate

```bash
ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
NODE_IP=<sno-node-ip>

# Check resources
oc get deploy,svc,route,pvc,cm,secret,networkpolicy -n shift-week-demo

# Test the app
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" https://$ROUTE | python3 -m json.tool | tee after-migration.json

# Post a new note to verify full functionality
curl -sk --connect-to "$ROUTE:443:$NODE_IP:443" -X POST -d "first note on SNO" https://$ROUTE
```

### Expected results

| Aspect | Crane only | Crane + Velero |
|---|---|---|
| App config (title, message, token) | Identical | Identical |
| Notes | Empty (data not migrated) | Present (data restored) |
| Route | Working, different hostname | Working, different hostname |
| NetworkPolicy | Working | Working |
| RBAC / SCCs | Working | Working |

## Assessment

### Key finding

Neither Crane nor Velero alone is sufficient for a clean MicroShift → SNO migration. Both are needed, and the order they run in is critical:

- **Crane** provides manifest quality — clean, adjustable YAML without runtime metadata. This is what makes the manifests work on the target cluster (correct pod specs, adjustable StorageClass and Route hostnames).
- **Velero** provides completeness — cluster-scoped resources (CRDs, ClusterRoles), PVC data, and auto-generated resources that Crane can't export.
- **Ordering** — Crane manifests must be applied before the full Velero restore. Reversing this produces broken pods with stale runtime state from the source cluster.

### What each tool covers

| Layer | Crane | Velero | Manual |
|---|---|---|---|
| Namespaced resources (Deployments, Services, Routes, etc.) | Clean export | Backup + restore (with runtime cruft) | |
| Cluster-scoped resources (CRDs, ClusterRoles) | | Backup + restore | Operators via OperatorHub |
| PVC data | | Kopia FSB or CSI snapshots | |
| Manifest adjustments (StorageClass, Route hostname) | Editable output | | Review + edit |
| Host-level deps (firewall, SELinux, mounts) | | | Fully manual |

### Lessons learned from the experiment

1. **Velero on MicroShift requires extra configuration**: fully qualified image names (`docker.io/...`), `privilegedFsBackup: true` in a ConfigMap referenced at install time, and privileged SCC grants. None of this is documented for MicroShift specifically.
2. **Velero's `--include-resources` breaks FSB data restores**: filtering to only PVCs/PVs restores the manifests but skips the Kopia file-level data, because FSB restore is triggered by restoring the Pod resource.
3. **Restore ordering matters**: Velero before Crane produces pods with injected `restore-wait` init containers that don't start the application. Crane before Velero produces clean pods that work correctly.
4. **SCCs are identical**: MicroShift ships the full set of OpenShift SCCs. This is not a migration concern.
5. **The MicroShift → SNO direction is favorable**: SNO is a superset of MicroShift in API surface. No missing APIs, no missing SCCs, no missing NetworkPolicy support. The main practical traps are StorageClass mismatches and missing operators/CRDs on the target.

## References

- [Crane — GitHub](https://github.com/migtools/crane)
- [Velero — File System Backup](https://velero.io/docs/v1.15/file-system-backup/)
- [Velero — AWS Plugin](https://github.com/vmware-tanzu/velero-plugin-for-aws)
- [Crane — Red Hat Cloud Experts](https://cloud.redhat.com/experts/redhat/crane/)
- [OpenShift Velero Plugin](https://github.com/openshift/openshift-velero-plugin)
