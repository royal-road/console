#!/usr/bin/env bash
# Create a kind cluster that stands in for the production cluster described in
# FORK.md ("Verification environment"), and seed it with seed-cluster.sh.
#
# Usage: royal-road/dev/kind-up.sh [name]
# Writes the kubeconfig to ${KIND_KUBECONFIG:-/tmp/kind-<name>.kubeconfig} and
# never touches ~/.kube/config. Re-running against an existing cluster is safe.
#
# Each kind node runs systemd, which stores keys in the kernel keyring of the
# user that owns Docker. Where that user is unprivileged on the host (rootless
# Docker, unprivileged LXC), the default kernel.keys.maxbytes=20000 fits only a
# few clusters; new containers then fail to start. Raise it on the host, e.g.
#   sysctl -w kernel.keys.maxkeys=20000 kernel.keys.maxbytes=2000000
set -euo pipefail

NAME=${1:-rr-console}
export KUBECONFIG=${KIND_KUBECONFIG:-/tmp/kind-${NAME}.kubeconfig}

if ! kind get clusters 2>/dev/null | grep -qx "$NAME"; then
  # KubeletInUserNamespace lets the kubelet start when Docker itself runs in a
  # user namespace (rootless Docker, unprivileged LXC), where /dev/kmsg and
  # oom_score_adj are unavailable. It is harmless elsewhere.
  kind create cluster --name "$NAME" --kubeconfig "$KUBECONFIG" --wait 180s --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  KubeletInUserNamespace: true
# Don't give every pod container its own session keyring; that eats the
# keyring quota described above.
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
      NoNewKeyring = true
EOF
else
  kind export kubeconfig --name "$NAME" --kubeconfig "$KUBECONFIG"
fi

"$(dirname "${BASH_SOURCE[0]}")/seed-cluster.sh" "$KUBECONFIG"

echo "kind cluster '$NAME' ready. KUBECONFIG=$KUBECONFIG"
