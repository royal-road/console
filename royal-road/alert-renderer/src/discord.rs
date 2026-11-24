//! The Discord message: one embed per alert rule in the notification, each
//! with its chart attached, and the multipart request that posts it.

use std::time::{Duration, Instant};

use serde_json::{Value, json};
use ureq::Agent;

use crate::chart::truncate;
use crate::labels;
use crate::payload::{Alert, Severity};
use crate::time;

/// Discord's limits (https://discord.com/developers/docs/resources/message#embed-object-embed-limits).
const MAX_EMBEDS: usize = 10;
const MAX_TOTAL: usize = 6000;
const MAX_DESCRIPTION: usize = 4096;
const MAX_FIELD_VALUE: usize = 1024;
const MAX_FIELDS_INLINE: usize = 9;
const MAX_LISTED: usize = 10;

const GREEN: u32 = 0x0ca30c;

/// One alert rule's alerts in this notification and what was learned about it.
pub struct Section<'a> {
    pub alerts: Vec<&'a Alert>,
    /// `running < desired`, `> 0.02`: the rule's condition, when known.
    pub condition: Option<String>,
    /// File name of the attached chart.
    pub chart: Option<String>,
    /// Why there is no chart, for the footer.
    pub chart_note: Option<String>,
}

impl Section<'_> {
    fn alertname(&self) -> &str {
        self.alerts[0].alertname()
    }

    fn firing(&self) -> usize {
        self.alerts.iter().filter(|a| a.firing()).count()
    }

    pub fn severity(&self) -> Severity {
        self.alerts.iter().map(|a| a.severity()).min().unwrap_or(Severity::Info)
    }
}

/// Splits alerts by rule: firing first, then by severity and name.
pub fn sections(alerts: &[Alert]) -> Vec<Section<'_>> {
    let mut out: Vec<Section> = Vec::new();
    for a in alerts {
        match out.iter_mut().find(|s| s.alertname() == a.alertname()) {
            Some(s) => s.alerts.push(a),
            None => out.push(Section { alerts: vec![a], condition: None, chart: None, chart_note: None }),
        }
    }
    out.sort_by(|a, b| {
        (a.firing() == 0, a.severity(), a.alertname()).cmp(&(b.firing() == 0, b.severity(), b.alertname()))
    });
    out
}

fn colour(s: &Section) -> u32 {
    if s.firing() == 0 {
        return GREEN;
    }
    let c = crate::chart::severity_colour(s.severity());
    u32::from(c.0) << 16 | u32::from(c.1) << 8 | u32::from(c.2)
}

