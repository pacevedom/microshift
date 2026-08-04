# Shift Week: MicroShift to OpenShift Transitions - Presentation

> Full reference doc: [shift-week-microshift-to-openshift.md](shift-week-microshift-to-openshift.md)

## The problem

MicroShift and SNO are different platforms - different OS (RHEL vs RHCOS), different control plane (embedded binary vs operator-managed pods), different lifecycle (rpm-ostree/bootc vs MCO). There is no upgrade path between them.

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

**[Crane](https://github.com/migtools/crane)** (Konveyor, CNCF Sandbox) was evaluated first but **failed as a migration tool**: no PVC data migration, no cluster-scoped resources (CRDs, ClusterRoles), and incompatible with Velero's data restore — Crane-created pods have different names than the backup, causing Velero's Kopia restore to deadlock. Every ordering was tried (Crane before/between/after Velero phases); none worked. Crane remains useful for **extracting clean YAML for GitOps onboarding**, not migration.

**[Velero](https://velero.io/)** works — migrates everything: namespaced resources, cluster-scoped resources (CRDs, ClusterRoles), and PVC data via Kopia file-level backup through S3.

## Gotchas discovered

| Problem | What happens | Fix |
|---|---|---|
| **Pod CIDR mismatch** | OVN-K rejects source IPs, pod can't start, data restore deadlocks | Velero resource modifier strips `k8s.ovn.org/pod-networks` |
| **StorageClass names** | MicroShift: `topolvm-provisioner`, SNO: `lvms-vg1`. PVC stays Pending | Create StorageClass alias on SNO |
| **Stale pods** | Velero injects `restore-wait` init container, app doesn't start | Delete pods after restore, Deployment recreates clean ones |
| **Route hostname** | Restored Route has MicroShift domain | `oc patch` to remove `spec.host` |
| **Velero on MicroShift** | CRI-O rejects short image names, data mover needs privileged access | `docker.io/` prefix, `privilegedFsBackup` ConfigMap |

## Without cloud S3

Velero needs an object store accessible from both clusters. No AWS/GCP/Azure? Use a **self-hosted S3-compatible server** backed by local or NFS storage.

```
┌─────────────┐       S3 API        ┌──────────────────┐
│ MicroShift  │ ──── backup ──────► │ S3-compatible    │
└─────────────┘                     │ server           │
┌─────────────┐       S3 API        │ (NFS/local disk) │
│    SNO      │ ◄─── restore ────── │                  │
└─────────────┘                     └──────────────────┘
```

Options: **SeaweedFS** (Apache 2.0, single container), **CloudServer/Zenko** (Apache 2.0), or any S3-compatible endpoint. Backend can be NFS mount or local disk.

Velero install is identical — only `--backup-location-config` changes:

```bash
--backup-location-config region=default,s3ForcePathStyle=true,s3Url=http://<s3-host>:8333
```

## Migration evidence

### App on MicroShift (before migration)

```
$ curl -sk --connect-to "$ROUTE:443:$MICROSHIFT_IP:443" https://$ROUTE | python3 -m json.tool
{
    "app": "Shift Week Demo",
    "message": "Notes API running on MicroShift",
    "token": "shift-week-demo-token-2026",
    "config": {
        "maxNotes": 10,
        "retentionDays": 7
    },
    "notes": [
        {
            "id": 1,
            "text": "test note from make",
            "createdAt": "2026-08-04T09:11:41.875566+00:00",
            "expiresAt": "2026-08-11T09:11:41.875566+00:00"
        },
        {
            "id": 2,
            "text": "note before migration - 1",
            "createdAt": "2026-08-04T09:12:40.353645+00:00",
            "expiresAt": "2026-08-11T09:12:40.353645+00:00"
        },
        {
            "id": 3,
            "text": "note before migration - 2",
            "createdAt": "2026-08-04T09:12:49.669979+00:00",
            "expiresAt": "2026-08-11T09:12:49.669979+00:00"
        }
    ]
}

```

### Velero backup (13 seconds)

```
$ velero backup create shift-week-backup --include-namespaces shift-week-demo --wait
Backup completed with status: Completed.

$ velero backup describe shift-week-backup --details
Phase:  Completed

Started:    2026-08-04 11:19:29 +0200 CEST
Completed:  2026-08-04 11:19:42 +0200 CEST

Total items to be backed up:  36
Items backed up:              36

Resource List:  (key resources — full list has 36 items)
  apiextensions.k8s.io/v1/CustomResourceDefinition:
    - noteconfigs.shift-week.example.com
  apps/v1/Deployment:
    - shift-week-demo/demo-app
  networking.k8s.io/v1/NetworkPolicy:
    - shift-week-demo/demo-app-allow-router-only
  rbac.authorization.k8s.io/v1/ClusterRole:
    - demo-app-cluster-role
  rbac.authorization.k8s.io/v1/ClusterRoleBinding:
    - demo-app-cluster-rolebinding
  route.openshift.io/v1/Route:
    - shift-week-demo/demo-app
  shift-week.example.com/v1/NoteConfig:
    - shift-week-demo/default
  v1/PersistentVolumeClaim:
    - shift-week-demo/demo-app-data
  v1/Secret:
    - shift-week-demo/demo-app-secret
  v1/Service:
    - shift-week-demo/demo-app

  Pod Volume Backups - kopia:
    Completed:
      shift-week-demo/demo-app-7956ff87c6-4qzhv: data (size: 432)
```

### Velero restore on SNO (17 seconds)

```
$ velero restore create shift-week-restore \
    --from-backup shift-week-backup \
    --resource-modifier-configmap strip-network-annotations \
    --wait
Restore completed with status: Completed.

$ velero restore describe shift-week-restore --details
Phase:                       Completed
Total items to be restored:  24
Items restored:              24

Started:    2026-08-04 11:48:38 +0200 CEST
Completed:  2026-08-04 11:48:55 +0200 CEST

Resource modifier:  strip-network-annotations

kopia Restores:
  Completed:
    shift-week-demo/demo-app-7956ff87c6-4qzhv: data (size: 432)

Resource List:  (key resources)
  CustomResourceDefinition:  noteconfigs.shift-week.example.com(created)
  Deployment:                shift-week-demo/demo-app(created)
  NetworkPolicy:             shift-week-demo/demo-app-allow-router-only(created)
  ClusterRole:               demo-app-cluster-role(created)
  ClusterRoleBinding:        demo-app-cluster-rolebinding(created)
  Route:                     shift-week-demo/demo-app(created)
  NoteConfig:                shift-week-demo/default(created)
  PersistentVolumeClaim:     shift-week-demo/demo-app-data(created)
  Secret:                    shift-week-demo/demo-app-secret(created)
  Service:                   shift-week-demo/demo-app(created)

Warnings:
  shift-week-demo:  kube-root-ca.crt already exists
                    openshift-service-ca.crt already exists
```

> **Note**: `kube-root-ca.crt` and `openshift-service-ca.crt` warnings are expected — these are auto-generated per cluster by Kubernetes and OpenShift. Velero correctly skipped them.

### Post-restore fixes

```
<!-- PASTE: oc delete pod -l app=demo-app -n shift-week-demo
     and:  oc wait --for=condition=ready pod -l app=demo-app -n shift-week-demo
     and:  oc patch route demo-app -n shift-week-demo --type=json \
             -p '[{"op":"remove","path":"/spec/host"}]' -->
```

### Resources on SNO after restore

```
$ oc get deploy,svc,route,pvc,cm,secret,networkpolicy,noteconfigs -n shift-week-demo
NAME                       READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/demo-app   1/1     1            1           2m6s

NAME               TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)    AGE
service/demo-app   ClusterIP   172.30.23.126   <none>        8080/TCP   2m6s

NAME                                HOST/PORT                                   PATH   SERVICES   PORT   TERMINATION     WILDCARD
route.route.openshift.io/demo-app   demo-app-shift-week-demo.apps.example.com          demo-app   8080   edge/Redirect   None

NAME                                  STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS          VOLUMEATTRIBUTESCLASS   AGE
persistentvolumeclaim/demo-app-data   Bound    pvc-7367d1dc-8344-46a4-86e6-e2d02396c335   1Gi        RWO            topolvm-provisioner   <unset>                 2m7s

NAME                                 DATA   AGE
configmap/demo-app-config            2      2m7s
configmap/kube-root-ca.crt           1      2m7s
configmap/openshift-service-ca.crt   1      2m7s

NAME                              TYPE                      DATA   AGE
secret/builder-dockercfg-rnj2k    kubernetes.io/dockercfg   1      2m7s
secret/default-dockercfg-2h2d7    kubernetes.io/dockercfg   1      2m7s
secret/demo-app-dockercfg-bmr92   kubernetes.io/dockercfg   1      2m7s
secret/demo-app-secret            Opaque                    1      2m7s
secret/deployer-dockercfg-jn7q7   kubernetes.io/dockercfg   1      2m7s

NAME                                                         POD-SELECTOR   AGE
networkpolicy.networking.k8s.io/demo-app-allow-router-only   app=demo-app   2m6s

NAME                                        AGE
noteconfig.shift-week.example.com/default   2m7s
```

```
$ oc get crd noteconfigs.shift-week.example.com
NAME                                 CREATED AT
noteconfigs.shift-week.example.com   2026-08-04T09:48:39Z
```

### App on SNO (after migration)

> Note: `"message": "Notes API running on MicroShift"` — this comes from the migrated ConfigMap, proving it transferred faithfully. In a real migration you'd update it.

```
$ curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" https://$ROUTE | python3 -m json.tool
{
    "app": "Shift Week Demo",
    "message": "Notes API running on MicroShift",
    "token": "shift-week-demo-token-2026",
    "config": {
        "maxNotes": 10,
        "retentionDays": 7
    },
    "notes": [
        {
            "id": 1,
            "text": "test note from make",
            "createdAt": "2026-08-04T09:11:41.875566+00:00",
            "expiresAt": "2026-08-11T09:11:41.875566+00:00"
        },
        {
            "id": 2,
            "text": "note before migration - 1",
            "createdAt": "2026-08-04T09:12:40.353645+00:00",
            "expiresAt": "2026-08-11T09:12:40.353645+00:00"
        },
        {
            "id": 3,
            "text": "note before migration - 2",
            "createdAt": "2026-08-04T09:12:49.669979+00:00",
            "expiresAt": "2026-08-11T09:12:49.669979+00:00"
        }
    ]
}
```

### New note on SNO (post-migration write)

```
$ curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" -X POST -d "first note on SNO" https://$ROUTE
$ curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" https://$ROUTE | python3 -m json.tool
{
    "app": "Shift Week Demo",
    "message": "Notes API running on MicroShift",
    "token": "shift-week-demo-token-2026",
    "config": {
        "maxNotes": 10,
        "retentionDays": 7
    },
    "notes": [
        {
            "id": 1,
            "text": "test note from make",
            "createdAt": "2026-08-04T09:11:41.875566+00:00",
            "expiresAt": "2026-08-11T09:11:41.875566+00:00"
        },
        {
            "id": 2,
            "text": "note before migration - 1",
            "createdAt": "2026-08-04T09:12:40.353645+00:00",
            "expiresAt": "2026-08-11T09:12:40.353645+00:00"
        },
        {
            "id": 3,
            "text": "note before migration - 2",
            "createdAt": "2026-08-04T09:12:49.669979+00:00",
            "expiresAt": "2026-08-11T09:12:49.669979+00:00"
        },
        {
            "id": 4,
            "text": "first note on SNO",
            "createdAt": "2026-08-04T09:58:12.873192+00:00",
            "expiresAt": "2026-08-11T09:58:12.873192+00:00"
        }
    ]
}
```

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

The more packaging the app has, the less Velero is needed — and the fewer gotchas you hit:

| Packaging | Manifests | Data | Gotchas avoided |
|---|---|---|---|
| **Raw manifests** (this demo) | Velero | Velero (Kopia) | None — all gotchas apply |
| **Helm** | `helm install` with target values | Velero (Kopia) | Pod CIDR, StorageClass, Route hostname, stale pods — fresh render avoids all |
| **Kustomize / GitOps** | Sync from Git with target overlay | Velero (Kopia) | Same as Helm — environment-aware manifests |
| **Operator** | Install operator + apply CR | Operator (some handle data internally) | Potentially no Velero at all |
| **Stateless** | Redeploy from pipeline | No data to migrate | No migration tooling needed |

Velero's unique value is **PVC data migration**. For manifests, the app's existing packaging handles environment adaptation better than Velero's backup/restore — which was designed for same-cluster recovery, not cross-environment deployment.

### Multi-namespace apps

This demo uses a single namespace (`shift-week-demo`). Production apps often span multiple namespaces (e.g. app, database, monitoring, shared infrastructure). Considerations:

- **Velero `--include-namespaces`** accepts a comma-separated list: `--include-namespaces ns1,ns2,ns3`. All namespaces are backed up/restored together.
- **Cross-namespace references** (e.g. a NetworkPolicy referencing pods in another namespace, or a ClusterRoleBinding referencing a ServiceAccount in a different namespace) restore correctly — Velero handles cluster-scoped and namespaced resources in a single pass.
- **Ordering matters for operators**: if namespace B depends on a CRD installed by an operator in namespace A, install the operator first, then restore. Velero restores CRD definitions but not the operator that manages them.
- **Shared namespaces** (e.g. `openshift-ingress`, `openshift-storage`) already exist on SNO — Velero skips them. Only app-owned namespaces should be in the backup scope.
- **Helm/GitOps simplifies this**: each namespace gets its own chart/overlay. No need to coordinate a single Velero backup across all of them — deploy manifests per-namespace, use Velero only for PVC data in namespaces that have it.

## Key takeaways

1. MicroShift → SNO migration is feasible with Velero
2. The hard parts are platform differences (pod CIDR, StorageClass names), not API incompatibility
3. SCCs, NetworkPolicies, Routes, RBAC all transfer cleanly
4. Velero on MicroShift needs extra config not documented upstream: fully qualified image names (`docker.io/...`), `privilegedFsBackup` ConfigMap, and privileged SCC grants
5. For production apps: invest in Helm/GitOps packaging, use Velero only for data
