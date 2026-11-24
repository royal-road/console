//! Draws the rule's series, its threshold and the firing interval as a PNG
//! sized for a Discord embed (dark theme), in-process with tiny-skia and an
//! embedded font.

use ab_glyph::{Font, FontRef, PxScale, ScaleFont, point};
use tiny_skia::{Color, Paint, PathBuilder, Pixmap, Rect, Stroke, StrokeDash, Transform};

use crate::expr::CmpOp;
use crate::fetch::Series;
use crate::labels;
use crate::payload::{Labels, Severity};
use crate::time::{self, Millis};

static FONT: &[u8] = include_bytes!("../assets/DejaVuSans-subset.ttf");

const W: u32 = 900;
const H: u32 = 380;
const LEFT: f32 = 92.0;
const RIGHT: f32 = 24.0;
const TOP: f32 = 64.0;
const BOTTOM: f32 = 44.0;
const TEXT_PX: f32 = 21.0;

/// Discord's dark embed background, so the image blends in.
const BG: Rgb = Rgb(0x2b, 0x2d, 0x31);
const GRID: Rgb = Rgb(0x3f, 0x41, 0x47);
const TEXT: Rgb = Rgb(0xb5, 0xba, 0xc1);
const TEXT_STRONG: Rgb = Rgb(0xdb, 0xde, 0xe1);
const OTHER: Rgb = Rgb(0x5c, 0x5f, 0x66);
/// Categorical order checked with the dataviz validator against BG (CVD ΔE >= 9.4);
/// red and yellow stay free for thresholds.
const PALETTE: [Rgb; 6] = [
    Rgb(0x39, 0x87, 0xe5),
    Rgb(0xd9, 0x59, 0x26),
    Rgb(0x19, 0x9e, 0x70),
    Rgb(0x90, 0x85, 0xe9),
    Rgb(0xd5, 0x51, 0x81),
    Rgb(0x0c, 0xa3, 0x0c),
];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Rgb(pub u8, pub u8, pub u8);

pub fn severity_colour(s: Severity) -> Rgb {
    match s {
        Severity::Critical => Rgb(0xd0, 0x3b, 0x3b),
        Severity::Warning => Rgb(0xfa, 0xb2, 0x19),
        Severity::Info => Rgb(0x94, 0x9b, 0xa4),
    }
}

pub struct Spec<'a> {
    pub start: Millis,
    pub end: Millis,
    pub severity: Severity,
    /// Each alert's firing interval; `None` = still firing.
    pub firing: Vec<(Millis, Option<Millis>)>,
    pub primary: Vec<Series>,
    pub primary_name: String,
    pub threshold_series: Vec<Series>,
    pub threshold_name: String,
    pub scalars: Vec<(CmpOp, f64)>,
    /// The notification's alerts; series with their labels are highlighted.
    pub alerts: Vec<&'a Labels>,
    pub max_series: usize,
}

#[derive(Debug)]
pub struct Line<'a> {
    pub series: &'a Series,
    pub colour: Rgb,
    pub dashed: bool,
    /// In the legend when set.
    pub label: Option<String>,
}

/// A series belongs to an alert when every one of its labels has the same
/// value on the alert (the alert adds rule labels on top).
fn belongs(series: &Labels, alert: &Labels) -> bool {
    // Rule labels such as `prometheus` override the series' own.
    series.iter().all(|(k, v)| labels::is_dropped(k) || alert.get(k) == Some(v))
}

fn belongs_loosely(series: &Labels, alert: &Labels) -> bool {
    series.iter().all(|(k, v)| labels::is_dropped(k) || labels::is_folded(k) || alert.get(k) == Some(v))
}

fn peak(s: &Series) -> f64 {
    s.points.iter().map(|p| p.1).fold(f64::MIN, f64::max)
}

