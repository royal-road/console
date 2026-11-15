#!/usr/bin/env bash
# Print BRIDGE_PLUGIN_PROXY with the trace federator route added to an
# existing value (pass it as $1, or nothing to start empty):
#   /api/proxy/plugin/distributed-tracing-console-plugin/backend/ -> trace-federator
# with the user's token (authorize: true). Plain HTTP inside the cluster.
#
#   current=$(kubectl -n <console-ns> get deploy <console> -o json |
#     jq -r '.spec.template.spec.containers[0].env[] | select(.name=="BRIDGE_PLUGIN_PROXY") | .value')
#   royal-road/deploy/tracing/plugin-proxy.sh "$current"
#
# Needs jq.
set -euo pipefail
ns=${TRACING_NAMESPACE:-tracing}
current=${1:-}
[[ -n $current ]] || current='{"services":[]}'
jq -c --arg ep "http://trace-federator.$ns.svc:8080" '
  .services |= ((. // []) | map(select(.consoleAPIPath != "/api/proxy/plugin/distributed-tracing-console-plugin/backend/"))
    + [{consoleAPIPath: "/api/proxy/plugin/distributed-tracing-console-plugin/backend/", endpoint: $ep, authorize: true}])
' <<<"$current"
