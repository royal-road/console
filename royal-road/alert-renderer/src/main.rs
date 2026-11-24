//! alert-renderer: an Alertmanager webhook receiver that posts each
//! notification to Discord as readable embeds with a chart of the rule.
//!
//!   alert-renderer                     serve (configuration from the environment)
//!   alert-renderer render IN OUT [NOW] write OUT/message.json and the charts for
//!                                      payload IN, as if it arrived at NOW

mod chart;
mod discord;
mod expr;
mod fetch;
mod labels;
mod payload;
mod time;

use std::io::Read;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use serde_json::{Value, json};

use discord::{Discord, Section, SendError};
use expr::Threshold;
use fetch::Sources;
use payload::Notification;
use time::Millis;

const MAX_BODY: usize = 1 << 20;
/// Notifications rendered at once; each holds its charts and query results
/// in memory, so this bounds memory under the 128 Mi limit.
const MAX_RENDERING: usize = 2;
/// Notifications accepted at once, rendering or waiting for a slot. A burst
/// (TargetDown groups by every label) queues instead of failing.
const MAX_ACCEPTED: usize = 32;
/// Timings from when a notification arrives. Alertmanager gives up after its
/// 30 s `timeout` and sends again, so everything, the Discord posts included,
/// ends by REQUEST_DEADLINE. A notification that waited QUEUE_WAIT for a slot
/// is refused with 503 and comes back later; chart work stops at
/// CHART_DEADLINE so the posts keep at least 10 s.
const QUEUE_WAIT: Duration = Duration::from_secs(8);
const CHART_DEADLINE: Duration = Duration::from_secs(15);
const REQUEST_DEADLINE: Duration = Duration::from_secs(25);
/// The longest chart window: 12 hours.
const MAX_WINDOW: Millis = 12 * 3_600_000;

struct Config {
    console: String,
    lookback: Millis,
    max_series: usize,
    max_charts: usize,
}

fn env(name: &str) -> Option<String> {
    std::env::var(name).ok().map(|v| v.trim().to_string()).filter(|v| !v.is_empty())
}

fn required(name: &str) -> String {
    env(name).unwrap_or_else(|| fail(&format!("{name} is not set")))
}

fn fail(msg: &str) -> ! {
    log(json!({"level": "error", "msg": msg}));
    std::process::exit(2)
}

fn log(mut v: Value) {
    v["ts"] = json!(time::rfc3339(time::now_ms()));
    eprintln!("{v}");
}

fn config() -> Config {
    let parse_num = |name: &str, default: usize| {
        env(name).map_or(default, |v| v.parse().unwrap_or_else(|_| fail(&format!("{name} must be a number"))))
    };
    let console = required("CONSOLE_URL");
    if !console.starts_with("https://") && !console.starts_with("http://") {
        fail("CONSOLE_URL must be an http(s) URL");
    }
    Config {
        console,
        lookback: env("CHART_LOOKBACK").map_or(3_600_000, |v| {
            time::parse_duration(&v)
                .unwrap_or_else(|| fail("CHART_LOOKBACK must be a duration such as 1h"))
                .as_millis() as Millis
        }),
        max_series: parse_num("CHART_MAX_SERIES", 8),
        max_charts: parse_num("CHART_MAX_CHARTS", 4),
    }
}

fn sources() -> Sources {
    Sources::new(&required("VMALERT_URL"), &required("VMSELECT_URL"), Duration::from_secs(5))
}

/// Longest rule expression shown as written; past it, a summary.
const CONDITION_MAX: usize = 200;

/// The rule's condition: the expression itself without label lists when it
/// is short, else in words (`running < desired`, `nginx_requests > 0.02`).
fn condition(expr: &str, plan: &expr::ChartPlan) -> Option<String> {
    let full = expr::condensed(expr);
    if !full.is_empty() && full.chars().count() <= CONDITION_MAX {
        return Some(full);
    }
    let lhs = plan.queries.iter().map(|q| chart::query_name(q)).collect::<Vec<_>>().join(" | ");
    let parts: Vec<String> = plan
        .thresholds
        .iter()
        .map(|t| match t {
            Threshold::Scalar { op, value } => format!("{} {}", op.symbol(), chart::format_value(*value)),
            Threshold::Series { op, query } => format!("{} {}", op.symbol(), chart::query_name(query)),
        })
        .collect();
    (!parts.is_empty()).then(|| format!("{lhs} {}", parts.join(" ")))
}

