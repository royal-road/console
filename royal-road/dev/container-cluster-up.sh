#!/usr/bin/env bash
# Create a cluster with Apple's `container cluster` that stands in for the
# production cluster described in FORK.md ("Verification environment"), and
# seed it with seed-cluster.sh. It runs kubeadm in a lightweight VM on the
# plugin's default kindest/node image (v1.35.5 in apple/container 1.7.1; kind's
# v1.36.4 and v1.37.0 images fail kubeadm init there). kind-up.sh does the
# same with kind.
#
# Usage: royal-road/dev/container-cluster-up.sh [name]
# Needs macOS on Apple silicon, apple/container 1.7.1+ with its cluster plugin
# (restart its services after upgrading), kubectl, helm and jq.
# Writes the kubeconfig to
# ${CONTAINER_CLUSTER_KUBECONFIG:-/tmp/container-cluster-<name>.kubeconfig}
# and never touches ~/.kube/config.
#
# A stopped cluster (after a host reboot or `container system stop`) cannot be
# restarted, so re-running this script deletes it, creates it again and seeds
# the new one.
set -euo pipefail

NAME=${1:-rr-console}
export KUBECONFIG=${CONTAINER_CLUSTER_KUBECONFIG:-/tmp/container-cluster-${NAME}.kubeconfig}

state=$(container cluster list --format json |
  jq -r --arg name "$NAME" '.[] | select(.id == $name) | .status.state')
case $state in
  running) ;;
  "") container cluster create --name "$NAME" ;;
  stopped)
    container cluster delete --name "$NAME"
    container cluster create --name "$NAME"
    ;;
  *)
    echo "container cluster '$NAME' is $state. Delete it: container cluster delete --name $NAME" >&2
    exit 1
    ;;
esac
container cluster write-config --name "$NAME" --kubeconfig "$KUBECONFIG"
# write-config never sets current-context, so a new kubeconfig file has none.
kubectl config use-context "$NAME" >/dev/null

"$(dirname "${BASH_SOURCE[0]}")/seed-cluster.sh" "$KUBECONFIG"

echo "container cluster '$NAME' ready. KUBECONFIG=$KUBECONFIG"