/// Escapes Discord markdown in label values.
pub fn escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for (i, c) in s.chars().enumerate() {
        let special = matches!(c, '\\' | '*' | '_' | '~' | '`' | '|' | '[' | ']' | '(' | ')' | '<' | '@')
            // Lists, headings and quotes only start a line.
            || (i == 0 && matches!(c, '-' | '#' | '>'));
        if special {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// Annotation text keeps its markdown (rules write `**bold**`), but a
/// templated label value must not turn it into a masked link.
fn annotation(s: &str) -> String {
    s.trim().replace("](", "]\\(")
}

fn ts(ms: i64) -> String {
    format!("<t:{}:R>", ms.div_euclid(1000))
}

fn title(s: &Section) -> String {
    let (n, firing) = (s.alerts.len(), s.firing());
    let state = match (firing, n) {
        (0, 1) => "Resolved".to_string(),
        (0, n) => format!("Resolved ({n})"),
        (f, n) if f == n && n == 1 => "Firing".to_string(),
        (f, n) if f == n => format!("Firing ({n})"),
        (f, n) => format!("{f} firing, {} resolved", n - f),
    };
    truncate(&format!("{state}: {}", s.alertname()), 256)
}

/// Labels with the same value on every alert of the section.
fn common<'a>(s: &Section<'a>) -> Vec<(&'a str, &'a str)> {
    let (first, _) = labels::classify(&s.alerts[0].labels);
    first
        .into_iter()
        .filter(|(k, v)| s.alerts.iter().all(|a| a.labels.get(*k).map(String::as_str) == Some(*v)))
        .collect()
}

fn link_text(a: &Alert) -> String {
    match (labels::workload(&a.labels), a.labels.get("namespace").filter(|n| !n.is_empty())) {
        (Some(w), _) => format!("Investigate {} in Observe", escape(w.name)),
        (None, Some(ns)) => format!("Investigate namespace {} in Observe", escape(ns)),
        (None, None) => "Open Observe › Overview".to_string(),
    }
}

fn links(a: &Alert, console: &str) -> String {
    let mut out = format!("[{}]({})", link_text(a), labels::console_link(console, a));
    if let Some(r) = labels::runbook(a) {
        out.push_str(&format!(" · [Runbook]({r})"));
    }
    out
}

fn description(s: &Section, console: &str, budget: usize) -> String {
    let a0 = s.alerts[0];
    let mut text = String::new();
    if s.alerts.len() == 1 {
        let summary = a0.annotations.get("summary").or_else(|| a0.annotations.get("message"));
        let desc = a0.annotations.get("description").filter(|d| Some(*d) != summary);
        if let Some(summary) = summary {
            text.push_str(&format!("**{}**\n", annotation(summary)));
        }
        if let Some(d) = desc {
            text.push_str(&annotation(d));
            text.push('\n');
        }
    } else {
        let same = |k: &str| {
            let v = a0.annotations.get(k)?;
            s.alerts.iter().all(|a| a.annotations.get(k) == Some(v)).then_some(v)
        };
        if let Some(summary) = same("summary") {
            text.push_str(&format!("**{}**\n", annotation(summary)));
        }
        let common_keys: Vec<&str> = common(s).iter().map(|(k, _)| *k).collect();
        for a in s.alerts.iter().take(MAX_LISTED) {
            let what = match a.annotations.get("summary").filter(|_| same("summary").is_none()) {
                Some(summary) => annotation(summary),
                None => {
                    let (id, _) = labels::classify(&a.labels);
                    let parts: Vec<String> = id
                        .iter()
                        .filter(|(k, _)| !common_keys.contains(k))
                        .take(3)
                        .map(|(k, v)| format!("{k}={}", escape(v)))
                        .collect();
                    if parts.is_empty() { escape(a.alertname()) } else { parts.join(", ") }
                }
            };
            let when = match (a.firing(), a.starts_ms(), a.ended_ms()) {
                (true, Some(t), _) => format!("since {}", ts(t)),
                (false, Some(t), Some(e)) => format!("resolved after {}", time::human_duration(e - t)),
                _ => String::new(),
            };
            text.push_str(&format!(
                "• {} · {when} · [Observe]({})\n",
                truncate(&what, 160),
                labels::console_link(console, a)
            ));
        }
        if s.alerts.len() > MAX_LISTED {
            text.push_str(&format!("…and {} more\n", s.alerts.len() - MAX_LISTED));
        }
    }
    let link_line = if s.alerts.len() == 1 { links(a0, console) } else { String::new() };
    let room = budget.saturating_sub(link_line.chars().count() + 2);
    let mut text = truncate(text.trim_end(), room);
    if !link_line.is_empty() {
        text.push_str("\n\n");
        text.push_str(&link_line);
    }
    text
}

fn fields(s: &Section, value_cap: usize) -> Vec<Value> {
    let mut out: Vec<Value> = common(s)
        .into_iter()
        .take(MAX_FIELDS_INLINE)
        .map(|(k, v)| json!({"name": labels::title(k), "value": truncate(&escape(v), value_cap), "inline": true}))
        .collect();
    let starts = s.alerts.iter().filter_map(|a| a.starts_ms()).min();
    let ends =
        s.alerts.iter().map(|a| a.ended_ms()).collect::<Option<Vec<_>>>().and_then(|e| e.into_iter().max());
    match (s.firing(), starts, ends) {
        (0, Some(t), Some(e)) => {
            out.push(json!({"name": "Lasted", "value": format!("{} (ended {})", time::human_duration(e - t), ts(e)), "inline": true}))
        }
        (_, Some(t), _) => out.push(json!({"name": "Since", "value": ts(t), "inline": true})),
        _ => {}
    }
    if let Some(c) = &s.condition {
        out.push(json!({"name": "Condition", "value": truncate(&format!("`{}`", c.replace('`', "'")), value_cap), "inline": false}));
    }
    out
}

fn footer(s: &Section) -> String {
    let (_, folded) = labels::classify(&s.alerts[0].labels);
    let mut parts: Vec<String> = folded
        .iter()
        .filter(|(k, v)| s.alerts.iter().all(|a| a.labels.get(*k).map(String::as_str) == Some(*v)))
        .map(|(k, v)| format!("{k}={v}"))
        .collect();
    if let Some(note) = &s.chart_note {
        parts.push(format!("no chart: {note}"));
    }
    truncate(&parts.join(" · "), 300)
}

fn embed(s: &Section, console: &str, description_budget: usize) -> Value {
    let a0 = s.alerts[0];
    let severity = s
        .alerts
        .iter()
        .min_by_key(|a| a.severity())
        .and_then(|a| a.severity_label())
        .unwrap_or("no severity");
    // Severity as the rule labels it, and the rule group.
    let author = match a0.labels.get("alertgroup") {
        Some(g) => format!("{} · {g}", severity.to_uppercase()),
        None => severity.to_uppercase(),
    };
    let mut e = json!({
        "title": title(s),
        "url": labels::console_link(console, a0),
        "color": colour(s),
        "author": {"name": truncate(&author, 256)},
        "description": description(s, console, description_budget),
        "fields": fields(s, (description_budget / 2).clamp(40, MAX_FIELD_VALUE)),
    });
    let footer = footer(s);
    if !footer.is_empty() {
        e["footer"] = json!({"text": footer});
    }
    let when = if s.firing() == 0 {
        s.alerts.iter().filter_map(|a| a.ended_ms()).max()
    } else {
        s.alerts.iter().filter_map(|a| a.starts_ms()).min()
    };
    if let Some(t) = when {
        e["timestamp"] = json!(time::rfc3339(t));
    }
    if let Some(file) = &s.chart {
        e["image"] = json!({"url": format!("attachment://{file}")});
    }
    e
}

/// Characters Discord counts against the 6000 total.
fn counted(e: &Value) -> usize {
    let len = |v: &Value| v.as_str().map_or(0, |s| s.chars().count());
    len(&e["title"])
        + len(&e["description"])
        + len(&e["author"]["name"])
        + len(&e["footer"]["text"])
        + e["fields"].as_array().map_or(0, |f| f.iter().map(|f| len(&f["name"]) + len(&f["value"])).sum())
}

/// The webhook message (`payload_json`).
pub fn message(sections: &[Section], console: &str, truncated: u64) -> Value {
    let mut shown = sections.len().min(MAX_EMBEDS - 1);
    if sections.len() == MAX_EMBEDS && truncated == 0 {
        shown = MAX_EMBEDS;
    }
    let mut budget = MAX_DESCRIPTION.min(1800);
    // Shorten descriptions and values until the message fits; if even the
    // shortest do not, show fewer rules.
    let embeds = loop {
        let mut embeds: Vec<Value> = sections[..shown].iter().map(|s| embed(s, console, budget)).collect();
        let rest: Vec<&str> = sections[shown..].iter().map(|s| s.alertname()).collect();
        let more = rest.len() + truncated as usize;
        if more > 0 {
            let names = truncate(&rest.iter().map(|n| escape(n)).collect::<Vec<_>>().join(", "), 600);
            embeds.push(json!({
                "title": format!("{more} more alerts in this notification"),
                "description": if names.is_empty() { "Alertmanager truncated the list.".to_string() } else { names },
                "color": 0x949ba4,
            }));
        }
        let total: usize = embeds.iter().map(counted).sum();
        if total <= MAX_TOTAL || shown == 0 {
            break embeds;
        }
        if budget > 120 {
            budget = budget * 2 / 3;
        } else {
            shown -= 1;
        }
    };
    let attachments: Vec<Value> = sections[..shown]
        .iter()
        .filter_map(|s| s.chart.as_ref())
        .enumerate()
        .map(|(i, f)| json!({"id": i, "filename": f}))
        .collect();
    json!({
        "embeds": embeds,
        "attachments": attachments,
        // Label and annotation text must never ping anyone.
        "allowed_mentions": {"parse": []},
    })
}

/// `multipart/form-data` with `payload_json` and `files[i]`.
pub fn multipart(payload: &Value, files: &[(String, Vec<u8>)], boundary: &str) -> Vec<u8> {
    let mut body = Vec::new();
    body.extend_from_slice(
        format!("--{boundary}\r\nContent-Disposition: form-data; name=\"payload_json\"\r\nContent-Type: application/json\r\n\r\n")
            .as_bytes(),
    );
    body.extend_from_slice(payload.to_string().as_bytes());
    body.extend_from_slice(b"\r\n");
    for (i, (name, data)) in files.iter().enumerate() {
        body.extend_from_slice(
            format!(
                "--{boundary}\r\nContent-Disposition: form-data; name=\"files[{i}]\"; filename=\"{name}\"\r\nContent-Type: image/png\r\n\r\n"
            )
            .as_bytes(),
        );
        body.extend_from_slice(data);
        body.extend_from_slice(b"\r\n");
    }
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
    body
}

pub struct Discord {
    agent: Agent,
    url: String,
}

#[derive(Debug)]
pub enum SendError {
    /// Discord refused the message itself (4xx other than 429).
    Rejected(u16, String),
    /// Worth a retry by Alertmanager.
    Transient(String),
}

impl std::fmt::Display for SendError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SendError::Rejected(code, body) => write!(f, "Discord rejected the message ({code}): {body}"),
            SendError::Transient(e) => write!(f, "Discord unavailable: {e}"),
        }
    }
}

