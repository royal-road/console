//! Which labels a reader needs, and where in the console the alert leads.

use crate::payload::{Alert, Labels};
use crate::time::Millis;

/// Shown elsewhere (title, colour) or meaningless to a reader.
const DROPPED: &[&str] = &[
    "__name__",
    "alertname",
    "severity",
    "alertgroup",
    "alertstate",
    "prometheus",
    "exported_prometheus",
    "prometheus_replica",
    "receive",
    "tenant_id",
];

/// Scrape plumbing: kept, but on one small line.
const FOLDED: &[&str] = &[
    "job",
    "instance",
    "endpoint",
    "metrics_path",
    "scheme",
    "uid",
    "id",
    "image",
    "exported_job",
    "exported_instance",
    "exported_namespace",
    "exported_pod",
    "exported_service",
    "scrape_job",
    "service_instance_id",
    "pod_template_hash",
    "controller_revision_hash",
];

/// Identifying labels, most specific place first. Others follow alphabetically.
const ORDER: &[&str] = &[
    "namespace",
    "deployment",
    "k8s_deployment_name",
    "statefulset",
    "k8s_statefulset_name",
    "daemonset",
    "k8s_daemonset_name",
    "cronjob",
    "job_name",
    "pod",
    "container",
    "service",
    "ingress",
    "persistentvolumeclaim",
    "node",
    "host",
];

pub fn is_dropped(key: &str) -> bool {
    DROPPED.contains(&key) || key.starts_with("__")
}

pub fn is_folded(key: &str) -> bool {
    FOLDED.contains(&key)
}

/// Reading order: folded labels last, the known places first, then names
/// before other labels (`model_name` before `model_id`).
pub fn rank(key: &str) -> (bool, usize, bool, &str) {
    (is_folded(key), ORDER.iter().position(|o| *o == key).unwrap_or(ORDER.len()), !key.ends_with("name"), key)
}

pub type Pairs<'a> = Vec<(&'a str, &'a str)>;

/// Identifying labels in reading order, then folded ones; dropped labels go.
pub fn classify(labels: &Labels) -> (Pairs<'_>, Pairs<'_>) {
    let mut identifying: Vec<(&str, &str)> = labels
        .iter()
        .filter(|(k, _)| !is_dropped(k) && !is_folded(k))
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    identifying.sort_by_key(|(k, _)| rank(k));
    let folded = labels.iter().filter(|(k, _)| is_folded(k)).map(|(k, v)| (k.as_str(), v.as_str())).collect();
    (identifying, folded)
}

/// `Namespace`, `Deployment`, `PVC`, `Model name`.
pub fn title(key: &str) -> String {
    match key {
        "k8s_deployment_name" => return "Deployment".into(),
        "k8s_statefulset_name" => return "StatefulSet".into(),
        "k8s_daemonset_name" => return "DaemonSet".into(),
        "statefulset" => return "StatefulSet".into(),
        "daemonset" => return "DaemonSet".into(),
        "cronjob" => return "CronJob".into(),
        "job_name" => return "Job".into(),
        "persistentvolumeclaim" => return "PVC".into(),
        _ => {}
    }
    let spaced = key.replace('_', " ");
    let mut c = spaced.chars();
    c.next().map_or_else(String::new, |f| f.to_uppercase().chain(c).collect())
}

/// A workload the console's Observe tab can show.
#[derive(Debug, PartialEq, Eq)]
pub struct Workload<'a> {
    /// The console's path segment: `deployments`, `statefulsets`, ...
    pub plural: &'static str,
    pub namespace: &'a str,
    pub name: &'a str,
}

pub fn workload(labels: &Labels) -> Option<Workload<'_>> {
    let namespace = labels.get("namespace").filter(|v| !v.is_empty())?;
    const KINDS: &[(&str, &str)] = &[
        ("deployment", "deployments"),
        ("k8s_deployment_name", "deployments"),
        ("statefulset", "statefulsets"),
        ("k8s_statefulset_name", "statefulsets"),
        ("daemonset", "daemonsets"),
        ("k8s_daemonset_name", "daemonsets"),
        ("pod", "pods"),
    ];
    KINDS.iter().find_map(|(label, plural)| {
        let name = labels.get(*label).filter(|v| !v.is_empty())?;
        Some(Workload { plural, namespace, name })
    })
}

const OBSERVE_LEAD_MS: Millis = 30 * 60_000;

/// Where "Investigate" goes.
///
/// With a workload: its Observe tab focused on the alert, from 30 minutes
/// before it started (observe-plugin's deep link). A resolved alert gets a
/// closed window that ends 30 minutes after it did, so the link still shows
/// the incident later. Without one: observe-plugin's `/observe/alert` route,
/// which resolves the labels to a workload in the console or falls back to
/// Observe › Alerts.
pub fn console_link(console: &str, alert: &Alert) -> String {
    let console = console.trim_end_matches('/');
    let name = alert.alertname();
    let from = alert.starts_ms().map(|t| t - OBSERVE_LEAD_MS);
    if let Some(w) = workload(&alert.labels) {
        let mut url = format!(
            "{console}/k8s/ns/{}/{}/{}/observe?focus=alert:{}",
            enc(w.namespace),
            w.plural,
            enc(w.name),
            enc(name)
        );
        if let Some(from) = from {
            let to = alert.ended_ms().map_or("now".to_string(), |t| (t + OBSERVE_LEAD_MS).to_string());
            url.push_str(&format!("&from={from}&to={to}"));
        }
        return url;
    }
    let mut url = format!("{console}/observe/alert?alertname={}", enc(name));
    let (identifying, _) = classify(&alert.labels);
    // Long values (hashes, messages) do not identify a workload and would
    // only bloat the link.
    for (k, v) in identifying.iter().filter(|(_, v)| v.len() <= 100).take(8) {
        url.push_str(&format!("&{}={}", enc(k), enc(v)));
    }
    if let Some(start) = alert.starts_ms() {
        url.push_str(&format!("&activeAt={start}"));
    }
    url
}