/// Fetches the rule and its data and draws the chart for one section.
fn chart_for(
    section: &Section,
    src: &Sources,
    cfg: &Config,
    now: Millis,
    deadline: Instant,
) -> Result<(Vec<u8>, Option<String>), String> {
    let expr = src.expression(section.alerts[0], deadline).map_err(|e| format!("rule unavailable ({e})"))?;
    let plan = expr::plan(&expr);
    let cond = condition(&expr, &plan);
    let starts = section.alerts.iter().filter_map(|a| a.starts_ms()).min().unwrap_or(now);
    let end = if section.alerts.iter().any(|a| a.firing()) {
        now
    } else {
        let ended = section.alerts.iter().filter_map(|a| a.ended_ms()).max().unwrap_or(now);
        (ended + cfg.lookback / 2).min(now)
    };
    // Repeat notifications of a long-running alert keep a readable window.
    let start = (starts - cfg.lookback).max(end - MAX_WINDOW);
    let end = end.max(start + 60_000);
    let mut primary = Vec::new();
    for q in plan.queries.iter().take(3) {
        primary.extend(src.range(q, start, end, deadline).map_err(|e| format!("query failed ({e})"))?);
    }
    let mut threshold_series = Vec::new();
    let mut threshold_name = String::new();
    let mut scalars = Vec::new();
    for t in &plan.thresholds {
        match t {
            Threshold::Scalar { op, value } => scalars.push((*op, *value)),
            Threshold::Series { query, .. } => {
                // A failed right-hand side still leaves a useful chart.
                threshold_series.extend(src.range(query, start, end, deadline).unwrap_or_default());
                threshold_name = chart::query_name(query);
            }
        }
    }
    let spec = chart::Spec {
        start,
        end,
        severity: section.severity(),
        firing: section.alerts.iter().filter_map(|a| Some((a.starts_ms()?, a.ended_ms()))).collect(),
        primary,
        primary_name: chart::query_name(&plan.queries[0]),
        threshold_series,
        threshold_name,
        scalars,
        alerts: section.alerts.iter().map(|a| &a.labels).collect(),
        max_series: cfg.max_series,
    };
    let png = chart::render(&spec).map_err(|e| format!("rendering failed ({e})"))?;
    Ok((png, cond))
}

/// The Discord message and its attachments for a notification. Charts not
/// done by `deadline` are left out.
fn render(
    n: &Notification,
    src: &Sources,
    cfg: &Config,
    now: Millis,
    deadline: Instant,
) -> (Value, Vec<(String, Vec<u8>)>) {
    let mut sections = discord::sections(&n.alerts);
    let charted = sections.len().min(cfg.max_charts);
    let results: Vec<Result<_, String>> = std::thread::scope(|scope| {
        let handles: Vec<_> = sections[..charted]
            .iter()
            .map(|s| scope.spawn(move || chart_for(s, src, cfg, now, deadline)))
            .collect();
        // A panic in fetching or drawing must not cost the notification: the text still goes out.
        handles.into_iter().map(|h| h.join().unwrap_or_else(|_| Err("rendering panicked".into()))).collect()
    });
    let mut files = Vec::new();
    for (section, result) in sections.iter_mut().zip(results) {
        match result {
            Ok((png, cond)) => {
                let name = format!("chart-{}.png", files.len());
                section.chart = Some(name.clone());
                section.condition = cond;
                files.push((name, png));
            }
            Err(e) => {
                log(
                    json!({"level": "warn", "msg": "no chart", "alertname": section.alerts[0].alertname(), "error": e}),
                );
                section.chart_note = Some(e);
            }
        }
    }
    let message = discord::message(&sections, &cfg.console, n.truncated_alerts);
    // Sections dropped to fit Discord's limits take their charts with them.
    let attached: Vec<&str> = message["attachments"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|a| a["filename"].as_str())
        .collect();
    files.retain(|(name, _)| attached.contains(&name.as_str()));
    (message, files)
}

fn boundary() -> String {
    static N: AtomicU64 = AtomicU64::new(0);
    format!("alert-renderer-{:x}-{:x}", time::now_ms(), N.fetch_add(1, Ordering::Relaxed))
}

