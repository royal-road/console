# trace-federator

A small Go service that serves the **Tempo HTTP API over several trace stores** and the **backend API of the distributed-tracing console plugin** (Observe › Traces). The console then searches and opens traces across every store at once, or one store at a time.

It also serves **profiles from Pyroscope**, scoped per namespace, for observe-plugin's Profiles tab ([Profiles](#profiles)). The name stays trace-federator.

Supported stores:

| Type | Store | How it is read |
|---|---|---|
| `tempo` | Grafana Tempo, VictoriaTraces (`/select/tempo`, v0.10 or later), anything with Tempo's API | Passthrough |
| `signoz` | SigNoz (checked against v0.144) | v5 query API, `POST /api/v5/query_range`, with a service-account API key |
| `sentry` | Sentry (checked against self-hosted 26.10.1) | Event Analytics Platform span APIs with an API token |

It uses the Go standard library only.

## Why it exists

Traces are expensive to keep, so they live in more than one place:
- a hot store with every span and a short retention;
- a long-term store with sampled traces;
- Sentry, with the SDKs' own samples.

No open-source component federates trace queries. Grafana's `federation-frontend` is Enterprise-only, and Jaeger v2 reads one backend. See the design notes in the PR for the full comparison.

## HTTP surface

| Path | Purpose |
|---|---|
| `GET /api/v1/list-tempo-resources` | The plugin's instance discovery. Returns one "instance" (view) per store the viewer may read, plus `all-stores` first. |
| `GET /proxy/{namespace}/{view}/{tenant}/api/search` | TraceQL search (`q`, `start`, `end`, `limit`, `slices`) |
| `…/api/v2/traces/{id}`, `…/api/traces/{id}` | Trace by ID: OTLP JSON (v2) or `batches` (v1) |
| `…/api/v2/search/tags`, `…/api/search/tags` | Tag names |
| `…/api/v2/search/tag/{tag}/values`, `…/api/search/tag/{tag}/values` | Tag values |
| `GET /profiles/{namespace}/scopes` | The base selectors a viewer may narrow in `{namespace}` ([Profiles](#profiles)) |
| `POST /profiles/{namespace}/{method}` | Pyroscope query passthrough ([Profiles](#profiles)) |
| `GET /metrics`, `GET /healthz` | Prometheus metrics; liveness |

The `namespace` and `tenant` path segments are ignored; they exist because the plugin builds its URLs that way.

### Merging

**Search** runs on every store of the view in parallel and unions the results by trace ID. For a trace found in several stores, the merged row takes:
- the earliest start;
- the longest duration;
- the root span name and service from whichever store has the root;
- the union of span sets.

The merged list is sorted newest first and cut to `limit`. Every row lists its `federationStores`. The order is "most recent N per store, merged", which is exact unless a store returns rows out of order.

Stores return the most recent matches, so a search over a busy hour gets traces from its last minutes only. **`slices=N`** (1 to 24, larger values are capped) spreads the samples over the range:
- `[start, end]` is split into N equal windows on whole seconds, never more windows than seconds.
- Each store searches every window in parallel, with limit `ceil(limit/N)`, within its usual `searchTimeout`. Any window failing fails that store.
- The results are merged as above, de-duplicated by trace ID and sorted newest first. The cut to `limit` is per window: at most `ceil(limit/N)` traces that start in each window, so several stores that return different traces still cover the whole range. When `N` does not divide `limit`, the final cut drops a few traces from the oldest window.
- Without `slices`, with `slices=1`, or without both `start` and `end`, the search is one query per store. `slices` is never sent to the stores.

Observe's trace scatter sends `slices=12`.

**Trace by ID** asks every store and concatenates the spans:
- Each resource gets the attribute **`federation.store=<store>`**, which the span details panel shows.
- IDs are normalised to hex (VictoriaTraces returns base64).
- A span ID present in several stores is kept once, from the store with the highest `priority`.

**Tags and values** are unions.

### Partial results

Each store has its own `searchTimeout` and `traceTimeout`. A store that times out or fails does not fail the request.

Every response carries **`X-Federation-Status`**, a JSON list of `{store, status, message}` in ASCII. The statuses are:

| Status | Meaning |
|---|---|
| `ok` | The store answered. |
| `skipped` | The time range is older than the store's `maxLookback`. |
| `denied` | The viewer may not read this store. |
| `unsupported` | The store cannot run this query (below). |
| `error` | The store failed. |
| `timeout` | The store did not answer within its timeout. |

The patched plugin shows a warning when any store is `unsupported`, `error` or `timeout`. Trace-by-ID responses also carry `message`, which Perses shows as a notice. The request fails with 502 only if no store answered.

### TraceQL

Stores of type `tempo` receive the query unchanged. For `signoz` and `sentry`, the federator parses the subset of TraceQL that the plugin's filter bar generates, plus simple hand-written variations:

```
{}   { C && C ... }   C := attr op value | ( C || C ... )   [ | select(...) ]
attr: name status kind duration traceDuration statusMessage .x span.x resource.x
op:   = != =~ !~ > >= < <=
```

Anything else is reported as `unsupported` for those stores and never silently returns nothing. That includes structural operators, several spansets, aggregates and `rootName`.

Regexes:
- An alternation of literals (`"a|b"`) becomes an IN list.
- SigNoz gets other regexes as anchored `REGEXP`.
- Sentry only accepts `prefix.*`.
- `traceDuration` is applied after fetching.

## Access control

The bridge's plugin proxy (`authorize: true`) forwards the viewer's bearer token. The federator checks it with **SelfSubjectAccessReviews made with that token**, so the API server authenticates the viewer (Dex OIDC tokens included). The federator's ServiceAccount needs no review permissions. Decisions are cached for `authz.cacheTTL` (60 s), keyed by a hash of the token.

- **`access: rbac`** (default for `tempo` and `signoz`): the viewer needs `get` on `traces.tracing.royalroad.com`, with resourceName = store name. This resource is virtual; nothing serves it. See ClusterRole `trace-reader` in `../deploy/tracing/10-rbac.yaml`. Traces are not reliably namespaced, so access is per store.
- **`access: sentry-bindings`** (required for `sentry`): Sentry data is visible per project, following [`SentryProjectBinding`](../deploy/crds/README.md#sentryprojectbinding)s. These are cluster-scoped, and only cluster-admins can write them.
  - A viewer sees a bound project, within the bound environments, if they can `get` one of the binding's targets: the Deployment, StatefulSet, DaemonSet, CronJob or Job, or `pods` in the namespace for namespace-wide targets.
  - Viewers who can `list` SentryProjectBindings (cluster-admins) see every bound project.
  - **Unbound projects are never shown.**
  - Sentry's trace endpoint ignores its `project` parameter, so the federator filters trace-by-ID results span by span. Search rows whose root project is not visible are shown without its name.
  - Span attributes are limited to `attributeAllowlist`, because Sentry spans carry user data such as `user.id` and `user.geo.*`.

The federator's ServiceAccount only lists SentryProjectBindings, every `kubernetes.bindingsRefresh` (30 s).

## Profiles

`/profiles/*` passes six methods of Pyroscope's Connect API through to `POST <profiles.url>/querier.v1.QuerierService/{method}`: `SelectMergeStacktraces`, `SelectSeries`, `Diff`, `Series`, `LabelNames` and `LabelValues`. Other methods get 404, `ProfileTypes` included: it takes no selector and lists every service's types. The plugin reads a workload's types from `Series` with `labelNames: ["__profile_type__"]`.

**Access.** The viewer needs `get` on `pods` in `{namespace}`, checked with a SelfSubjectAccessReview made with their token and cached like the others. There is no extra ClusterRole: whoever can see a namespace's pods can see its profiles.

**Selectors** are the security boundary. Each `labelSelector`, each side of a `Diff`, and the one entry of `matchers` is parsed strictly (`{label op "string", …}`, ops `= != =~ !~`, double-quoted Go strings) and must have one of two shapes:

| Shape | Condition | Forwarded as |
|---|---|---|
| Labelled | exactly one `namespace` matcher, `namespace="<ns>"` | re-serialised |
| Legacy | no `namespace` matcher, exactly one `service_name` matcher, `service_name="<s>"` with `s` in `profiles.legacyServices[<ns>]` | re-serialised, with `namespace=~"\|<ns>"` appended |

Anything else gets 400. Other matchers (`pod`, `k8s_deployment_name`, `env`) only narrow. The legacy shape covers profiles pushed before the services carried `namespace` and `k8s_deployment_name` labels: `namespace=~"|<ns>"` matches a profile without the label or with that namespace, never one labelled with another namespace. `legacyServices` is a trust statement: it says unlabelled profiles of that service come from that namespace. Remove an entry once the service has carried the labels for Pyroscope's retention.

**Requests** are rebuilt from an allowlist of fields per method; any other field gets 400, so a field Pyroscope adds later cannot bypass these checks. Field names are matched exactly.

| Method | Fields | Checks |
|---|---|---|
| `SelectMergeStacktraces` | `profileTypeID`, `labelSelector`, `start`, `end`, `maxNodes`, `stackTraceSelector` | `maxNodes` missing or 0 becomes 8192 (Pyroscope's default); larger than `profiles.maxNodes` is clamped. `stackTraceSelector` may hold `callSite: [{name}]` only. |
| `SelectSeries` | `profileTypeID`, `labelSelector`, `start`, `end`, `step`, `groupBy`, `aggregation`, `limit` | `step` ≥ 1 s, at most 2000 points. `stackTraceSelector` gets 400: Pyroscope ignores it here, so the selected-function timeline uses `SelectMergeStacktraces` with `callSite` and `maxNodes: 1` per bucket. |
| `Diff` | `left`, `right` | Each side as `SelectMergeStacktraces` |
| `Series` | `matchers` (exactly one), `labelNames`, `start`, `end` | Pyroscope returns nothing for several matchers |
| `LabelNames` | `matchers` (one), `start`, `end` | |
| `LabelValues` | `name`, `matchers` (one), `start`, `end` | |

`start` and `end` (Unix ms, numbers or strings) are required, and `end - start` must be at most `profiles.maxWindow` (24 h) for every method: Pyroscope's `max_query_length` is 1 day, for the metadata methods too. Bodies are at most 64 KiB (413) and `application/json` (415).

**Responses** are Pyroscope's JSON, streamed, with `Cache-Control: no-store`, and gzip when the request accepts it (a 1 h flame graph at 16k nodes: 0.89 MB, 0.13 MB gzipped). int64 fields are JSON strings. Errors are Connect-shaped, `{"code", "message"}`:

| Status | Code | When |
|---|---|---|
| 400 | `invalid_argument` | A rule above, or Pyroscope's own 4xx (passed through) |
| 401 | `unauthenticated` | No token, or the API server rejected it |
| 403 | `permission_denied` | No `get pods` in the namespace |
| 404 | `not_found` | Unknown method, or no `profiles` block configured |
| 413 | `resource_exhausted` | Body over 64 KiB |
| 415 | `invalid_argument` | Not `application/json` |
| 502 | `unavailable` | Pyroscope unreachable or 5xx, or the access review failed |
| 504 | `deadline_exceeded` | Over `profiles.timeout` |

Profile data is not cached. The viewer's token never reaches Pyroscope.

## Configuration

JSON (a ConfigMap), passed with `-config` (default `/etc/trace-federator/config.json`). See `../deploy/tracing/20-config.yaml`. Durations accept Go syntax plus `Nd` for days.

| Field | Meaning |
|---|---|
| `listen` | Default `:8080`. |
| `instanceNamespace` | Namespace label shown in the instance dropdown (default `tracing`). |
| `allView` | Name of the view over all stores (default `all-stores`). |
| `authz.group`, `authz.resource`, `authz.cacheTTL` | Virtual resource for `rbac` stores; cache lifetime. |
| `kubernetes.apiServer`, `tokenFile`, `caFile`, `insecureSkipVerify` | Defaults to the in-cluster configuration. |
| `kubernetes.bindingsRefresh` | How often SentryProjectBindings are re-listed. |
| `backends[].name` | Store name: shown to users, and used as the RBAC resourceName. |
| `type`, `url`, `access` | See above. |
| `priority` | Higher wins when the same span is in several stores. Put stores with full OTLP attributes above Sentry. |
| `maxLookback` | The store's retention. Older search windows are skipped, and trace-by-ID looks back this far. |
| `searchTimeout`, `traceTimeout` | Per store; defaults 8 s and 5 s. |
| `bearerTokenFile` | `tempo`: optional bearer token. |
| `apiKeyFile` | `signoz`: API key. |
| `tokenFile`, `organization`, `publicURL` | `sentry`: credentials, organization, and the base URL for links. |
| `spanNameField`, `attributeAllowlist`, `unsupportedAttributes` | `sentry`: the field TraceQL `name` maps to (default `span.name`); the attributes returned; and the TraceQL attributes Sentry never has, which are reported as `unsupported` instead of matching nothing. |
| `profiles.url` | Pyroscope's base URL. Without a `profiles` block every `/profiles/*` path answers 404. |
| `profiles.timeout`, `maxWindow`, `maxNodes` | Upstream timeout (20 s), the longest window (24 h, Pyroscope's `max_query_length`), the `maxNodes` ceiling (16384). |
| `profiles.legacyServices` | Namespace → `service_name` values allowed in the legacy selector shape. |

## Store notes

**SigNoz:**
- **Search** takes four steps:
  1. A raw span query finds the most recent matching spans, ordered by time. With `traceDuration`, it finds root spans of matching duration instead.
  2. Their distinct trace IDs are collected.
  3. A `requestType: trace` query gets the summaries.
  4. A root-span query gets the start times.
- The `trace` request type ignores `order` and returns the longest traces first, so it cannot be the first step.
- **Trace by ID** is a raw query on `trace_id` over `maxLookback`. It was fast in testing: about 0.3 s over 30 days.
- The SigNoz data checked had no `k8s.*` resource attributes.

**Sentry:**
- Search uses `/organizations/{org}/traces/`.
- Trace by ID uses `/organizations/{org}/trace/{id}/` with `additional_attributes`.
- Tag values use `/organizations/{org}/trace-items/attributes/{key}/values/`.
- `resource.service.name` is the project slug.
- Sentry fills attributes a span lacks with `""` or `0`; the federator drops those.
- Sentry and OpenTelemetry trace IDs match when Sentry spans come from OpenTelemetry (`origin: auto.otel`, Sentry.OpenTelemetry). In that case the same span can be in Sentry and an OTLP store, and priority picks the OTLP copy.

**VictoriaTraces:**
- Search rows have no `serviceStats`, so the plugin shows "0 spans" for traces found only there.
- Use v0.10 or later: `traceDuration` fails on v0.9.

## Metrics

| Metric | Labels |
|---|---|
| `tracefed_requests_total` | `endpoint`, `code` |
| `tracefed_backend_requests_total` | `store`, `outcome` (Pyroscope calls use store `pyroscope`) |
| `tracefed_backend_duration_seconds` | `store` |
| `tracefed_partial_responses_total` | `endpoint` |
| `tracefed_deduplicated_spans_total` | `store` |
| `tracefed_access_reviews_total` | `allowed` |

Profiles requests use the endpoint labels `profiles-scopes` and `profiles-<method>` (`profiles-other` for unknown methods). Rejected selectors are also logged with the namespace and method.

## Development

```sh
cd royal-road/trace-federator
go test ./...
docker build -t trace-federator .
```

CI: `.github/workflows/royal-road-trace-federator.yml` tests on pull requests, and on `main` pushes `ghcr.io/royal-road/trace-federator:<short-sha>` and `:main`.