/// The lines to draw, back to front, and how many coloured candidates did
/// not fit. Series of the notification's alerts get colours in alert order
/// (if none match, the highest do); other series are drawn faint, the
/// highest `max_series` of them. Threshold series are dashed in the colour
/// of the series they match.
pub fn lines<'a>(spec: &'a Spec) -> (Vec<Line<'a>>, usize) {
    let mut coloured: Vec<&Series> = Vec::new();
    // Exact first; if no series matches, without scrape labels (`instance`,
    // `job`...), which differ when the alert came from another scrape.
    for strict in [true, false] {
        for alert in &spec.alerts {
            for s in &spec.primary {
                let ok = if strict { belongs(&s.labels, alert) } else { belongs_loosely(&s.labels, alert) };
                if ok && !coloured.iter().any(|c| std::ptr::eq(*c, s)) {
                    coloured.push(s);
                }
            }
        }
        if !coloured.is_empty() {
            break;
        }
    }
    let mut by_peak: Vec<&Series> = spec.primary.iter().collect();
    by_peak.sort_by(|a, b| peak(b).total_cmp(&peak(a)));
    if coloured.is_empty() {
        coloured = by_peak.iter().take(spec.max_series.min(PALETTE.len())).copied().collect();
    }
    let hidden = coloured.len().saturating_sub(PALETTE.len());
    coloured.truncate(PALETTE.len());

    let mut out = Vec::new();
    for s in by_peak.iter().filter(|s| !coloured.iter().any(|c| std::ptr::eq(*c, **s))).take(spec.max_series)
    {
        out.push(Line { series: s, colour: OTHER, dashed: false, label: None });
    }
    let names = legend_names(&coloured, &spec.primary, &spec.primary_name);
    let mut dashed_drawn = false;
    for (i, s) in coloured.iter().enumerate() {
        let colour = PALETTE[i];
        for t in spec
            .threshold_series
            .iter()
            .filter(|t| belongs(&t.labels, &s.labels) || belongs(&s.labels, &t.labels))
        {
            out.push(Line { series: t, colour, dashed: true, label: None });
            dashed_drawn = true;
        }
        out.push(Line { series: s, colour, dashed: false, label: Some(names[i].clone()) });
    }
    if !dashed_drawn && spec.threshold_series.len() <= 3 {
        for t in &spec.threshold_series {
            out.push(Line { series: t, colour: TEXT, dashed: true, label: None });
        }
    }
    (out, hidden)
}

/// Legend text: the values of the labels that tell the query's series
/// apart (identifying labels first, names before ids), else the query's
/// metric.
fn legend_names(coloured: &[&Series], all: &[Series], fallback: &str) -> Vec<String> {
    let mut keys: Vec<&str> = all.iter().flat_map(|s| s.labels.keys().map(String::as_str)).collect();
    keys.sort();
    keys.dedup();
    let mut differing: Vec<&str> = keys
        .into_iter()
        .filter(|k| !labels::is_dropped(k))
        .filter(|k| all.iter().any(|s| s.labels.get(*k) != all[0].labels.get(*k)))
        .collect();
    differing.sort_by_key(|k| labels::rank(k));
    let names: Vec<String> = coloured
        .iter()
        .map(|s| {
            let v: Vec<&str> =
                differing.iter().filter_map(|k| s.labels.get(*k)).map(String::as_str).take(2).collect();
            if v.is_empty() { fallback.to_string() } else { v.join(" / ") }
        })
        .collect();
    // Sibling pods share a long prefix: `sentry-snuba-consumer-...` and
    // `sentry-snuba-metrics-consumer-...` become `…consumer-...`, `…metrics-...`.
    let prefix = common_prefix(&names);
    names
        .iter()
        .map(|n| if prefix >= 8 { format!("…{}", &n[prefix..]) } else { n.clone() })
        .map(|n| truncate(&n, 34))
        .collect()
}

/// The length of the common prefix of `names`, cut after a `-`, `_` or `/`.
fn common_prefix(names: &[String]) -> usize {
    let [first, rest @ ..] = names else { return 0 };
    if rest.is_empty() {
        return 0;
    }
    let mut len = first.len();
    for n in rest {
        len = first.bytes().zip(n.bytes()).take(len).take_while(|(a, b)| a == b).count();
    }
    first[..len].rfind(['-', '_', '/']).map_or(0, |i| i + 1)
}

/// Where each legend entry goes: `(x, row)`, in up to three rows, the first
/// shortened by the notes on its right. Entries that do not fit are left out.
fn legend_layout(widths: &[f32], notes_w: f32) -> Vec<(f32, usize)> {
    let (left, right) = (LEFT, W as f32 - RIGHT);
    let mut out = Vec::new();
    let (mut x, mut row) = (left, 0);
    for w in widths {
        let limit = if row == 0 { right - notes_w } else { right };
        if x + w > limit {
            row += 1;
            x = left;
        }
        if row > 2 || x + w > right {
            break;
        }
        out.push((x, row));
        x += w;
    }
    out
}

