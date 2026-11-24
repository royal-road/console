//! Alertmanager's webhook payload (version 4) and what the service reads from it.

use std::collections::BTreeMap;

use serde::Deserialize;

use crate::time::{self, Millis};

pub type Labels = BTreeMap<String, String>;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Notification {
    #[serde(default)]
    pub receiver: String,
    #[serde(default)]
    pub group_key: String,
    #[serde(default)]
    pub truncated_alerts: u64,
    pub alerts: Vec<Alert>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Alert {
    pub status: String,
    #[serde(default)]
    pub labels: Labels,
    #[serde(default)]
    pub annotations: Labels,
    #[serde(default)]
    pub starts_at: String,
    #[serde(default)]
    pub ends_at: String,
    #[serde(default, rename = "generatorURL")]
    pub generator_url: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Severity {
    Critical,
    Warning,
    Info,
}

impl Severity {
    /// The rule's own `severity` label decides; `high`, `error` and `page`
    /// count as critical, anything unknown as info.
    pub fn from_label(v: Option<&str>) -> Severity {
        match v.map(str::to_ascii_lowercase).as_deref() {
            Some("critical" | "high" | "error" | "page" | "emergency" | "alert") => Severity::Critical,
            Some("warning" | "warn" | "medium") => Severity::Warning,
            _ => Severity::Info,
        }
    }
}

impl Alert {
    pub fn firing(&self) -> bool {
        self.status == "firing"
    }

    pub fn alertname(&self) -> &str {
        self.labels.get("alertname").map_or("(unnamed alert)", String::as_str)
    }

    /// The `severity` label as the rule wrote it.
    pub fn severity_label(&self) -> Option<&str> {
        self.labels.get("severity").map(String::as_str).filter(|s| !s.is_empty())
    }

    pub fn severity(&self) -> Severity {
        Severity::from_label(self.severity_label())
    }

    pub fn starts_ms(&self) -> Option<Millis> {
        time::parse_rfc3339(&self.starts_at).filter(|t| time::is_set(*t))
    }

    /// When a resolved alert ended. Firing alerts carry a future estimate in
    /// `endsAt`, which is not an end.
    pub fn ended_ms(&self) -> Option<Millis> {
        if self.firing() {
            return None;
        }
        time::parse_rfc3339(&self.ends_at).filter(|t| time::is_set(*t))
    }

    /// vmalert's `group_id` and `alert_id` from the generator URL. Only digits
    /// are accepted; the host part is ignored.
    pub fn vmalert_ids(&self) -> Option<(String, String)> {
        let query = self.generator_url.split_once('?')?.1;
        let query = query.split('#').next()?;
        let mut group = None;
        let mut alert = None;
        for pair in query.split('&') {
            let Some((k, v)) = pair.split_once('=') else { continue };
            if v.is_empty() || !v.bytes().all(|b| b.is_ascii_digit()) {
                continue;
            }
            match k {
                "group_id" => group = Some(v.to_string()),
                "alert_id" => alert = Some(v.to_string()),
                _ => {}
            }
        }
        Some((group?, alert?))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub const GPUSTACK: &str = include_str!("../testdata/payloads/gpustack-firing.json");

    #[test]
    fn parses_a_real_alertmanager_payload() {
        let n: Notification = serde_json::from_str(GPUSTACK).unwrap();
        assert_eq!(n.alerts.len(), 1);
        let a = &n.alerts[0];
        assert!(a.firing());
        assert_eq!(a.alertname(), "GPUStackModelUnderReplicated");
        assert_eq!(a.severity(), Severity::Critical);
        assert_eq!(a.starts_ms(), time::parse_rfc3339("2026-11-24T01:29:00Z"));
        assert_eq!(a.ended_ms(), None);
        assert_eq!(a.vmalert_ids(), Some(("3330991254042310896".into(), "8932090162394429460".into())));
    }

    #[test]
    fn generator_url_ids_must_be_digits() {
        let mut a: Alert = serde_json::from_str(r#"{"status":"firing"}"#).unwrap();
        assert_eq!(a.vmalert_ids(), None);
        a.generator_url = "http://x/vmalert/alert?group_id=1&alert_id=2".into();
        assert_eq!(a.vmalert_ids(), Some(("1".into(), "2".into())));
        a.generator_url = "http://x/vmalert/alert?group_id=1&alert_id=2%26x".into();
        assert_eq!(a.vmalert_ids(), None);
        a.generator_url = "http://prometheus/graph?g0.expr=up".into();
        assert_eq!(a.vmalert_ids(), None);
    }

    #[test]
    fn severity_from_the_rule_label() {
        assert_eq!(Severity::from_label(Some("high")), Severity::Critical);
        assert_eq!(Severity::from_label(Some("Warning")), Severity::Warning);
        assert_eq!(Severity::from_label(Some("none")), Severity::Info);
        assert_eq!(Severity::from_label(None), Severity::Info);
    }

    #[test]
    fn resolved_alerts_have_an_end() {
        let a: Alert = serde_json::from_str(
            r#"{"status":"resolved","startsAt":"2026-11-23T02:48:00Z","endsAt":"2026-11-23T02:52:30Z"}"#,
        )
        .unwrap();
        assert_eq!(a.ended_ms(), time::parse_rfc3339("2026-11-23T02:52:30Z"));
    }
}