/// Posts the message; if Discord rejects it with the images, once more without.
fn deliver(
    discord: &Discord,
    mut payload: Value,
    files: Vec<(String, Vec<u8>)>,
    deadline: Instant,
) -> Result<(), SendError> {
    let b = boundary();
    match discord.post(&discord::multipart(&payload, &files, &b), &b, deadline) {
        Err(SendError::Rejected(code, body)) if !files.is_empty() => {
            log(
                json!({"level": "warn", "msg": "Discord rejected the message with charts; sending text only", "status": code, "body": body}),
            );
            payload["attachments"] = json!([]);
            for e in payload["embeds"].as_array_mut().into_iter().flatten() {
                if let Some(obj) = e.as_object_mut() {
                    obj.remove("image");
                }
            }
            discord.post(&discord::multipart(&payload, &[], &b), &b, deadline)
        }
        other => other,
    }
}

/// At most MAX_RENDERING holders at once.
struct Slots {
    busy: Mutex<usize>,
    freed: Condvar,
}

struct Slot<'a>(&'a Slots);

impl Slots {
    fn acquire(&self, until: Instant) -> Option<Slot<'_>> {
        let mut busy = self.busy.lock().unwrap_or_else(|e| e.into_inner());
        while *busy >= MAX_RENDERING {
            let left = until.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return None;
            }
            busy = self.freed.wait_timeout(busy, left).unwrap_or_else(|e| e.into_inner()).0;
        }
        *busy += 1;
        Some(Slot(self))
    }
}

impl Drop for Slot<'_> {
    fn drop(&mut self) {
        *self.0.busy.lock().unwrap_or_else(|e| e.into_inner()) -= 1;
        self.0.freed.notify_one();
    }
}

fn handle(mut req: tiny_http::Request, discord: &Discord, src: &Sources, cfg: &Config, slots: &Slots) {
    let arrived = Instant::now();
    let respond = |req: tiny_http::Request, code: u16, body: &str| {
        let _ = req.respond(tiny_http::Response::from_string(body).with_status_code(code));
    };
    match (req.method(), req.url()) {
        (tiny_http::Method::Get, "/healthz") => return respond(req, 200, "ok\n"),
        (tiny_http::Method::Post, "/alertmanager") => {}
        _ => return respond(req, 404, "not found\n"),
    }
    let mut body = Vec::new();
    if req.as_reader().take(MAX_BODY as u64 + 1).read_to_end(&mut body).is_err() || body.len() > MAX_BODY {
        return respond(req, 413, "payload too large or unreadable\n");
    }
    let n: Notification = match serde_json::from_slice(&body) {
        Ok(n) => n,
        Err(e) => return respond(req, 400, &format!("not an Alertmanager webhook payload: {e}\n")),
    };
    if n.alerts.is_empty() {
        return respond(req, 200, "nothing to send\n");
    }
    let Some(_slot) = slots.acquire(arrived + QUEUE_WAIT) else {
        return respond(req, 503, "busy\n");
    };
    let (payload, files) = render(&n, src, cfg, time::now_ms(), arrived + CHART_DEADLINE);
    let charts = files.len();
    let result = deliver(discord, payload, files, arrived + REQUEST_DEADLINE);
    let mut line = json!({
        "level": "info",
        "msg": "notification",
        "receiver": n.receiver,
        "group_key": n.group_key,
        "alerts": n.alerts.len(),
        "charts": charts,
        "ms": arrived.elapsed().as_millis() as u64,
    });
    match result {
        Ok(()) => {
            log(line);
            respond(req, 200, "sent\n");
        }
        Err(e) => {
            line["level"] = json!("error");
            line["error"] = json!(e.to_string());
            log(line);
            // Alertmanager retries on 5xx, not on 4xx; a rejected message would fail again.
            let code = if matches!(e, SendError::Rejected(..)) { 422 } else { 502 };
            respond(req, code, &format!("{e}\n"));
        }
    }
}