pub fn truncate(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let mut t: String = s.chars().take(max.saturating_sub(1)).collect();
    t.push('…');
    t
}

/// The metric a query is mostly about, for legends: the first identifier
/// that is neither a function call nor a keyword.
pub fn query_name(q: &str) -> String {
    const KEYWORDS: &[&str] = &[
        "by",
        "without",
        "on",
        "ignoring",
        "group_left",
        "group_right",
        "bool",
        "and",
        "or",
        "unless",
        "offset",
        "default",
        "if",
        "ifnot",
        "inf",
        "nan",
    ];
    let b = q.as_bytes();
    let mut i = 0;
    let mut depth_brace = 0;
    while i < b.len() {
        let c = b[i];
        match c {
            b'{' => depth_brace += 1,
            b'}' => depth_brace -= 1,
            b'"' | b'\'' | b'`' => {
                i += 1;
                while i < b.len() && b[i] != c {
                    i += if b[i] == b'\\' { 2 } else { 1 };
                }
            }
            b'[' => {
                while i < b.len() && b[i] != b']' {
                    i += 1;
                }
            }
            _ if depth_brace == 0 && (c.is_ascii_alphabetic() || c == b'_' || c == b':') => {
                let start = i;
                while i < b.len() && (b[i].is_ascii_alphanumeric() || b[i] == b'_' || b[i] == b':') {
                    i += 1;
                }
                let word = &q[start..i];
                let next = q[i..].trim_start();
                let prev_by = q[..start].trim_end().ends_with(['(', ',']) && is_label_list(q, start);
                let next_word: String =
                    next.chars().take_while(|c| c.is_ascii_alphanumeric() || *c == '_').collect();
                let aggregation =
                    next_word.eq_ignore_ascii_case("by") || next_word.eq_ignore_ascii_case("without");
                if !next.starts_with('(')
                    && !aggregation
                    && !KEYWORDS.contains(&word.to_ascii_lowercase().as_str())
                    && !prev_by
                {
                    return word.to_string();
                }
                continue;
            }
            _ => {}
        }
        i += 1;
    }
    truncate(q.trim(), 40)
}

/// `true` when `pos` is inside `by (...)`, `on (...)` and the like.
fn is_label_list(q: &str, pos: usize) -> bool {
    let before = &q[..pos];
    let Some(open) = before.rfind('(') else { return false };
    if before[open..].contains(')') {
        return false;
    }
    let head = before[..open].trim_end();
    ["by", "without", "on", "ignoring", "group_left", "group_right"].iter().any(|k| {
        head.len() >= k.len()
            && head[head.len() - k.len()..].eq_ignore_ascii_case(k)
            && !head[..head.len() - k.len()].ends_with(|c: char| c.is_ascii_alphanumeric() || c == '_')
    })
}

/// A short form for axes and legends: 0.02, 1.5k, 3.2M, 2.1G.
pub fn format_value(v: f64) -> String {
    let a = v.abs();
    let (scaled, unit) = if a >= 1e12 {
        (v / 1e12, "T")
    } else if a >= 1e9 {
        (v / 1e9, "G")
    } else if a >= 1e6 {
        (v / 1e6, "M")
    } else if a >= 1e4 {
        (v / 1e3, "k")
    } else {
        (v, "")
    };
    let digits = if scaled.abs() >= 100.0 || scaled == scaled.trunc() {
        0
    } else if scaled.abs() >= 10.0 {
        1
    } else if scaled.abs() >= 1.0 {
        2
    } else {
        // Three significant digits below 1.
        (2 - scaled.abs().log10().floor() as i32).clamp(0, 9) as usize
    };
    let s = format!("{scaled:.digits$}");
    let s = if s.contains('.') { s.trim_end_matches('0').trim_end_matches('.').to_string() } else { s };
    format!("{s}{unit}")
}

fn nice_step(range: f64, ticks: f64) -> f64 {
    let raw = range / ticks;
    let mag = 10f64.powf(raw.log10().floor());
    let f = raw / mag;
    let nice = if f <= 1.0 {
        1.0
    } else if f <= 2.0 {
        2.0
    } else if f <= 2.5 {
        2.5
    } else if f <= 5.0 {
        5.0
    } else {
        10.0
    };
    nice * mag
}

