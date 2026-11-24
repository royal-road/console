# Traces: trace-federator and the distributed-tracing plugin

Adds **Observe › Traces** to the console. Traces are searched and opened across several trace stores through [`royal-road/trace-federator`](../../trace-federator/README.md):
- VictoriaTraces and other Tempo-compatible stores;
- SigNoz;
- Sentry.

The plugin is [`royal-road/plugins/distributed-tracing-console-plugin`](../../plugins/distributed-tracing-console-plugin/README.md).

The federator also serves **profiles** from Pyroscope to observe-plugin's Profiles tab, through the same plugin proxy route (`/api/proxy/plugin/distributed-tracing-console-plugin/backend/profiles/...`). See [Profiles](#profiles).

```
browser ─▶ bridge ─(/api/plugins/distributed-tracing-console-plugin/)──────────▶ plugin (static files)
                  └(/api/proxy/plugin/distributed-tracing-console-plugin/backend/,
                    authorize: true → user's token)─▶ trace-federator ─▶ VictoriaTraces / SigNoz / Sentry
                                                                     ├▶ Pyroscope (/profiles/*)
                                                                     └▶ API server (SelfSubjectAccessReview with the user's token)
```

| File | What |
|---|---|
| `00-namespace.yaml` | Namespace `tracing` |
| `10-rbac.yaml` | Federator ServiceAccount (lists SentryProjectBindings only), and ClusterRole `trace-reader` for the OTLP stores |
| `20-config.yaml` | Federator configuration, with placeholders. The `profiles` block points at Pyroscope. |
| `30-federator.yaml` | Federator Deployment (2 replicas), Service and PDB. Reads Secret `trace-federator-credentials`. |
| `40-console-plugin.yaml` | Plugin Deployment and Service |
| `50-consoleplugin.yaml` | ConsolePlugin for Administration › Dynamic Plugins (display only) |
| `60-networkpolicy.yaml` | Only the console (and your metrics scraper) may call the federator and the plugin |
| `plugin-proxy.sh` | Adds the federator route to an existing `BRIDGE_PLUGIN_PROXY` value |
| `examples/sentryprojectbinding.yaml` | Bindings for Sentry access |

## Install

1. CRDs:

   ```sh
   kubectl apply --server-side -f royal-road/deploy/crds/observability.royalroad.com_sentryprojectbindings.yaml
   ```

   `90_consoleplugins.crd.yaml` is also needed for `50-consoleplugin.yaml`.
2. Fill in `20-config.yaml` (SigNoz URL, Sentry organization and public URL) and the image tags.
3. Create the credentials Secret. Never commit it. Its keys:
   - `sentry-token`: a Sentry API token with `org:read project:read event:read`;
   - `signoz-api-key`: a SigNoz service-account key with Viewer.

   For example, from a secret store that exposes them as environment variables:

   ```sh
   d=$(mktemp -d); printf %s "$SENTRY_TOKEN" > $d/sentry-token; printf %s "$SIGNOZ_KEY" > $d/signoz-api-key
   kubectl -n tracing create secret generic trace-federator-credentials --from-file=$d/sentry-token --from-file=$d/signoz-api-key
   rm -rf $d
   ```

4. Apply `00`–`60`.
5. Bridge:

   ```sh
   # add to BRIDGE_PLUGINS (comma-separated):
   distributed-tracing-console-plugin=http://distributed-tracing-console-plugin.tracing.svc:8080/
   # add the backend route to BRIDGE_PLUGIN_PROXY, keeping existing routes:
   royal-road/deploy/tracing/plugin-proxy.sh "$current_BRIDGE_PLUGIN_PROXY"
   # add to BRIDGE_I18N_NAMESPACES:
   plugin__distributed-tracing-console-plugin
   ```