/// Percent-encodes everything but RFC 3986 unreserved characters.
pub fn enc(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// A link annotation worth a button: `runbook_url`, `runbook`, `dashboard`.
pub fn runbook(alert: &Alert) -> Option<&str> {
    ["runbook_url", "runbook", "runbook_link"]
        .iter()
        .filter_map(|k| alert.annotations.get(*k))
        .map(|v| v.trim())
        .find(|v| v.starts_with("https://") || v.starts_with("http://"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn labels(pairs: &[(&str, &str)]) -> Labels {
        pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect()
    }

    fn alert(json: &str) -> Alert {
        serde_json::from_str(json).unwrap()
    }

    #[test]
    fn identifying_labels_first_noise_folded_or_dropped() {
        let l = labels(&[
            ("alertname", "TargetDown"),
            ("severity", "warning"),
            ("prometheus", "openshift-monitoring/k8s"),
            ("alertgroup", "general.rules"),
            ("job", "scrape/x"),
            ("instance", "192.0.2.1:9100"),
            ("model_id", "55"),
            ("pod", "api-1"),
            ("namespace", "royal-road"),
            ("container", "api"),
        ]);
        let (id, folded) = classify(&l);
        assert_eq!(
            id,
            [("namespace", "royal-road"), ("pod", "api-1"), ("container", "api"), ("model_id", "55")]
        );
        assert_eq!(folded, [("instance", "192.0.2.1:9100"), ("job", "scrape/x")]);
    }

    #[test]
    fn titles() {
        assert_eq!(title("namespace"), "Namespace");
        assert_eq!(title("k8s_deployment_name"), "Deployment");
        assert_eq!(title("model_name"), "Model name");
        assert_eq!(title("persistentvolumeclaim"), "PVC");
    }

    #[test]
    fn workload_from_labels() {
        let l = labels(&[("namespace", "royal-road"), ("k8s_deployment_name", "royalroad-api")]);
        assert_eq!(
            workload(&l),
            Some(Workload { plural: "deployments", namespace: "royal-road", name: "royalroad-api" })
        );
        let l = labels(&[("namespace", "sentry"), ("pod", "relay-0"), ("statefulset", "relay")]);
        assert_eq!(workload(&l).unwrap().plural, "statefulsets");
        assert_eq!(workload(&labels(&[("pod", "x")])), None);
        assert_eq!(workload(&labels(&[("namespace", "x"), ("service", "y")])), None);
    }

    #[test]
    fn observe_deep_link_for_a_firing_workload_alert() {
        let a = alert(
            r#"{"status":"firing","startsAt":"2026-11-24T14:40:00Z",
                "labels":{"alertname":"KubePodCrashLooping","namespace":"royal-road","deployment":"royalroad-api"}}"#,
        );
        assert_eq!(
            console_link("https://console.example.com/", &a),
            "https://console.example.com/k8s/ns/royal-road/deployments/royalroad-api/observe\
             ?focus=alert:KubePodCrashLooping&from=1795529400000&to=now"
        );
    }

    #[test]
    fn resolved_alerts_link_to_a_closed_window() {
        let a = alert(
            r#"{"status":"resolved","startsAt":"2026-11-24T14:40:00Z","endsAt":"2026-11-24T14:50:00Z",
                "labels":{"alertname":"X","namespace":"n","pod":"p"}}"#,
        );
        assert!(
            console_link("https://c", &a)
                .ends_with("/k8s/ns/n/pods/p/observe?focus=alert:X&from=1795529400000&to=1795533600000")
        );
    }

    #[test]
    fn without_a_workload_the_alert_route_resolves_it() {
        let a = alert(
            r#"{"status":"firing","startsAt":"2026-11-24T14:40:00Z",
                "labels":{"alertname":"IngressHigh5xxRate","namespace":"royal-road","ingress":"api",
                          "service":"royalroad-api","severity":"warning","prometheus":"p"}}"#,
        );
        assert_eq!(
            console_link("https://c", &a),
            "https://c/observe/alert?alertname=IngressHigh5xxRate&namespace=royal-road\
             &service=royalroad-api&ingress=api&activeAt=1795531200000"
        );
    }

    #[test]
    fn label_values_are_encoded() {
        let a = alert(r#"{"status":"firing","labels":{"alertname":"A&b","namespace":"x y","pod":"p/../q"}}"#);
        assert_eq!(
            console_link("https://c", &a),
            "https://c/k8s/ns/x%20y/pods/p%2F..%2Fq/observe?focus=alert:A%26b"
        );
    }

    #[test]
    fn runbook_only_when_it_is_a_web_link() {
        let a = alert(
            r#"{"status":"firing","annotations":{"runbook_url":"javascript:alert(1)","runbook":"https://r/x"}}"#,
        );
        assert_eq!(runbook(&a), Some("https://r/x"));
    }
}