/// The y range: all drawn values and thresholds, padded, from 0 when the
/// data is non-negative and not far from it.
fn y_range(lines: &[Line], scalars: &[(CmpOp, f64)]) -> (f64, f64) {
    let values =
        lines.iter().flat_map(|l| l.series.points.iter().map(|p| p.1)).chain(scalars.iter().map(|s| s.1));
    let (mut lo, mut hi) =
        values.filter(|v| v.is_finite()).fold((f64::MAX, f64::MIN), |(lo, hi), v| (lo.min(v), hi.max(v)));
    if lo > hi {
        return (0.0, 1.0);
    }
    if lo >= 0.0 && lo <= (hi - lo) {
        lo = 0.0;
    }
    if hi == lo {
        let pad = if hi == 0.0 { 1.0 } else { hi.abs() * 0.1 };
        return (lo - if lo == 0.0 { 0.0 } else { pad }, hi + pad);
    }
    let pad = (hi - lo) * 0.08;
    hi += pad;
    if lo != 0.0 {
        lo -= pad;
    }
    (lo, hi)
}

struct Canvas {
    pixmap: Pixmap,
    font: FontRef<'static>,
}

impl Canvas {
    fn paint(colour: Rgb, alpha: u8) -> Paint<'static> {
        let mut p = Paint::default();
        p.set_color_rgba8(colour.0, colour.1, colour.2, alpha);
        p.anti_alias = true;
        p
    }

    fn rect(&mut self, x: f32, y: f32, w: f32, h: f32, colour: Rgb, alpha: u8) {
        if let Some(r) = Rect::from_xywh(x, y, w.max(0.5), h.max(0.5)) {
            self.pixmap.fill_rect(r, &Self::paint(colour, alpha), Transform::identity(), None);
        }
    }

    fn polyline(&mut self, pts: &[(f32, f32)], colour: Rgb, alpha: u8, width: f32, dashed: bool) {
        let mut pb = PathBuilder::new();
        for (i, (x, y)) in pts.iter().enumerate() {
            if i == 0 { pb.move_to(*x, *y) } else { pb.line_to(*x, *y) }
        }
        if pts.len() == 1 {
            pb.line_to(pts[0].0 + 0.1, pts[0].1);
        }
        let Some(path) = pb.finish() else { return };
        let mut stroke = Stroke { width, line_join: tiny_skia::LineJoin::Round, ..Stroke::default() };
        stroke.line_cap = if pts.len() == 1 { tiny_skia::LineCap::Round } else { tiny_skia::LineCap::Butt };
        if dashed {
            stroke.dash = StrokeDash::new(vec![10.0, 7.0], 0.0);
        }
        self.pixmap.stroke_path(&path, &Self::paint(colour, alpha), &stroke, Transform::identity(), None);
    }

    fn text_width(&self, text: &str, px: f32) -> f32 {
        let f = self.font.as_scaled(PxScale::from(px));
        let mut w = 0.0;
        let mut prev = None;
        for c in text.chars() {
            let id = f.glyph_id(c);
            if let Some(p) = prev {
                w += f.kern(p, id);
            }
            w += f.h_advance(id);
            prev = Some(id);
        }
        w
    }

    /// Draws `text` with its baseline at `y`; `align` 0 = left, 0.5 = centre, 1 = right.
    fn text(&mut self, text: &str, x: f32, y: f32, px: f32, colour: Rgb, align: f32) {
        let x0 = x - self.text_width(text, px) * align;
        let scale = PxScale::from(px);
        let f = self.font.as_scaled(scale);
        let (w, h) = (self.pixmap.width() as i32, self.pixmap.height() as i32);
        let mut caret = x0;
        let mut prev = None;
        for c in text.chars() {
            let id = f.glyph_id(c);
            if let Some(p) = prev {
                caret += f.kern(p, id);
            }
            let glyph = id.with_scale_and_position(scale, point(caret, y));
            caret += f.h_advance(id);
            prev = Some(id);
            let Some(outline) = self.font.outline_glyph(glyph) else { continue };
            let bounds = outline.px_bounds();
            let pixels = self.pixmap.pixels_mut();
            outline.draw(|gx, gy, cov| {
                let (px_, py) = (bounds.min.x as i32 + gx as i32, bounds.min.y as i32 + gy as i32);
                if px_ < 0 || py < 0 || px_ >= w || py >= h {
                    return;
                }
                let p = &mut pixels[(py * w + px_) as usize];
                let a = cov.clamp(0.0, 1.0);
                let mix = |d: u8, s: u8| (s as f32 * a + d as f32 * (1.0 - a)).round() as u8;
                // The background is opaque, so premultiplied = straight here.
                *p = tiny_skia::PremultipliedColorU8::from_rgba(
                    mix(p.red(), colour.0),
                    mix(p.green(), colour.1),
                    mix(p.blue(), colour.2),
                    255,
                )
                .unwrap_or(*p);
            });
        }
    }
}