6. Access:
   - Bind `trace-reader`, or a narrower copy listing fewer stores, to the groups that may read the OTLP stores.
   - Create `SentryProjectBinding`s (cluster-admins only) for the Sentry projects users should see; see [`../crds/README.md`](../crds/README.md#sentryprojectbinding).
   - Cluster-admins can read every store and every bound project.

## Profiles

The `profiles` block in `20-config.yaml` turns on `/profiles/*`. Without it those paths answer 404 and observe-plugin shows "Profiles not connected".

1. Pyroscope must admit the federator. Its ingress NetworkPolicy (`pyroscope-ingress` in namespace `pyroscope`, kept in the infrastructure repository) needs this entry on the port-4040 rule, with both selectors in one element so only the federator pods match:

   ```yaml
   - namespaceSelector:
       matchLabels:
         kubernetes.io/metadata.name: tracing
     podSelector:
       matchLabels:
         app.kubernetes.io/name: trace-federator
   ```

2. Access needs no new role: a user sees a namespace's profiles when they can `get pods` there.
3. `legacyServices` lists, per namespace, the `service_name` values whose profiles have no `namespace` label yet. Today that is `royalroad.website` in `royal-road`. When to remove an entry is in [the federator README](../../trace-federator/README.md#profiles).
4. Roll back by removing the `profiles` block and restarting the federator. Nothing else depends on it.

The rules for selectors, windows and sizes are in [the federator README](../../trace-federator/README.md#profiles).

## Access model

| Store | Rule |
|---|---|
| `access: rbac` (VictoriaTraces, SigNoz) | `get` on `traces.tracing.royalroad.com`, resourceName = store name. All or nothing per store: traces are not reliably namespaced. |
| `access: sentry-bindings` (Sentry) | Only projects with a SentryProjectBinding, within its environments. Visible if the user can `get` a bound workload, or pods in a bound namespace. Admins see every bound project. |
| Profiles (Pyroscope) | `get` on `pods` in the namespace. Every selector must name that namespace, or a service listed for it in `legacyServices`. |

The instance dropdown lists only views with at least one readable store. A user with none sees "No trace stores available".

## Verified on kind

Setup:
- `royal-road/dev/kind-up.sh`, plus the federator and plugin images built from this repository.
- Two VictoriaTraces v0.13.0 instances (`victoria`, `victoria-archive`).
- The real self-hosted SigNoz and Sentry, read-only.
- A `legacy-tempo` store pointing at a Service that does not exist.
- Two bridges run off-cluster with `run-bridge.sh`: one as cluster-admin, one as a ServiceAccount `dev`. `dev` has:
  - `view` in namespace `royal-road`;
  - `get` on `traces` for the two VictoriaTraces stores.
- Bindings:
  - Sentry project A (production only) → Deployment `royal-road/website`;
  - project B → namespace `api`.

Results:
- **Merge.** One real trace was present in SigNoz (34 spans) and Sentry (43 spans, from Sentry.OpenTelemetry, so the trace IDs match). A synthetic span was added to each VictoriaTraces instance. Opened as admin through `all-stores`, it showed spans from all four stores:
  - `victoria`: 1; `victoria-archive`: 1; `signoz`: 34;
  - `sentry`: 9 — the spans SigNoz did not have. Duplicates were dropped by span ID.

  Each span's details show `federation.store`.
- **Partial results.** `legacy-tempo` fails. The search and trace pages show the warning, with the stores that answered. A namespace filter shows "sentry: cannot run this query" (Sentry has no `k8s.namespace.name`), and the VictoriaTraces results still show.
- **Scoping.**
  - `dev` sees the `all-stores`, `victoria`, `victoria-archive` and `sentry` views, but not `signoz` or `legacy-tempo`.
  - `dev` opens the same trace with only the VictoriaTraces spans and the project-A Sentry spans.
  - Searching project B as `dev` returns nothing; as admin it returns traces.
  - A forged or missing token gets 401.
- **CRD.** A target with `kind` but no `name` is rejected by the CEL rule. `dev` can neither create nor list bindings.