fn serve() {
    let cfg = Arc::new(config());
    let src = Arc::new(sources());
    let url_file =
        env("DISCORD_WEBHOOK_URL_FILE").unwrap_or_else(|| "/etc/alert-renderer/discord/url".into());
    let url = std::fs::read_to_string(&url_file)
        .unwrap_or_else(|e| fail(&format!("cannot read DISCORD_WEBHOOK_URL_FILE {url_file}: {}", e.kind())))
        .trim()
        .to_string();
    let allow_http = env("DISCORD_ALLOW_HTTP").as_deref() == Some("true");
    if !(url.starts_with("https://") || allow_http && url.starts_with("http://")) {
        fail("the Discord webhook URL must start with https://");
    }
    let discord = Arc::new(Discord::new(url, allow_http));
    let listen = env("LISTEN_ADDR").unwrap_or_else(|| "0.0.0.0:8080".into());
    let server =
        tiny_http::Server::http(&listen).unwrap_or_else(|e| fail(&format!("cannot listen on {listen}: {e}")));
    log(
        json!({"level": "info", "msg": "listening", "addr": listen, "vmalert": src.vmalert, "vmselect": src.vmselect}),
    );
    let in_flight = Arc::new(AtomicUsize::new(0));
    let slots = Arc::new(Slots { busy: Mutex::new(0), freed: Condvar::new() });
    for req in server.incoming_requests() {
        if in_flight.fetch_add(1, Ordering::SeqCst) >= MAX_ACCEPTED {
            in_flight.fetch_sub(1, Ordering::SeqCst);
            let _ = req.respond(tiny_http::Response::from_string("busy\n").with_status_code(503));
            continue;
        }
        let (cfg, src, discord, in_flight, slots) =
            (cfg.clone(), src.clone(), discord.clone(), in_flight.clone(), slots.clone());
        std::thread::spawn(move || {
            handle(req, &discord, &src, &cfg, &slots);
            in_flight.fetch_sub(1, Ordering::SeqCst);
        });
    }
}

/// `render IN OUT [NOW]`: the message and charts a payload would produce.
fn render_to_dir(args: &[String]) {
    let [input, out, rest @ ..] = args else {
        fail("usage: alert-renderer render PAYLOAD.json OUTDIR [NOW]")
    };
    let now = rest.first().map_or_else(time::now_ms, |t| {
        time::parse_rfc3339(t).unwrap_or_else(|| fail("NOW must be RFC 3339"))
    });
    let raw = std::fs::read(input).unwrap_or_else(|e| fail(&format!("{input}: {e}")));
    let n: Notification = serde_json::from_slice(&raw).unwrap_or_else(|e| fail(&format!("{input}: {e}")));
    let (payload, files) = render(&n, &sources(), &config(), now, Instant::now() + CHART_DEADLINE);
    std::fs::create_dir_all(out).unwrap_or_else(|e| fail(&format!("{out}: {e}")));
    let write = |name: &str, data: &[u8]| {
        std::fs::write(format!("{out}/{name}"), data).unwrap_or_else(|e| fail(&format!("{out}/{name}: {e}")))
    };
    write("message.json", serde_json::to_string_pretty(&payload).unwrap_or_default().as_bytes());
    for (name, png) in &files {
        write(name, png);
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        None => serve(),
        Some("render") => render_to_dir(&args[1..]),
        Some(other) => fail(&format!("unknown command {other}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_full_house_waits_for_a_slot_then_gives_up() {
        let slots = Slots { busy: Mutex::new(0), freed: Condvar::new() };
        let soon = || Instant::now() + Duration::from_millis(50);
        let held: Vec<_> = (0..MAX_RENDERING).map(|_| slots.acquire(soon()).unwrap()).collect();
        assert!(slots.acquire(soon()).is_none(), "full: gives up at its deadline");
        std::thread::scope(|s| {
            let waiter = s.spawn(|| slots.acquire(Instant::now() + Duration::from_secs(5)).is_some());
            std::thread::sleep(Duration::from_millis(50));
            drop(held);
            assert!(waiter.join().unwrap(), "a freed slot goes to the one waiting");
        });
    }

    #[test]
    fn conditions_as_written_when_short_else_in_words() {
        let c = |e: &str| condition(e, &expr::plan(e));
        assert_eq!(
            c("max by (m) (gpustack:model_running_instances) < max by (m) (gpustack:model_desired_instances)")
                .as_deref(),
            Some("max(gpustack:model_running_instances) < max(gpustack:model_desired_instances)")
        );
        let ratio = "sum(rate(nginx_ingress_controller_requests{status=~\"5..\"}[5m])) / sum(rate(nginx_ingress_controller_requests[5m])) > 0.02";
        assert_eq!(c(ratio).as_deref(), Some(ratio), "a ratio is not shown as a bare counter");
        let long = format!("{} > 0.02", ["sum(rate(nginx_ingress_controller_requests[5m]))"; 5].join(" + "));
        assert_eq!(c(&long).as_deref(), Some("nginx_ingress_controller_requests > 0.02"));
        assert_eq!(c("absent(up{job=\"mgr\"})").as_deref(), Some("absent(up{job=\"mgr\"})"));
    }
}
