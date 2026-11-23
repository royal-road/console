# monitoring-plugin build

Builds [openshift/monitoring-plugin](https://github.com/openshift/monitoring-plugin) for the console fork (FORK.md, WP4 part B). The plugin provides the Observe pages: Alerting (alerts, silences, alerting rules), Metrics, Dashboards and Targets.

| File | Purpose |
|---|---|
| `upstream.env` | Upstream repository, branch and pinned commit. |
| `patches/` | `git apply`-able patches, applied in name order on top of the pinned commit. See [Patches](#patches). |
| `Dockerfile` | Build on public images (`node:22`, `golang:1.26`, `ubi9/ubi-minimal`). The build context is this directory; the source is fetched at `UPSTREAM_COMMIT`. |

CI: `.github/workflows/royal-road-monitoring-plugin.yml` builds on pull requests that touch this directory and pushes `ghcr.io/royal-road/monitoring-plugin:<short-sha>` and `:main` from `main`.

## Why we build it

No public image of the `release-5.0` branch exists. Upstream's own Dockerfiles pull from `registry.redhat.io` (needs a Red Hat login) or OpenShift's CI registry. `quay.io/openshift-observability-ui/monitoring-plugin` only has development tags (`latest`, `devspace-5.0`, PR builds). `quay.io/openshift/origin-monitoring-plugin` needs credentials.

## Differences from upstream's build

- Public base images instead of `registry.redhat.io/ubi9/nodejs-22` and `ubi9/go-toolset`.
- `CGO_ENABLED=0` without `GOEXPERIMENT=strictfipsruntime`. Upstream's FIPS mode needs Red Hat's OpenSSL-backed Go toolchain.
- Same runtime layout and entrypoint (`-features alerting,metrics,legacy-dashboards,targets`).

## Patches

0001 and 0002 make the plugin work with VictoriaMetrics: 0001 for a vmalert API difference, 0002 for vmagent's external label clashing with our rule labels. 0003 lets observe-plugin (console-plugins, `plugins/observe/`) link into Metrics at a time window. 0004 leaves the Perses dashboards as the only Dashboards item in the Observe menu.

| Patch | Why |
|---|---|
| `0001-treat-missing-rule-alerts-as-empty.patch` | vmalert's rules API omits `alerts` on an inactive rule; Prometheus sends an empty list. Without the patch Observe › Alerting breaks. |
| `0002-match-exported-labels-in-alert-graph.patch` | The alert details graph drops every series whose labels differ from the alert's. Our platform rules set `prometheus: openshift-monitoring/k8s` (the plugin reads that label to show Source: Platform), but vmagent labels every series with its own external label `prometheus=<namespace>/<vmagent name>`. vmalert keeps the series' value on the alert as `exported_prometheus`, so the patch accepts a series whose label matches the alert's `exported_<label>`. Without it the graph says "No datapoints found" on every platform alert whose query keeps the `prometheus` label, which is most of them. |
| `0003-read-time-window-from-query-browser-url.patch` | Metrics (`/monitoring/query-browser`, and `/dev-monitoring/ns/<ns>/metrics`) keeps its time span only in Redux, so a link can't say when to look. The patch reads optional `from` and `to` URL parameters (epoch milliseconds) into the graph's span and end time: `?query0=<PromQL>&from=<ms>&to=<ms>`. The graph then stays on that window and doesn't poll. Zooming moves the window and writes it back to the URL; choosing a span in the graph controls drops `from`/`to` and returns to a live graph ending now. Without the parameters nothing changes. The query table below the graph still shows the current values, as upstream. |
| `0004-hide-legacy-dashboards-nav-item.patch` | Removes the admin Observe › **Dashboards** menu item (legacy ConfigMap dashboards), so the menu has one Dashboards entry: Dashboards (Perses) from `monitoring-console-plugin`. The route `/monitoring/dashboards` and the developer perspective's Dashboards tab stay. Targets and Dashboards (Perses) were placed after the removed item, so the patch places Targets after Dashboards (Perses), or after Metrics when Perses isn't loaded, and fixes Metrics' anchor (`alerts` → `alerting`, the Alerting item's real id). The menu then reads Alerting, Metrics, Dashboards (Perses), Targets. |

## What the plugin needs at runtime

- **No hosts are hard-coded in the plugin.** Its Go backend serves the static bundle and patches `plugin-manifest.json` for the enabled features. It proxies to Thanos or Alertmanager only in the `acm-alerting` feature, and there the URLs are flags (`-thanos-querier`, `-alertmanager`). We don't enable that feature.
- The frontend calls the bridge: `/api/prometheus`, `/api/prometheus-tenancy`, `/api/alertmanager` and `/api/alertmanager-tenancy` (base URLs from `SERVER_FLAGS`), and `/api/console/monitoring-dashboard-config` for dashboards. It doesn't use `/api/proxy/plugin/...`. Those paths belong to the separate `monitoring-console-plugin` (COO features: Perses, incidents, ACM).
- It chooses the tenancy paths per user. It runs a SelfSubjectAccessReview for `get` on `prometheusrules.monitoring.coreos.com` (alerts) and on `prometheuses/api` named `k8s` (metrics), both in all namespaces. If the user doesn't have it, it uses `/api/*-tenancy?namespace=<ns>`.

## Bumping upstream

1. Pick the upstream branch matching the console's base release (`git ls-remote https://github.com/openshift/monitoring-plugin.git 'refs/heads/release-*'`).
2. Update `UPSTREAM_BRANCH` and `UPSTREAM_COMMIT` in `upstream.env`.
3. Check that each patch in `patches/` still applies: `docker build .` in this directory fails if one doesn't.
4. Re-run the kind check in `royal-road/deploy/README.md` ("Monitoring plugin").
