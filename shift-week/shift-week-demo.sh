#!/bin/bash
set -euo pipefail

# Shift Week Demo Script
#
# Pre-requisites (done before the presentation):
#   - App deployed on MicroShift with test notes
#   - Velero installed on both clusters (with ConfigMap + SCC grants)
#   - Velero backup completed on MicroShift
#   - SNO prepared: LVMS + StorageClass alias + resource modifier ConfigMap
#   - Velero restore already completed on SNO
#
# Usage: run each PART manually during the demo, not as a single script
#
# Parameters:
#   MICROSHIFT_IP - MicroShift node IP address
#   SNO_IP        - SNO node IP address
#   MICROSHIFT_KUBECONFIG - path to MicroShift kubeconfig (default: ~/.kube/microshift)
#   SNO_KUBECONFIG        - path to SNO kubeconfig (default: ~/.kube/sno)

MICROSHIFT_IP="${MICROSHIFT_IP:?Set MICROSHIFT_IP before running}"
SNO_IP="${SNO_IP:?Set SNO_IP before running}"
MICROSHIFT_KUBECONFIG="${MICROSHIFT_KUBECONFIG:-$HOME/.kube/microshift}"
SNO_KUBECONFIG="${SNO_KUBECONFIG:-$HOME/.kube/sno}"

# ============================================================
# PART 1: Show the app on MicroShift (~30s)
# ============================================================
export KUBECONFIG=$MICROSHIFT_KUBECONFIG
echo "=== Velero on MicroShift ==="
oc get pod -n velero
velero backup-location get

echo "=== App on MicroShift ==="
export KUBECONFIG=$MICROSHIFT_KUBECONFIG

ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
echo "Route: $ROUTE"

echo ""
echo "--- Notes on MicroShift ---"
curl -sk --connect-to "$ROUTE:443:$MICROSHIFT_IP:443" https://$ROUTE | python3 -m json.tool

# ============================================================
# PART 2: Show the restore result on SNO (~30s)
# ============================================================

echo ""
echo "=== Velero restore on SNO (pre-staged) ==="
export KUBECONFIG=$SNO_KUBECONFIG

velero backup get
velero restore describe shift-week-restore --details | grep -E "Phase|kopia"

# ============================================================
# PART 3: Clean up and patch (~30s)
# ============================================================

echo ""
echo "=== Cleaning up stale pods ==="

oc delete pod -l app=demo-app -n shift-week-demo
oc wait --for=condition=ready pod -l app=demo-app -n shift-week-demo --timeout=120s

oc patch route demo-app -n shift-week-demo --type=json \
  -p '[{"op":"remove","path":"/spec/host"}]'

# ============================================================
# PART 4: Validate on SNO (~30s)
# ============================================================

echo ""
echo "=== App on SNO ==="

ROUTE=$(oc get route demo-app -n shift-week-demo -o jsonpath='{.spec.host}')
echo "Route: $ROUTE"

echo ""
echo "--- Notes on SNO (should match MicroShift) ---"
curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" https://$ROUTE | python3 -m json.tool

echo ""
echo "--- Posting a new note on SNO ---"
curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" -X POST -d "first note on SNO after migration" https://$ROUTE

echo ""
echo "--- Final state ---"
curl -sk --connect-to "$ROUTE:443:$SNO_IP:443" https://$ROUTE | python3 -m json.tool
