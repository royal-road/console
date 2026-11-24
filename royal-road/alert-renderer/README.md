# alert-renderer

An Alertmanager webhook receiver that posts each notification to a Discord
webhook as readable embeds, each with a chart of the alert rule.

Alertmanager's own Discord integration posts every label as a `name = value`
bullet and links to the vmalert pod by IP. alert-renderer posts this instead:

- **Title and colour:** `Firing: <alertname>`, `Firing (3): …` or
  `Resolved: …`. The colour is red for critical, amber for warning, grey for
  anything else, and green when resolved. The severity comes from the rule's own
  `severity` label and is shown above the title with the rule group.
- **Summary and description** from the annotations, then **Investigate** and
  **Runbook** (`runbook_url`) links.
- **Identifying labels** as compact inline fields: namespace, workload, pod,
  service, ingress, and the rule's own labels. Scrape labels (`job`,
  `instance`, `endpoint`…) are folded into the footer. `prometheus`,
  `alertgroup` and the like are dropped.
- **Since / Lasted** as Discord timestamps, and the rule's **condition**,
  for example `nginx_ingress_controller_requests > 0.02`.
- **A chart** of the rule's expression from the alert's start minus
  `CHART_LOOKBACK` to now, at most 12 hours. A resolved alert's chart ends
  `CHART_LOOKBACK/2` after it resolved.
- **Grouped alerts:** one embed per rule. Labels shared by all alerts become
  fields. Each alert gets one line with what tells it apart, how long it has
  been firing, and its own Observe link.

Previews of real alerts are in the task's `mockups/discord/`, and
[`dev/preview.py`](dev/preview.py) makes them.

## The chart

The expression comes from vmalert. Alertmanager's payload has no expression,
only the `generatorURL` with vmalert's `group_id` and `alert_id`.
alert-renderer reads `/api/v1/alert?group_id=&alert_id=` while the alert is
active. After that (resolved alerts), it reads
`/api/v1/rules?type=alert&rule_name[]=<alertname>` and picks the rule by group
ID, then by `alertgroup`. It never uses the generator URL's host.

[`src/expr.rs`](src/expr.rs) splits the expression at its loosest top-level
binary operator. This is a lexer that knows brackets, strings, comments and
operator precedence, not a full PromQL parser: no maintained Rust crate parses
MetricsQL, and the operands must stay as their original text so they can be
queried unchanged. The cases:

| Expression | Charted |
|---|---|
| `x > 5`, `5 < x`, `x > bool 5`, `x > 0.05 * 100`, `x > on() group_left vector(5)` | `x`, with a horizontal line at 5 (red for critical, amber for warning) |
| `x < y` (two vectors, any matching modifiers) | `x`, with `y` dashed in the colour of the `x` series it matches |
| `a > 1 and on(i) b`, `a > 1 unless b`, `a > 1 default 0`, `a > 1 if b` | the `a > 1` side, as above |
| `a < 3 or b < 3` (every branch a comparison) | `a` and `b`, with every branch's lines |
| `max by (l) (x == 1)`, `min(…)` | `max by (l) (x)` with the line (min and max keep the values they select) |
| `x > 1 < 5` | `x` with both lines |
| anything else (`absent(…)`, `vector(1)`, arithmetic) | the expression as written, without a line |

`testdata/prod-exprs.json` holds every alerting rule expression in production
(329 on 2026-11-24, hostnames and IPs replaced). The tests run the planner over
all of them: every query balances, and 307 of the 329 get a threshold.

Each query runs as a `query_range` with about 240 points. Series whose labels
match an alert in the notification are coloured, in alert order. If no series
matches exactly, the scrape labels are ignored. If still none matches, the
highest series are coloured. Up to `CHART_MAX_SERIES` other series are drawn
faint, and the legend counts the rest. The firing interval is shaded. Times are
in UTC.