impl Discord {
    /// `url` is the webhook URL; it is a secret and never logged.
    pub fn new(url: String, allow_http: bool) -> Discord {
        let agent = Agent::config_builder()
            .timeout_connect(Some(Duration::from_secs(5)))
            .https_only(!allow_http)
            .http_status_as_error(false)
            .max_redirects(0)
            .proxy(None)
            .build()
            .new_agent();
        Discord { agent, url }
    }

    /// Posts once, within `deadline`. Alertmanager retries what fails
    /// transiently (a 429 included) with its own backoff.
    pub fn post(&self, body: &[u8], boundary: &str, deadline: Instant) -> Result<(), SendError> {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(SendError::Transient("no time left".into()));
        }
        let mut resp = self
            .agent
            .post(&self.url)
            .config()
            .timeout_global(Some(left))
            .build()
            .header("Content-Type", format!("multipart/form-data; boundary={boundary}"))
            .header("User-Agent", "alert-renderer")
            .send(body)
            // ureq's errors name the host, not the token path; keep only the kind.
            .map_err(|e| SendError::Transient(error_kind(&e)))?;
        let status = resp.status().as_u16();
        let text = resp.body_mut().with_config().limit(64 * 1024).read_to_string().unwrap_or_default();
        match status {
            200..=299 => Ok(()),
            429 => Err(SendError::Transient("rate limited".into())),
            400..=499 => Err(SendError::Rejected(status, truncate(&text, 300))),
            _ => Err(SendError::Transient(format!("status {status}"))),
        }
    }
}