const TICK_STEPS_S: &[i64] =
    &[60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172_800, 604_800];

/// Renders the chart. Errors only if the canvas or font cannot be created.
pub fn render(spec: &Spec) -> Result<Vec<u8>, String> {
    let font = FontRef::try_from_slice(FONT).map_err(|e| e.to_string())?;
    let pixmap = Pixmap::new(W, H).ok_or("canvas")?;
    let mut c = Canvas { pixmap, font };
    c.pixmap.fill(Color::from_rgba8(BG.0, BG.1, BG.2, 255));

    let (lines, hidden) = lines(spec);
    let mut notes = vec!["UTC".to_string()];
    if lines.iter().any(|l| l.dashed) {
        notes.push(format!("dashed: {}", truncate(&spec.threshold_name, 30)));
    }
    let faint = lines.iter().filter(|l| l.colour == OTHER).count() + hidden;
    let omitted = spec.primary.len().saturating_sub(lines.iter().filter(|l| !l.dashed).count());
    if faint + omitted > 0 {
        notes.push(format!("{} other series", faint + omitted));
    }
    let notes = notes.join(" · ");
    let legend: Vec<(&Line, f32)> = lines
        .iter()
        .filter(|l| l.label.is_some())
        .map(|l| (l, 48.0 + c.text_width(l.label.as_deref().unwrap_or_default(), TEXT_PX)))
        .collect();
    let notes_w = c.text_width(&notes, TEXT_PX - 3.0) + 24.0;
    let placed = legend_layout(&legend.iter().map(|(_, w)| *w).collect::<Vec<_>>(), notes_w);
    let rows = placed.iter().map(|(_, r)| r + 1).max().unwrap_or(1);
    let top = TOP + 30.0 * (rows - 1) as f32;
    let (plot_l, plot_r, plot_t, plot_b) = (LEFT, W as f32 - RIGHT, top, H as f32 - BOTTOM);
    let (t0, t1) = (spec.start as f64, spec.end.max(spec.start + 1) as f64);
    let (y0, y1) = y_range(&lines, &spec.scalars);
    let sx = |t: f64| plot_l + ((t - t0) / (t1 - t0)) as f32 * (plot_r - plot_l);
    let sy = |v: f64| plot_b - ((v - y0) / (y1 - y0)) as f32 * (plot_b - plot_t);
    let sev = severity_colour(spec.severity);

    // Firing intervals.
    for (start, end) in &spec.firing {
        let a = sx((*start as f64).max(t0));
        let b = sx(end.map_or(t1, |e| (e as f64).min(t1)));
        if b > a {
            c.rect(a, plot_t, b - a, plot_b - plot_t, sev, 34);
            c.rect(a - 1.0, plot_t, 2.0, plot_b - plot_t, sev, 150);
        }
    }

    // Horizontal grid and y labels.
    let step = nice_step(y1 - y0, 4.0);
    let mut v = (y0 / step).ceil() * step;
    while v <= y1 + step * 1e-9 {
        let y = sy(v).round() + 0.5;
        c.rect(plot_l, y - 0.5, plot_r - plot_l, 1.0, GRID, 255);
        c.text(
            &format_value(if v.abs() < step * 1e-9 { 0.0 } else { v }),
            plot_l - 12.0,
            y + 6.5,
            TEXT_PX,
            TEXT,
            1.0,
        );
        v += step;
    }

    // Time axis.
    let span_s = ((t1 - t0) / 1000.0) as i64;
    let tick_s = TICK_STEPS_S.iter().copied().find(|s| span_s / s <= 6).unwrap_or(604_800);
    let mut t = (spec.start / 1000 / tick_s + 1) * tick_s * 1000;
    let multi_day = span_s > 86_400;
    while (t as f64) < t1 {
        let x = sx(t as f64);
        c.rect(x, plot_b, 1.0, 6.0, GRID, 255);
        let label = if multi_day && t % 86_400_000 == 0 { time::mmdd(t) } else { time::hhmm(t) };
        let half = c.text_width(&label, TEXT_PX) / 2.0;
        if x + half < W as f32 - 2.0 {
            c.text(&label, x, plot_b + 28.0, TEXT_PX, TEXT, 0.5);
        }
        t += tick_s * 1000;
    }
    c.rect(plot_l, plot_b, plot_r - plot_l, 1.0, OTHER, 255);

    // Series: faint ones first, coloured on top.
    for l in &lines {
        let pts = |s: &Series| -> Vec<Vec<(f32, f32)>> {
            // Break the line where samples are missing for more than 3 steps.
            let gap = (crate::fetch::step_secs(spec.start, spec.end) * 1000 * 3) as Millis;
            let mut runs: Vec<Vec<(f32, f32)>> = vec![];
            let mut last: Option<Millis> = None;
            for (t, v) in &s.points {
                if last.is_none_or(|l| t - l > gap) {
                    runs.push(vec![]);
                }
                runs.last_mut().unwrap().push((sx(*t as f64), sy(*v)));
                last = Some(*t);
            }
            runs
        };
        let (alpha, width) = if l.colour == OTHER { (200, 2.0) } else { (255, 3.0) };
        for run in pts(l.series) {
            c.polyline(&run, l.colour, alpha, width, l.dashed);
        }
    }

    // Scalar thresholds, labelled at the right end.
    for (op, value) in &spec.scalars {
        if !value.is_finite() {
            continue;
        }
        let y = sy(*value);
        c.polyline(&[(plot_l, y), (plot_r, y)], sev, 255, 2.5, false);
        let label = format!("{} {}", op.symbol(), format_value(*value));
        let tw = c.text_width(&label, TEXT_PX);
        let ly = if y - plot_t > 30.0 { y - 8.0 } else { y + 24.0 };
        c.rect(plot_r - tw - 14.0, ly - 19.0, tw + 12.0, 25.0, BG, 220);
        c.text(&label, plot_r - 8.0, ly, TEXT_PX, sev, 1.0);
    }

    // Legend across the top, notes on the right of the first row.
    for ((l, _), (x, row)) in legend.iter().zip(&placed) {
        let ly = 34.0 + 30.0 * *row as f32;
        c.polyline(&[(*x, ly - 7.0), (x + 18.0, ly - 7.0)], l.colour, 255, 4.0, false);
        c.text(l.label.as_deref().unwrap_or_default(), x + 26.0, ly, TEXT_PX, TEXT_STRONG, 0.0);
    }
    c.text(&notes, plot_r, 34.0, TEXT_PX - 3.0, TEXT, 1.0);
    if spec.primary.is_empty() {
        c.text(
            "no data in this window",
            (plot_l + plot_r) / 2.0,
            (plot_t + plot_b) / 2.0,
            TEXT_PX + 2.0,
            TEXT,
            0.5,
        );
    }

    c.pixmap.encode_png().map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn series(pairs: &[(&str, &str)], values: &[f64]) -> Series {
        Series {
            labels: pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect(),
            points: values.iter().enumerate().map(|(i, v)| (i as Millis * 60_000, *v)).collect(),
        }
    }

    fn spec<'a>(primary: Vec<Series>, threshold_series: Vec<Series>, alerts: Vec<&'a Labels>) -> Spec<'a> {
        Spec {
            start: 0,
            end: 3_600_000,
            severity: Severity::Critical,
            firing: vec![(1_800_000, None)],
            primary,
            primary_name: "running".into(),
            threshold_series,
            threshold_name: "desired".into(),
            scalars: vec![],
            alerts,
            max_series: 8,
        }
    }

    #[test]
    fn the_alerts_series_are_coloured_the_rest_faint() {
        let alert: Labels = [("alertname", "X"), ("model", "b"), ("severity", "critical")]
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        let s = spec(
            vec![
                series(&[("model", "a")], &[5.0]),
                series(&[("model", "b")], &[0.0]),
                series(&[("model", "c")], &[9.0]),
            ],
            vec![series(&[("model", "b")], &[1.0]), series(&[("model", "a")], &[1.0])],
            vec![&alert],
        );
        let (lines, hidden) = lines(&s);
        assert_eq!(hidden, 0);
        let coloured: Vec<_> = lines.iter().filter(|l| l.colour != OTHER).collect();
        assert_eq!(coloured.len(), 2);
        assert!(coloured[0].dashed && coloured[0].series.labels["model"] == "b");
        assert_eq!(coloured[1].label.as_deref(), Some("b"));
        assert_eq!(coloured[1].colour, PALETTE[0]);
        let faint: Vec<_> =
            lines.iter().filter(|l| l.colour == OTHER).map(|l| &l.series.labels["model"]).collect();
        assert_eq!(faint, ["c", "a"]);
    }

    #[test]
    fn without_a_match_the_highest_series_are_coloured_and_capped() {
        let many: Vec<Series> = (0..20).map(|i| series(&[("pod", &format!("p{i}"))], &[i as f64])).collect();
        let mut s = spec(many, vec![], vec![]);
        s.max_series = 4;
        let (lines, _) = lines(&s);
        let coloured: Vec<_> =
            lines.iter().filter(|l| l.colour != OTHER).map(|l| l.label.clone().unwrap()).collect();
        assert_eq!(coloured, ["p19", "p18", "p17", "p16"]);
        assert_eq!(lines.iter().filter(|l| l.colour == OTHER).count(), 4);
    }

    #[test]
    fn a_label_less_series_matches_every_alert() {
        let alert: Labels = [("alertname".to_string(), "X".to_string())].into();
        let s = spec(vec![series(&[], &[1.0])], vec![], vec![&alert]);
        let (lines, _) = lines(&s);
        assert_eq!(lines[0].label.as_deref(), Some("running"));
    }

    #[test]
    fn sibling_names_lose_their_common_prefix() {
        let names = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert_eq!(
            common_prefix(&names(&["sentry-snuba-consumer-6f", "sentry-snuba-metrics-consumer-6f"])),
            13
        );
        assert_eq!(common_prefix(&names(&["abc", "abd"])), 0);
        assert_eq!(common_prefix(&names(&["only-one"])), 0);
    }

    #[test]
    fn legend_wraps_into_three_rows_at_most() {
        let placed = legend_layout(&[300.0; 8], 200.0);
        let rows: Vec<usize> = placed.iter().map(|(_, r)| *r).collect();
        assert_eq!(rows, [0, 1, 1, 2, 2], "the notes leave room for one entry on the first row");
        let placed = legend_layout(&[700.0], 200.0);
        assert_eq!(placed, [(LEFT, 1)], "an entry that would run into the notes starts the second row");
    }

    #[test]
    fn values() {
        assert_eq!(format_value(0.0), "0");
        assert_eq!(format_value(0.02), "0.02");
        assert_eq!(format_value(0.0234567), "0.0235");
        assert_eq!(format_value(1.5), "1.5");
        assert_eq!(format_value(12.26), "12.3");
        assert_eq!(format_value(250.0), "250");
        assert_eq!(format_value(15_300.0), "15.3k");
        assert_eq!(format_value(2_147_483_648.0), "2.15G");
        assert_eq!(format_value(-3.0), "-3");
    }

    #[test]
    fn query_names() {
        assert_eq!(
            query_name("max by (cluster_name, model_id) (gpustack:model_desired_instances{job=\"g\"})"),
            "gpustack:model_desired_instances"
        );
        assert_eq!(
            query_name("sum(rate(nginx_ingress_controller_requests{status=~\"5..\"}[5m]))"),
            "nginx_ingress_controller_requests"
        );
        assert_eq!(query_name("vector(1)"), "vector(1)");
    }

    #[test]
    fn renders_a_png() {
        let alert: Labels = [("model".to_string(), "b".to_string())].into();
        let mut s = spec(
            vec![
                series(&[("model", "b")], &[1.0, 1.0, 0.0, 0.0]),
                series(&[("model", "a")], &[2.0, 2.0, 2.0, 2.0]),
            ],
            vec![series(&[("model", "b")], &[1.0, 1.0, 1.0, 1.0])],
            vec![&alert],
        );
        s.scalars = vec![(CmpOp::Gt, 1.5)];
        let png = render(&s).unwrap();
        assert_eq!(&png[1..4], b"PNG");
        let decoded = Pixmap::decode_png(&png).unwrap();
        assert_eq!((decoded.width(), decoded.height()), (W, H));
        // Something was drawn in the series colour.
        let p = PALETTE[0];
        assert!(decoded.pixels().iter().any(|px| (px.red(), px.green(), px.blue()) == (p.0, p.1, p.2)));
        render(&spec(vec![], vec![], vec![])).unwrap();
    }
}
