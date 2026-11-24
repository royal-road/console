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

/// The labels kube-state-metrics' own scrape adds to series that name no pod
/// (`kube_deployment_*`, `kube_cronjob_*`): they describe the exporter, not
/// the alert's subject. When a series is about a pod, its `container` is that
/// pod's container, so this does not match.
const KSM_SCRAPE: &[&str] = &["pod", "container", "service"];

fn is_ksm_scrape(labels: &Labels) -> bool {
    let is = |k: &str| labels.get(k).map(String::as_str) == Some("kube-state-metrics");
    is("job") && is("container")
}

/// Identifying labels in reading order, then folded ones; dropped labels go.
pub fn classify(labels: &Labels) -> (Pairs<'_>, Pairs<'_>) {
    let ksm = is_ksm_scrape(labels);
    let folded_here = |k: &str| is_folded(k) || ksm && KSM_SCRAPE.contains(&k);
    let mut identifying: Vec<(&str, &str)> = labels
        .iter()
        .filter(|(k, _)| !is_dropped(k) && !folded_here(k))
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    identifying.sort_by_key(|(k, _)| rank(k));
    let folded =
        labels.iter().filter(|(k, _)| folded_here(k)).map(|(k, v)| (k.as_str(), v.as_str())).collect();
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
    let ksm = is_ksm_scrape(labels);
    KINDS.iter().filter(|(label, _)| !(ksm && *label == "pod")).find_map(|(label, plural)| {
        let name = labels.get(*label).filter(|v| !v.is_empty())?;
        Some(Workload { plural, namespace, name })
    })
}

const OBSERVE_LEAD_MS: Millis = 30 * 60_000;

/// Where "Investigate" goes: observe-plugin's deep link, from 30 minutes
/// before the alert started. A resolved alert gets a closed window that ends
/// 30 minutes after it did, so the link still shows the incident later.
///
/// The workload's Observe tab when the labels name one, else the namespace's
/// Observe tab, else Observe › Overview for the same window.
pub fn console_link(console: &str, alert: &Alert) -> String {
    let console = console.trim_end_matches('/');
    let focus = format!("focus=alert:{}", enc(alert.alertname()));
    let mut url = if let Some(w) = workload(&alert.labels) {
        format!("{console}/k8s/ns/{}/{}/{}/observe?{focus}", enc(w.namespace), w.plural, enc(w.name))
    } else if let Some(ns) = alert.labels.get("namespace").filter(|v| !v.is_empty()) {
        format!("{console}/k8s/cluster/namespaces/{}/observe?{focus}", enc(ns))
    } else {
        format!("{console}/observe/overview?")
    };
    if let Some(from) = alert.starts_ms().map(|t| t - OBSERVE_LEAD_MS) {
        let to = alert.ended_ms().map_or("now".to_string(), |t| (t + OBSERVE_LEAD_MS).to_string());
        if !url.ends_with('?') {
            url.push('&');
        }
        url.push_str(&format!("from={from}&to={to}"));
    }
    url.trim_end_matches('?').to_string()
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
    fn without_a_workload_the_namespace_observe_tab() {
        let a = alert(
            r#"{"status":"firing","startsAt":"2026-11-24T14:40:00Z",
                "labels":{"alertname":"IngressHigh5xxRate","namespace":"royal-road","ingress":"api",
                          "service":"royalroad-api","severity":"warning","prometheus":"p"}}"#,
        );
        assert_eq!(
            console_link("https://c", &a),
            "https://c/k8s/cluster/namespaces/royal-road/observe?focus=alert:IngressHigh5xxRate\
             &from=1795529400000&to=now"
        );
    }

    #[test]
    fn without_a_namespace_observe_overview() {
        let a = alert(
            r#"{"status":"firing","startsAt":"2026-11-24T14:40:00Z",
                "labels":{"alertname":"NodeFilesystemSpaceFillingUp","instance":"192.0.2.1:9100"}}"#,
        );
        assert_eq!(console_link("https://c", &a), "https://c/observe/overview?from=1795529400000&to=now");
        let a = alert(r#"{"status":"firing","labels":{"alertname":"X"}}"#);
        assert_eq!(console_link("https://c", &a), "https://c/observe/overview");
    }

    /// AbuseDetectorNotSucceeding as it fired in production: kube-state-metrics'
    /// scrape adds its own pod, container and service to a CronJob series.
    #[test]
    fn kube_state_metrics_scrape_labels_are_not_the_subject() {
        let a = alert(
            r#"{"status":"firing","startsAt":"2026-11-24T14:40:00Z",
                "labels":{"alertname":"AbuseDetectorNotSucceeding","namespace":"abuse-detection",
                          "cronjob":"abuse-detector","job":"kube-state-metrics","container":"kube-state-metrics",
                          "pod":"kube-state-metrics-79bc4974d-5f5qv","service":"kube-state-metrics",
                          "instance":"192.0.2.7:8443","severity":"warning"}}"#,
        );
        assert_eq!(workload(&a.labels), None);
        let (id, folded) = classify(&a.labels);
        assert_eq!(id, [("namespace", "abuse-detection"), ("cronjob", "abuse-detector")]);
        assert!(folded.contains(&("pod", "kube-state-metrics-79bc4974d-5f5qv")));
        assert!(console_link("https://c", &a).starts_with(
            "https://c/k8s/cluster/namespaces/abuse-detection/observe?focus=alert:AbuseDetectorNotSucceeding&"
        ));
        // A deployment label still wins over the exporter's pod.
        let l = labels(&[
            ("namespace", "sentry"),
            ("deployment", "snuba-consumer"),
            ("job", "kube-state-metrics"),
            ("container", "kube-state-metrics"),
            ("pod", "kube-state-metrics-1"),
        ]);
        assert_eq!(workload(&l).unwrap().name, "snuba-consumer");
        // A series about the exporter's own pod (no scrape job) keeps its pod.
        let l = labels(&[
            ("namespace", "openshift-monitoring"),
            ("pod", "kube-state-metrics-1"),
            ("container", "kube-state-metrics"),
        ]);
        assert_eq!(workload(&l).unwrap().plural, "pods");
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