The PNG is drawn in-process with [tiny-skia](https://github.com/linebender/tiny-skia)
and a subset of DejaVu Sans compiled into the binary
([`assets/LICENSE-DejaVu.txt`](assets/LICENSE-DejaVu.txt)), so the image needs
no fonts or fontconfig. If the rule or the data cannot be fetched, or
rendering fails, the message still goes out without the chart, and the footer
says why.

## Links

**Investigate** opens the console:
- **With `namespace` and a workload label** (`deployment`, `statefulset`,
  `daemonset`, their OTel `k8s_*_name` forms, or `pod`): the workload's
  Observe tab, `/k8s/ns/<ns>/<plural>/<name>/observe?focus=alert:<alertname>&from=<start − 30 min>&to=now`.
  For a resolved alert, `to` is 30 minutes after it ended instead of `now`, so
  the link still shows the incident later.
- **Otherwise:** observe-plugin's `/observe/alert?alertname=…&<identifying
  labels>&activeAt=<ms>` route. It resolves the labels (a Service or Ingress,
  for example) to a workload in the console, or falls back to Observe ›
  Alerts.

## Discord

alert-renderer sends one `multipart/form-data` POST: `payload_json` (embeds,
`attachments`, `allowed_mentions: {parse: []}` so label text can never ping)
and `files[i]` (the PNGs).
- It stays within Discord's limits: 10 embeds, 6000 characters, 4096 per
  description and 1024 per field. It shortens text first, then lists the
  rules it left out.
- Label values are markdown-escaped.
- On a 429 it waits for `retry_after` (5 s at most) and retries once.
- If Discord rejects the message with images, it is sent once more as text
  only.

alert-renderer answers Alertmanager:
- `200` when sent;
- `502` when Discord is unreachable or failing, so Alertmanager retries;
- `422` when Discord rejects the message (a retry would fail the same way);
- `503` when two notifications are already in progress.

## Configuration

| Variable | Default | |
|---|---|---|
| `CONSOLE_URL` | required | Console base URL for links |
| `VMALERT_URL` | required | vmalert with its path prefix, e.g. `http://vmalert:8080/vmalert` |
| `VMSELECT_URL` | required | Prometheus API root, e.g. `http://vmselect:8481/select/0/prometheus` |
| `DISCORD_WEBHOOK_URL_FILE` | `/etc/alert-renderer/discord/url` | File with the webhook URL (a secret: never logged) |
| `LISTEN_ADDR` | `0.0.0.0:8080` | |
| `CHART_LOOKBACK` | `1h` | Chart window before the first alert started |
| `CHART_MAX_SERIES` | `8` | Faint series drawn besides the alert's own |
| `CHART_MAX_CHARTS` | `4` | Rules per notification that get a chart |
| `DISCORD_ALLOW_HTTP` | unset | `true` allows a plain-HTTP webhook URL (tests only) |

Endpoints:
- `POST /alertmanager`: the webhook;
- `GET /healthz`: health check.

Limits and timeouts:
- every vmalert and vmselect call has a 5 s timeout;
- Discord calls have a 10 s timeout;
- chart work for one notification stops after 15 s;
- webhook bodies are capped at 1 MiB;
- rules responses are capped at 4 MiB;
- `query_range` responses are capped at 2 MiB.

## Build, test, preview

```sh
cargo test                      # parser, labels, embeds, chart, time
cargo clippy --all-targets -- -D warnings
dev/e2e.sh target/debug/alert-renderer   # HTTP end to end against dev/fake_backends.py
docker build -t alert-renderer .         # FROM scratch, one static binary

# The message and charts a payload would produce, as if it arrived at NOW:
VMALERT_URL=… VMSELECT_URL=… CONSOLE_URL=… \
  alert-renderer render testdata/payloads/ingress-5xx-firing.json out/ 2026-11-23T00:50:30Z
dev/preview.py out.html "IngressHigh5xxRate" out/ 2026-11-23T00:50:30Z
```

`dev/e2e.sh` starts four renderers against fake vmalert, vmselect and Discord
backends and posts every payload in `testdata/payloads/`. It checks:
- the HTTP answers;
- the captured multipart parts;
- the text-only retry and the 429 retry;
- the 502 when Discord is down;
- 400, 404 and 413 on bad requests;
- that the webhook URL never reaches the logs.

## Image

The image is `FROM scratch` and holds only the static musl binary, about 3.4
MB, run as UID 65532. It has no shell, package manager, libc, CA bundle
(rustls carries the webpki roots) or fonts.

The release profile uses `lto`, one codegen unit and `strip`. It unwinds on
panic instead of aborting: a rendering bug is caught, and the notification
still goes out as text.

There are six direct dependencies: serde, serde_json, ureq (rustls), tiny_http,
tiny-skia and ab_glyph. Runtime dependencies total 54 crates.

`cargo audit` reports only RUSTSEC-2026-0192: ttf-parser, under ab_glyph, is
unmaintained. It only parses the font compiled into the binary.

Deployment: [`royal-road/deploy/alert-renderer/`](../deploy/alert-renderer/README.md).