fn error_kind(e: &ureq::Error) -> String {
    match e {
        ureq::Error::Timeout(t) => format!("timeout ({t})"),
        ureq::Error::Io(io) => format!("io: {}", io.kind()),
        ureq::Error::HostNotFound => "host not found".into(),
        ureq::Error::ConnectionFailed => "connection failed".into(),
        ureq::Error::Tls(_) | ureq::Error::Rustls(_) => "TLS error".into(),
        _ => "request failed".into(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::payload::Notification;

    const CONSOLE: &str = "https://console.example.com";

    fn alerts(json: &str) -> Vec<Alert> {
        serde_json::from_str::<Notification>(json).unwrap().alerts
    }

    fn crashloop(pod: &str, status: &str) -> String {
        format!(
            r#"{{"status":"{status}","labels":{{"alertname":"KubePodCrashLooping","severity":"warning","namespace":"sentry",
               "pod":"{pod}","container":"consumer","job":"kube-state-metrics","prometheus":"openshift-monitoring/k8s"}},
               "annotations":{{"summary":"Pod sentry/{pod} is crash looping"}},
               "startsAt":"2026-11-24T10:00:00Z","endsAt":"2026-11-24T10:20:00Z"}}"#
        )
    }

    #[test]
    fn single_firing_alert() {
        let a = alerts(include_str!("../testdata/payloads/gpustack-firing.json"));
        let mut s = sections(&a);
        s[0].condition = Some("gpustack:model_running_instances < gpustack:model_desired_instances".into());
        s[0].chart = Some("chart-0.png".into());
        let m = message(&s, CONSOLE, 0);
        let e = &m["embeds"][0];
        assert_eq!(e["title"], "Firing: GPUStackModelUnderReplicated");
        assert_eq!(e["color"], 0xd03b3b);
        assert_eq!(e["author"]["name"], "CRITICAL · gpustack-serving");
        let d = e["description"].as_str().unwrap();
        assert!(
            d.starts_with("**GPUStack model qwen38-sglang-tp2-stable1 is under-replicated (0 running)**\n")
        );
        assert!(d.contains("[Open Observe › Overview](https://console.example.com/observe/overview?from="));
        let names: Vec<&str> =
            e["fields"].as_array().unwrap().iter().map(|f| f["name"].as_str().unwrap()).collect();
        assert_eq!(names, ["Service", "Cluster name", "Model name", "Model id", "Since", "Condition"]);
        assert_eq!(e["fields"][4]["value"], "<t:1795483740:R>");
        assert!(e.get("footer").is_none(), "nothing folded, no footer");
        assert_eq!(e["image"]["url"], "attachment://chart-0.png");
        assert_eq!(m["attachments"][0]["filename"], "chart-0.png");
        assert_eq!(m["allowed_mentions"]["parse"], json!([]));
        assert!(!m.to_string().contains("vmalert-pod"), "the vmalert pod link never reaches Discord");
    }

    #[test]
    fn resolved_alert_is_green_and_says_how_long() {
        let a = alerts(&format!(r#"{{"alerts":[{}]}}"#, crashloop("relay-0", "resolved")));
        let m = message(&sections(&a), CONSOLE, 0);
        let e = &m["embeds"][0];
        assert_eq!(e["title"], "Resolved: KubePodCrashLooping");
        assert_eq!(e["color"], GREEN);
        let lasted = e["fields"].as_array().unwrap().iter().find(|f| f["name"] == "Lasted").unwrap();
        assert_eq!(lasted["value"], "20m (ended <t:1795515600:R>)");
        assert!(
            e["url"]
                .as_str()
                .unwrap()
                .contains("/k8s/ns/sentry/pods/relay-0/observe?focus=alert:KubePodCrashLooping")
        );
        assert_eq!(e["footer"]["text"], "job=kube-state-metrics");
    }

    #[test]
    fn grouped_alerts_share_labels_and_list_instances() {
        let a = alerts(&format!(
            r#"{{"alerts":[{},{},{}]}}"#,
            crashloop("c-0", "firing"),
            crashloop("c-1", "firing"),
            crashloop("c-2", "firing")
        ));
        let m = message(&sections(&a), CONSOLE, 0);
        let e = &m["embeds"][0];
        assert_eq!(e["title"], "Firing (3): KubePodCrashLooping");
        let names: Vec<&str> =
            e["fields"].as_array().unwrap().iter().map(|f| f["name"].as_str().unwrap()).collect();
        assert_eq!(names, ["Namespace", "Container", "Since"], "pod differs, so it is listed, not a field");
        let d = e["description"].as_str().unwrap();
        assert_eq!(d.lines().filter(|l| l.starts_with("• Pod sentry/c-")).count(), 3);
        assert!(d.contains("[Observe](https://console.example.com/k8s/ns/sentry/pods/c-1/observe?"));
    }

    #[test]
    fn mixed_rules_get_one_embed_each_firing_first() {
        let a = alerts(&format!(
            r#"{{"alerts":[{},{{"status":"firing","labels":{{"alertname":"B","severity":"critical"}}}}]}}"#,
            crashloop("c-0", "resolved")
        ));
        let m = message(&sections(&a), CONSOLE, 0);
        assert_eq!(m["embeds"][0]["title"], "Firing: B");
        assert_eq!(m["embeds"][1]["title"], "Resolved: KubePodCrashLooping");
    }

    #[test]
    fn stays_within_discord_limits() {
        let long = "x".repeat(5000);
        let many: Vec<String> = (0..14)
            .map(|i| {
                format!(
                    r#"{{"status":"firing","labels":{{"alertname":"A{i}","severity":"warning","service":"{long}"}},
                        "annotations":{{"description":"{long}"}}}}"#
                )
            })
            .collect();
        let a = alerts(&format!(r#"{{"alerts":[{}],"truncatedAlerts":3}}"#, many.join(",")));
        let m = message(&sections(&a), CONSOLE, 3);
        let embeds = m["embeds"].as_array().unwrap();
        assert_eq!(embeds.len(), 10);
        assert_eq!(embeds[9]["title"], "8 more alerts in this notification");
        assert!(embeds.iter().map(counted).sum::<usize>() <= MAX_TOTAL);
        for e in embeds {
            assert!(e["description"].as_str().unwrap().chars().count() <= MAX_DESCRIPTION);
            for f in e["fields"].as_array().into_iter().flatten() {
                assert!(f["value"].as_str().unwrap().chars().count() <= MAX_FIELD_VALUE);
            }
        }
    }

    #[test]
    fn label_values_cannot_inject_markdown_or_mentions() {
        let a = alerts(
            r#"{"alerts":[{"status":"firing","labels":{"alertname":"X","service":"@everyone [x](https://evil)"}}]}"#,
        );
        let m = message(&sections(&a), CONSOLE, 0);
        assert_eq!(m["embeds"][0]["fields"][0]["value"], "\\@everyone \\[x\\]\\(https://evil\\)");
        assert_eq!(m["allowed_mentions"]["parse"], json!([]));
    }

    #[test]
    fn annotations_cannot_carry_masked_links() {
        let a = alerts(
            r#"{"alerts":[{"status":"firing","labels":{"alertname":"X"},
                "annotations":{"summary":"5xx on [/login](https://evil)","description":"route **[a](https://evil)**"}}]}"#,
        );
        let d = message(&sections(&a), CONSOLE, 0)["embeds"][0]["description"].as_str().unwrap().to_string();
        assert!(d.starts_with("**5xx on [/login]\\(https://evil)**\nroute **[a]\\(https://evil)**"), "{d}");
    }

    #[test]
    fn multipart_body() {
        let body = multipart(&json!({"a": 1}), &[("chart-0.png".into(), vec![0x89, b'P'])], "B");
        let text = String::from_utf8_lossy(&body);
        assert!(text.starts_with("--B\r\nContent-Disposition: form-data; name=\"payload_json\""));
        assert!(text.contains("name=\"files[0]\"; filename=\"chart-0.png\"\r\nContent-Type: image/png"));
        assert!(text.ends_with("--B--\r\n"));
    }
}
