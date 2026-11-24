//! The few time conversions the service needs, without a date library.

use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Milliseconds since the Unix epoch.
pub type Millis = i64;

pub fn now_ms() -> Millis {
    SystemTime::now().duration_since(UNIX_EPOCH).map_or(0, |d| d.as_millis() as Millis)
}

/// Parses RFC 3339 as Alertmanager writes it (`2026-11-24T01:14:00.123Z`,
/// `...+01:00`). Go's zero time (`0001-01-01T00:00:00Z`) parses to a
/// negative value; callers treat anything before 1971 as unset.
pub fn parse_rfc3339(s: &str) -> Option<Millis> {
    let b = s.as_bytes();
    if b.len() < 20 || b[4] != b'-' || b[7] != b'-' || !matches!(b[10], b'T' | b't' | b' ') {
        return None;
    }
    let num = |r: std::ops::Range<usize>| s.get(r)?.parse::<i64>().ok();
    let (y, mo, d) = (num(0..4)?, num(5..7)?, num(8..10)?);
    let (h, mi, sec) = (num(11..13)?, num(14..16)?, num(17..19)?);
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 60 {
        return None;
    }
    let mut i = 19;
    let mut ms = 0;
    if b.get(i) == Some(&b'.') {
        i += 1;
        let digits = b[i..].iter().take_while(|c| c.is_ascii_digit()).count();
        if digits == 0 {
            return None;
        }
        let frac = &s[i..i + digits.min(3)];
        ms = frac.parse::<i64>().ok()? * 10_i64.pow(3 - frac.len() as u32);
        i += digits;
    }
    let offset_min = match b.get(i)? {
        b'Z' | b'z' if i + 1 == b.len() => 0,
        sign @ (b'+' | b'-') if i + 6 == b.len() && b[i + 3] == b':' => {
            let off = num(i + 1..i + 3)? * 60 + num(i + 4..i + 6)?;
            if *sign == b'+' { off } else { -off }
        }
        _ => return None,
    };
    let days = days_from_civil(y, mo, d);
    Some(((days * 86400 + h * 3600 + mi * 60 + sec - offset_min * 60) * 1000) + ms)
}

/// `true` when `ms` is a real time rather than Go's zero time.
pub fn is_set(ms: Millis) -> bool {
    ms > 365 * 86_400_000
}

/// Howard Hinnant's days_from_civil.
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

fn civil_from_days(z: i64) -> (i64, i64, i64) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { yoe + era * 400 + 1 } else { yoe + era * 400 }, m, d)
}

/// `HH:MM` in UTC.
pub fn hhmm(ms: Millis) -> String {
    let secs = ms.div_euclid(1000);
    let (h, m) = (secs.rem_euclid(86400) / 3600, secs.rem_euclid(3600) / 60);
    format!("{h:02}:{m:02}")
}

/// `MM-DD` in UTC.
pub fn mmdd(ms: Millis) -> String {
    let (_, m, d) = civil_from_days(ms.div_euclid(86_400_000));
    format!("{m:02}-{d:02}")
}

/// `RFC 3339` in UTC with seconds, for logs.
pub fn rfc3339(ms: Millis) -> String {
    let (y, mo, d) = civil_from_days(ms.div_euclid(86_400_000));
    let secs = ms.div_euclid(1000).rem_euclid(86400);
    format!("{y:04}-{mo:02}-{d:02}T{:02}:{:02}:{:02}Z", secs / 3600, secs % 3600 / 60, secs % 60)
}

/// Parses `90s`, `5m`, `1h30m`, `2d`, `1w` and `250ms`.
pub fn parse_duration(s: &str) -> Option<Duration> {
    let mut total = 0.0;
    let mut rest = s.trim();
    if rest.is_empty() {
        return None;
    }
    while !rest.is_empty() {
        let n = rest.find(|c: char| !(c.is_ascii_digit() || c == '.')).unwrap_or(rest.len());
        let value: f64 = rest[..n].parse().ok()?;
        rest = &rest[n..];
        let u = rest.find(|c: char| c.is_ascii_digit()).unwrap_or(rest.len());
        let unit = match &rest[..u] {
            "ms" => 0.001,
            "s" => 1.0,
            "m" => 60.0,
            "h" => 3600.0,
            "d" => 86400.0,
            "w" => 604_800.0,
            _ => return None,
        };
        rest = &rest[u..];
        total += value * unit;
    }
    Some(Duration::from_secs_f64(total))
}

/// `14m`, `2h 5m`, `3d 4h`: how long something lasted.
pub fn human_duration(ms: Millis) -> String {
    let s = ms.max(0) / 1000;
    let (d, h, m) = (s / 86400, s % 86400 / 3600, s % 3600 / 60);
    match (d, h, m) {
        (0, 0, 0) => format!("{s}s"),
        (0, 0, m) => format!("{m}m"),
        (0, h, 0) => format!("{h}h"),
        (0, h, m) => format!("{h}h {m}m"),
        (d, 0, _) => format!("{d}d"),
        (d, h, _) => format!("{d}d {h}h"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rfc3339_forms() {
        assert_eq!(parse_rfc3339("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(parse_rfc3339("2026-11-24T01:14:00Z"), Some(1_795_482_840_000));
        assert_eq!(parse_rfc3339("2026-11-24T01:14:00.5Z"), Some(1_795_482_840_500));
        assert_eq!(parse_rfc3339("2026-11-24T01:14:00.123456789Z"), Some(1_795_482_840_123));
        assert_eq!(parse_rfc3339("2026-11-24T02:14:00+01:00"), Some(1_795_482_840_000));
        assert_eq!(parse_rfc3339("2026-11-24T00:14:00-01:00"), Some(1_795_482_840_000));
        assert!(!is_set(parse_rfc3339("0001-01-01T00:00:00Z").unwrap()));
        assert_eq!(parse_rfc3339("2026-13-01T00:00:00Z"), None);
        assert_eq!(parse_rfc3339("yesterday"), None);
        assert_eq!(parse_rfc3339("2026-11-24T01:14:00"), None);
    }

    #[test]
    fn formatting_round_trips() {
        let t = parse_rfc3339("2026-11-24T01:14:09Z").unwrap();
        assert_eq!(rfc3339(t), "2026-11-24T01:14:09Z");
        assert_eq!(hhmm(t), "01:14");
        assert_eq!(mmdd(t), "11-24");
        assert_eq!(rfc3339(parse_rfc3339("2024-02-29T23:59:59Z").unwrap()), "2024-02-29T23:59:59Z");
    }

    #[test]
    fn durations() {
        assert_eq!(parse_duration("90s"), Some(Duration::from_secs(90)));
        assert_eq!(parse_duration("1h30m"), Some(Duration::from_secs(5400)));
        assert_eq!(parse_duration("250ms"), Some(Duration::from_millis(250)));
        assert_eq!(parse_duration("5x"), None);
        assert_eq!(parse_duration(""), None);
        assert_eq!(human_duration(14 * 60_000), "14m");
        assert_eq!(human_duration(125 * 60_000), "2h 5m");
        assert_eq!(human_duration(3 * 86_400_000 + 4 * 3_600_000), "3d 4h");
        assert_eq!(human_duration(42_000), "42s");
    }
}
