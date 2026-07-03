# Shift Week: MicroShift to OpenShift Transitions - Presentation

> Full reference doc: [shift-week-microshift-to-openshift.md](shift-week-microshift-to-openshift.md)

## The problem

MicroShift and SNO are different platforms - different OS (RHEL vs RHCOS), different control plane (embedded binary vs operator-managed pods), different lifecycle (rpm-ostree vs MCO). There is no upgrade path between them.

When a workload outgrows MicroShift, the **application** must be migrated. The platform stays behind.

## What makes migration hard?

It depends on the application:

| Dimension | Easy | Hard |
|---|---|---|
| **State** | Stateless - just redeploy | Persistent PVCs - must migrate data |
| **Complexity** | Deployment + Service | CRDs, ClusterRoles, multi-namespace |
| **Packaging** | Helm / GitOps / Operator | Raw manifests - no environment adaptation |

Our demo app is the hard case: **stateful + complex + raw manifests**.

## What tool?

We evaluated **Crane** (Konveyor) and **Velero**.

**Crane failed** - can't migrate PVC data, can't export CRDs, and is incompatible with Velero's data restore (pod name mismatch causes deadlock).

**Velero works** - migrates everything: namespaced resources, cluster-scoped resources (CRDs, ClusterRoles), and PVC data via Kopia file-level backup through S3.

## Gotchas discovered

| Problem | What happens | Fix |
|---|---|---|
| **Pod CIDR mismatch** | OVN-K rejects source IPs, pod can't start, data restore deadlocks | Velero resource modifier strips `k8s.ovn.org/pod-networks` |
| **StorageClass names** | MicroShift: `topolvm-provisioner`, SNO: `lvms-vg1`. PVC stays Pending | Create StorageClass alias on SNO |
| **Stale pods** | Velero injects `restore-wait` init container, app doesn't start | Delete pods after restore, Deployment recreates clean ones |
| **Route hostname** | Restored Route has MicroShift domain | `oc patch` to remove `spec.host` |
| **Velero on MicroShift** | CRI-O rejects short image names, data mover needs privileged access | `docker.io/` prefix, `privilegedFsBackup` ConfigMap |

## Demo

> See [demo script](shift-week-demo.sh)

**Pre-staged** (done before the presentation):
- App deployed on MicroShift with test notes
- Velero installed on both clusters
- Velero backup completed on MicroShift
- SNO prepared: LVMS installed, StorageClass alias created, resource modifier ConfigMap ready

**Live**:

1. Show the app on MicroShift — notes exist
2. Run Velero restore on SNO
3. Delete pods, patch Route
4. Show the app on SNO — same notes

## The migration in one slide

```
MicroShift                          SNO
┌──────────┐   velero backup   ┌──────────┐
│ App      │ ───────────────── │          │
│ CRDs     │        S3         │          │
│ PVC data │ ◄───────────────► │          │
└──────────┘   velero restore  │ App      │
                + resource     │ CRDs     │
                  modifier     │ PVC data │
               + delete pods   └──────────┘
               + patch route
```

## For bigger apps

The more packaging the app has, the less Velero is needed for manifests:

```
No packaging  → Velero for everything (this demo)
Helm chart    → Helm for manifests + Velero for data only
GitOps        → ArgoCD/Flux for manifests + Velero for data only
Operator      → Install operator + apply CR (may handle data too)
Stateless     → Just redeploy
```

Velero's unique value is **PVC data migration**. For manifests, use whatever the app already has.

## Key takeaways

1. MicroShift → SNO migration is feasible with Velero
2. The hard parts are platform differences (pod CIDR, StorageClass names), not API incompatibility
3. SCCs, NetworkPolicies, Routes, RBAC all transfer cleanly
4. Velero on MicroShift needs extra config not documented upstream
5. For production apps: invest in Helm/GitOps packaging, use Velero only for data
