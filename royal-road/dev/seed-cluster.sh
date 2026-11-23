#!/usr/bin/env bash
# Seed a cluster so it stands in for the production cluster described in
# FORK.md ("Verification environment"):
#   - the only OpenShift CRD production has: helmchartrepositories.helm.openshift.io
#   - Rook CRDs (no operator), so storage-related flags behave as in production
#   - the prometheus-operator Prometheus CRD, which production also has
#   - a few Helm releases with several upgrades each, so superseded revisions exist
#   - a cluster-admin ServiceAccount for running the bridge off-cluster
#
# Usage: royal-road/dev/seed-cluster.sh <kubeconfig>
# kind-up.sh and container-cluster-up.sh run it. It uses only the given
# kubeconfig, never $KUBECONFIG or ~/.kube/config. Re-running it finishes a
# partial seed and leaves a complete one unchanged.
#
# Extra CRDs from royal-road/deploy/crds/ are NOT installed here; work packages
# that add them apply them explicitly so "with and without" can be compared.
set -euo pipefail

if [ $# -lt 1 ] || [ ! -f "$1" ]; then
  echo "usage: $0 <kubeconfig>" >&2
  exit 2
fi
export KUBECONFIG=$1

# openshift/api commit the console vendors (go.mod replace directive).
OPENSHIFT_API_SHA=6ab113cb736829b0cb17174b9b26d355e4cda3a0
ROOK_VERSION=v1.21.2
PROMETHEUS_OPERATOR_VERSION=v0.86.2
HELM_RELEASES=${HELM_RELEASES:-6}   # releases per namespace
HELM_UPGRADES=${HELM_UPGRADES:-5}   # upgrades per release

kubectl cluster-info >/dev/null

kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/openshift/api/${OPENSHIFT_API_SHA}/helm/v1beta1/zz_generated.crd-manifests/00_helmchartrepositories.crd.yaml"
kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/rook/rook/${ROOK_VERSION}/deploy/examples/crds.yaml"
# Production runs prometheus-operator; the console's PROMETHEUS flag (which
# gates the Overview dashboard) checks for this CRD.
kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${PROMETHEUS_OPERATOR_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_prometheuses.yaml"

# Cluster-admin identity for the bridge (BRIDGE_USER_AUTH=disabled).
kubectl -n kube-system get sa console-dev >/dev/null 2>&1 ||
  kubectl -n kube-system create sa console-dev
kubectl get clusterrolebinding console-dev >/dev/null 2>&1 ||
  kubectl create clusterrolebinding console-dev --clusterrole=cluster-admin \
    --serviceaccount=kube-system:console-dev

# Helm releases with superseded revisions, from a chart generated locally (no
# network chart repo needed). Each release ends at revision 1 + HELM_UPGRADES.
# A release that is missing or behind gets only the revisions it lacks.
chartdir=$(mktemp -d)
trap 'rm -rf "$chartdir"' EXIT
helm create "$chartdir/demo" >/dev/null
for ns in helm-a helm-b helm-c; do
  for i in $(seq 0 $((HELM_RELEASES - 1))); do
    revision=$(helm history "demo-$i" -n "$ns" --max 1 2>/dev/null |
      awk 'NR == 2 { print $1 }') || revision=0
    for ((r = ${revision:-0}; r <= HELM_UPGRADES; r++)); do
      # replicaCount=0: no pods, so the cluster stays light.
      helm upgrade --install "demo-$i" "$chartdir/demo" -n "$ns" --create-namespace \
        --set replicaCount=0 --set podAnnotations.rev="r$r" >/dev/null
    done
  done
done

echo "Helm release secrets: $(kubectl get secrets -A -l owner=helm --no-headers | wc -l)"
