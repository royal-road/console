# distributed-tracing-console-plugin build

Builds [openshift/distributed-tracing-console-plugin](https://github.com/openshift/distributed-tracing-console-plugin) for the Royal Road console. It provides **Observe › Traces**: the search page with its scatter plot, and the trace details page with its Gantt chart. Data comes from [`royal-road/trace-federator`](../../trace-federator/README.md), which federates VictoriaTraces, SigNoz and Sentry.

| File | Purpose |
|---|---|
| `upstream.env` | Upstream repository, branch and pinned commit. |
| `patches/*.patch` | Our changes, applied in order with `git apply`. |
| `Dockerfile` | Fetches the pinned commit, applies the patches, builds the frontend, and serves `web/dist` with nginx on 8080. |

CI: `.github/workflows/royal-road-distributed-tracing-plugin.yml` builds on pull requests that touch this directory. On `main` it pushes `ghcr.io/royal-road/distributed-tracing-console-plugin:<short-sha>` and `:main`.

```sh
docker build -t distributed-tracing-console-plugin royal-road/plugins/distributed-tracing-console-plugin
```

## Frontend only

Upstream's image also contains a Go backend. That backend:
- discovers TempoStack and TempoMonolithic resources (Tempo operator CRDs);
- skips instances without multi-tenancy;
- reverse-proxies to the Tempo gateway over the serving-cert CA.

None of that exists here. trace-federator implements the same backend API instead: `GET /api/v1/list-tempo-resources` and `/proxy/{ns}/{name}/{tenant}/…`. The bridge's plugin proxy points `/api/proxy/plugin/distributed-tracing-console-plugin/backend/` at it, with `authorize: true`. So no backend patch is needed, and the image is static files only.

Each "Tempo instance" in the dropdown is a federator view:
- `all-stores`, which queries every store the user may read;
- one view per store.

## Upstream ref

Upstream has no console-5.0 release branch; its release branches are for the Cluster Observability Operator. `main` at the pinned commit builds against `@openshift-console/dynamic-plugin-sdk` 4.22, React 18 and PatternFly 6, which console 5.0 serves.

To bump:
1. Set `UPSTREAM_COMMIT` to the new commit.
2. Build. `git apply` fails loudly if a patch no longer applies.
3. Re-run the kind check in `royal-road/deploy/tracing/README.md`.

## Patches

| Patch | Change |
|---|---|
| `0001-federation-status.patch` | **Per-store status.** The Perses Tempo client drops response headers. The patch gives the Tempo datasource a `fetchJson` (`web/src/federation.ts`) that records the federator's `X-Federation-Status` header. `FederationStatusAlert` then warns on the search and trace pages when stores timed out, failed or could not run the query, and lists the stores the results come from. |
| | **Instance selection.** The first instance is preselected even when there are several (the federator lists `all-stores` first). |
| | **Empty state.** "No Tempo instances yet / create a TempoStack" becomes "No trace stores available", because an empty list from the federator means no access. |
| | **Locale.** The English locale is regenerated with `npm run i18n`. |
| `0002-place-traces-in-observe-order.patch` | **Menu position.** Observe › Traces gets an anchor: after Logs, else Metrics, Dashboards (Perses) or observe-plugin's Explorer. Upstream has none, so the console placed Traces by plugin load order and it could head the menu. monitoring-plugin's patch 0005 describes the whole menu order. |

The source store of each span shows without any patch: it is the resource attribute `federation.store` in the span details panel.

To write a new patch:
1. Clone upstream at `UPSTREAM_COMMIT`.
2. `git apply` the existing patches and commit them.
3. Make your change and commit it.
4. Run `npm run i18n` in `web/` if strings changed.
5. Export with `git format-patch -1 --zero-commit --no-signature`.
