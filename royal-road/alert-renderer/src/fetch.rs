//! Reads the rule from vmalert and the chart data from vmselect.

use std::collections::BTreeMap;
use std::time::Duration;

use serde::Deserialize;
use ureq::Agent;

use crate::labels::enc;
use crate::payload::{Alert, Labels};
use crate::time::Millis;

const RULES_LIMIT: u64 = 4 << 20;
const RANGE_LIMIT: u64 = 2 << 20;

pub struct Sources {
    agent: Agent,
    /// vmalert including its path prefix, e.g. `http://vmalert:8080/vmalert`.
    pub vmalert: String,
    /// Prometheus API root, e.g. `http://vmselect:8481/select/0/prometheus`.
    pub vmselect: String,
}

#[derive(Clone, Debug)]
pub struct Series {
    pub labels: Labels,
    pub points: Vec<(Millis, f64)>,
}

impl Sources {
    pub fn new(vmalert: &str, vmselect: &str, timeout: Duration) -> Sources {
        let agent = Agent::config_builder()
            .timeout_global(Some(timeout))
            .timeout_connect(Some(Duration::from_secs(3)))
            .max_redirects(0)
            .proxy(None)
            .build()
            .new_agent();
        Sources {
            agent,
            vmalert: vmalert.trim_end_matches('/').to_string(),
            vmselect: vmselect.trim_end_matches('/').to_string(),
        }
    }

    fn get_json<T: for<'de> Deserialize<'de>>(&self, url: &str, limit: u64) -> Result<T, String> {
        let mut resp = self.agent.get(url).call().map_err(|e| e.to_string())?;
        let body = resp.body_mut().with_config().limit(limit).read_to_vec().map_err(|e| e.to_string())?;
        serde_json::from_slice(&body).map_err(|e| format!("bad JSON: {e}"))
    }

    /// The rule's expression. The active alert's endpoint knows it while the
    /// alert is firing; once it resolved, the rules list (filtered by name,
    /// then by the generator URL's group) does.
    pub fn expression(&self, alert: &Alert) -> Result<String, String> {
        let ids = alert.vmalert_ids();
        if let Some((group, id)) = &ids {
            #[derive(Deserialize)]
            struct ActiveAlert {
                expression: String,
            }
            let url = format!("{}/api/v1/alert?group_id={group}&alert_id={id}", self.vmalert);
            if let Ok(a) = self.get_json::<ActiveAlert>(&url, RULES_LIMIT) {
                return Ok(a.expression);
            }
        }
        #[derive(Deserialize)]
        struct Rules {
            data: RulesData,
        }
        #[derive(Deserialize)]
        struct RulesData {
            groups: Vec<Group>,
        }
        #[derive(Deserialize)]
        struct Group {
            #[serde(default)]
            id: String,
            #[serde(default)]
            name: String,
            rules: Vec<Rule>,
        }
        #[derive(Deserialize)]
        struct Rule {
            name: String,
            query: String,
            #[serde(default, rename = "type")]
            kind: String,
        }
        let name = alert.alertname();
        let url = format!("{}/api/v1/rules?type=alert&rule_name%5B%5D={}", self.vmalert, enc(name));
        let rules: Rules = self.get_json(&url, RULES_LIMIT)?;
        let group_id = ids.as_ref().map(|(g, _)| g.as_str());
        let alertgroup = alert.labels.get("alertgroup").map(String::as_str);
        let candidates = || {
            rules
                .data
                .groups
                .iter()
                .flat_map(|g| g.rules.iter().map(move |r| (g, r)))
                .filter(|(_, r)| r.name == name && (r.kind.is_empty() || r.kind == "alerting"))
        };
        candidates()
            .find(|(g, _)| Some(g.id.as_str()) == group_id)
            .or_else(|| candidates().find(|(g, _)| Some(g.name.as_str()) == alertgroup))
            .or_else(|| candidates().next())
            .map(|(_, r)| r.query.clone())
            .ok_or_else(|| format!("vmalert has no rule {name}"))
    }

    /// `query_range` over `[start, end]` with about 240 points.
    pub fn range(&self, query: &str, start: Millis, end: Millis) -> Result<Vec<Series>, String> {
        #[derive(Deserialize)]
        struct Resp {
            data: Data,
        }
        #[derive(Deserialize)]
        struct Data {
            result: Vec<Item>,
        }
        #[derive(Deserialize)]
        struct Item {
            #[serde(default)]
            metric: BTreeMap<String, String>,
            #[serde(default)]
            values: Vec<(f64, String)>,
        }
        let step = step_secs(start, end);
        let url = format!(
            "{}/api/v1/query_range?query={}&start={}&end={}&step={step}s",
            self.vmselect,
            enc(query),
            start / 1000,
            end / 1000,
        );
        let resp: Resp = self.get_json(&url, RANGE_LIMIT)?;
        Ok(resp
            .data
            .result
            .into_iter()
            .map(|i| Series {
                labels: i.metric,
                points: i
                    .values
                    .into_iter()
                    .filter_map(|(t, v)| Some(((t * 1000.0) as Millis, v.parse::<f64>().ok()?)))
                    .filter(|(_, v)| v.is_finite())
                    .collect(),
            })
            .filter(|s| !s.points.is_empty())
            .collect())
    }
}

/// A step that gives roughly 240 points, rounded to a whole 15 s.
pub fn step_secs(start: Millis, end: Millis) -> i64 {
    let raw = ((end - start) / 1000 / 240).max(15);
    (raw + 14) / 15 * 15
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn steps() {
        assert_eq!(step_secs(0, 3_600_000), 15);
        assert_eq!(step_secs(0, 6 * 3_600_000), 90);
        assert_eq!(step_secs(0, 25 * 3_600_000), 375);
    }
}
