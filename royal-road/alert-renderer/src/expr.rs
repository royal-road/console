//! Turns an alerting rule's expression into what to chart.
//!
//! This is not a PromQL parser. It lexes MetricsQL just far enough to find
//! the binary operators outside any brackets, splits at the loosest one, and
//! keeps the operands as the original text so they can be queried unchanged.
//! `x > 5` charts `x` with a line at 5; `x < y` charts `x` with `y` dashed;
//! `a > 5 and on(i) b` charts the `a > 5` side. Anything else is charted as
//! written, without a threshold.

/// A comparison, oriented so that the charted series is on its left.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CmpOp {
    Gt,
    Ge,
    Lt,
    Le,
    Eq,
    Ne,
}

impl CmpOp {
    pub fn symbol(self) -> &'static str {
        match self {
            CmpOp::Gt => ">",
            CmpOp::Ge => ">=",
            CmpOp::Lt => "<",
            CmpOp::Le => "<=",
            CmpOp::Eq => "==",
            CmpOp::Ne => "!=",
        }
    }

    /// `5 < x` is `x > 5`.
    fn flipped(self) -> CmpOp {
        match self {
            CmpOp::Gt => CmpOp::Lt,
            CmpOp::Ge => CmpOp::Le,
            CmpOp::Lt => CmpOp::Gt,
            CmpOp::Le => CmpOp::Ge,
            other => other,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum Threshold {
    /// A horizontal line.
    Scalar { op: CmpOp, value: f64 },
    /// Another query, drawn dashed.
    Series { op: CmpOp, query: String },
}

#[derive(Clone, Debug, PartialEq)]
pub struct ChartPlan {
    /// Queries whose series are the alert's values (more than one only for
    /// `a < 3 or b < 3`).
    pub queries: Vec<String>,
    pub thresholds: Vec<Threshold>,
}

/// What to chart for `expr`.
pub fn plan(expr: &str) -> ChartPlan {
    let cleaned = strip_comments(expr);
    analyse(&cleaned)
}

fn analyse(e: &str) -> ChartPlan {
    let e = strip_parens(e);
    let whole = || ChartPlan { queries: vec![e.to_string()], thresholds: vec![] };
    let ops = top_level_ops(e);
    let Some(lowest) = ops.iter().map(|o| o.kind.priority()).min() else {
        return unwrap_min_max(e).unwrap_or_else(whole);
    };
    let at: Vec<&OpSpan> = ops.iter().filter(|o| o.kind.priority() == lowest).collect();
    // Every level below `^` is left-associative: the last operator is the root.
    let root = at[at.len() - 1];
    let (lhs, rhs) = (&e[..root.start], &e[root.end..]);
    match root.kind {
        // The value of `a default b`, `a if b`, `a and b` and `a unless b`
        // comes from `a`.
        OpKind::Default | OpKind::If | OpKind::And => analyse(lhs),
        OpKind::Or => {
            let mut branches = Vec::new();
            let mut from = 0;
            for op in &at {
                branches.push(&e[from..op.start]);
                from = op.end;
            }
            branches.push(&e[from..]);
            // `a > 1 or b < 2`: every branch's series with every line, as
            // long as each branch is a comparison.
            let plans: Vec<ChartPlan> = branches.iter().map(|b| analyse(b)).collect();
            if plans.iter().any(|p| p.thresholds.is_empty()) {
                return whole();
            }
            let mut out = ChartPlan { queries: vec![], thresholds: vec![] };
            for p in plans {
                for q in p.queries {
                    if !out.queries.contains(&q) {
                        out.queries.push(q);
                    }
                }
                for t in p.thresholds {
                    if !out.thresholds.contains(&t) {
                        out.thresholds.push(t);
                    }
                }
            }
            out
        }
        OpKind::Cmp(op) => {
            if let Some(value) = constant(rhs) {
                with_threshold(lhs, Threshold::Scalar { op, value })
            } else if let Some(value) = constant(lhs) {
                with_threshold(rhs, Threshold::Scalar { op: op.flipped(), value })
            } else {
                let query = strip_parens(rhs).to_string();
                with_threshold(lhs, Threshold::Series { op, query })
            }
        }
        OpKind::Arith => whole(),
    }
}

/// `max by (x) (a == 1)` charts `max by (x) (a)` with the line at 1: `min`
/// and `max` keep the values they select, so the threshold still applies.
fn unwrap_min_max(e: &str) -> Option<ChartPlan> {
    let toks = lex(e);
    let is_word = |i: usize, words: &[&str]| matches!(toks.get(i).map(|t| &t.kind), Some(Tok::Word(w)) if words.contains(&w.as_str()));
    let close_of = |open: usize| {
        let mut depth = 0;
        for (i, t) in toks.iter().enumerate().skip(open) {
            match t.kind {
                Tok::Open => depth += 1,
                Tok::Close => {
                    depth -= 1;
                    if depth == 0 {
                        return Some(i);
                    }
                }
                _ => {}
            }
        }
        None
    };
    if !is_word(0, &["max", "min"]) {
        return None;
    }
    let mut open = 1;
    if is_word(1, &["by", "without"]) {
        open = close_of(2)? + 1;
    }
    if toks.get(open)?.kind != Tok::Open || &e[toks[open].start..toks[open].end] != "(" {
        return None;
    }
    let close = close_of(open)?;
    let after = close + 1;
    let tail_ok = after == toks.len()
        || (is_word(after, &["by", "without"]) && close_of(after + 1) == Some(toks.len() - 1));
    if !tail_ok {
        return None;
    }
    let (prefix, inner, suffix) =
        (&e[..toks[open].end], &e[toks[open].end..toks[close].start], &e[toks[close].start..]);
    let p = analyse(inner);
    if p.thresholds.is_empty() {
        return None;
    }
    let wrap = |q: &str| format!("{prefix}{q}{suffix}");
    Some(ChartPlan {
        queries: p.queries.iter().map(|q| wrap(q)).collect(),
        thresholds: p
            .thresholds
            .into_iter()
            .map(|t| match t {
                Threshold::Series { op, query } => Threshold::Series { op, query: wrap(&query) },
                scalar => scalar,
            })
            .collect(),
    })
}

/// `side` charted with `threshold`; a chained `x > 1 < 5` keeps both lines.
fn with_threshold(side: &str, threshold: Threshold) -> ChartPlan {
    let side = strip_parens(side);
    let chained = top_level_ops(side)
        .iter()
        .map(|o| o.kind.priority())
        .min()
        .is_some_and(|p| p == OpKind::Cmp(CmpOp::Eq).priority());
    let mut plan = if chained {
        analyse(side)
    } else {
        ChartPlan { queries: vec![side.to_string()], thresholds: vec![] }
    };
    plan.thresholds.push(threshold);
    plan
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum OpKind {
    Default,
    If,
    Or,
    And,
    Cmp(CmpOp),
    Arith,
}

impl OpKind {
    /// MetricsQL's binary operator priorities (lower binds looser).
    fn priority(self) -> u8 {
        match self {
            OpKind::Default => 0,
            OpKind::If => 1,
            OpKind::Or => 2,
            OpKind::And => 3,
            OpKind::Cmp(_) => 4,
            OpKind::Arith => 5,
        }
    }
}

/// A binary operator outside brackets: `start..end` covers the operator and
/// its modifiers (`bool`, `on(...)`, `group_left(...)`), so the operands are
/// `[..start]` and `[end..]`.
#[derive(Debug)]
struct OpSpan {
    kind: OpKind,
    start: usize,
    end: usize,
}

fn top_level_ops(e: &str) -> Vec<OpSpan> {
    let toks = lex(e);
    let mut ops = Vec::new();
    let mut depth = 0usize;
    // At the start, and after an operator or `(`, a `-` is unary.
    let mut want_operand = true;
    let mut i = 0;
    while i < toks.len() {
        let t = &toks[i];
        i += 1;
        match t.kind {
            Tok::Open => {
                depth += 1;
                want_operand = true;
                continue;
            }
            Tok::Close => {
                depth = depth.saturating_sub(1);
                want_operand = false;
                continue;
            }
            _ if depth > 0 => continue,
            Tok::Operand => {
                want_operand = false;
                continue;
            }
            Tok::Punct => continue,
            Tok::Sym(_) | Tok::Word(_) => {}
        }
        let kind = match &t.kind {
            Tok::Sym(s) => match *s {
                "==" => OpKind::Cmp(CmpOp::Eq),
                "!=" => OpKind::Cmp(CmpOp::Ne),
                ">=" => OpKind::Cmp(CmpOp::Ge),
                "<=" => OpKind::Cmp(CmpOp::Le),
                ">" => OpKind::Cmp(CmpOp::Gt),
                "<" => OpKind::Cmp(CmpOp::Lt),
                "+" | "-" if want_operand => continue,
                _ => OpKind::Arith,
            },
            Tok::Word(w) if !want_operand => match w.as_str() {
                "default" => OpKind::Default,
                "if" | "ifnot" => OpKind::If,
                "or" => OpKind::Or,
                "and" | "unless" => OpKind::And,
                "atan2" => OpKind::Arith,
                // `by`, `without`, `offset`, `keep_metric_names`...
                _ => continue,
            },
            Tok::Word(_) => {
                want_operand = false;
                continue;
            }
            _ => unreachable!(),
        };
        let mut end = t.end;
        // Modifiers: bool, on|ignoring (...), group_left|group_right [(...)].
        let skip_group = |i: &mut usize, end: &mut usize| {
            if toks.get(*i).is_some_and(|t| t.kind == Tok::Open) {
                let mut d = 0;
                while let Some(t) = toks.get(*i) {
                    *i += 1;
                    match t.kind {
                        Tok::Open => d += 1,
                        Tok::Close => {
                            d -= 1;
                            if d == 0 {
                                *end = t.end;
                                break;
                            }
                        }
                        _ => {}
                    }
                }
            }
        };
        let word_at = |i: usize, words: &[&str]| matches!(toks.get(i).map(|t| &t.kind), Some(Tok::Word(w)) if words.contains(&w.as_str()));
        if word_at(i, &["bool"]) {
            end = toks[i].end;
            i += 1;
        }
        if word_at(i, &["on", "ignoring"]) {
            end = toks[i].end;
            i += 1;
            skip_group(&mut i, &mut end);
        }
        if word_at(i, &["group_left", "group_right"]) {
            end = toks[i].end;
            i += 1;
            skip_group(&mut i, &mut end);
        }
        ops.push(OpSpan { kind, start: t.start, end });
        want_operand = true;
    }
    ops
}

#[derive(Debug, PartialEq)]
enum Tok {
    Open,
    Close,
    /// A number, string or duration.
    Operand,
    /// An identifier or keyword, lowercased.
    Word(String),
    /// An operator symbol.
    Sym(&'static str),
    /// `,`, `:`, `@` and anything else that is neither.
    Punct,
}

struct Token {
    kind: Tok,
    start: usize,
    end: usize,
}

fn lex(e: &str) -> Vec<Token> {
    let b = e.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        let start = i;
        let kind = match c {
            _ if c.is_ascii_whitespace() => {
                i += 1;
                continue;
            }
            b'(' | b'[' | b'{' => {
                i += 1;
                Tok::Open
            }
            b')' | b']' | b'}' => {
                i += 1;
                Tok::Close
            }
            b'"' | b'\'' | b'`' => {
                i += 1;
                while i < b.len() && b[i] != c {
                    if b[i] == b'\\' && c != b'`' {
                        i += 1;
                    }
                    i += 1;
                }
                i = (i + 1).min(b.len());
                Tok::Operand
            }
            b'0'..=b'9' | b'.' => {
                // Numbers, 1e-3, 0x1f, and durations or suffixes such as 5m, 1Ki.
                while i < b.len()
                    && (b[i].is_ascii_alphanumeric()
                        || b[i] == b'.'
                        || ((b[i] == b'-' || b[i] == b'+') && matches!(b[i - 1], b'e' | b'E')))
                {
                    i += 1;
                }
                Tok::Operand
            }
            _ if c.is_ascii_alphabetic() || c == b'_' || c == b':' => {
                while i < b.len() && (b[i].is_ascii_alphanumeric() || matches!(b[i], b'_' | b':' | b'.')) {
                    i += 1;
                }
                let w = e[start..i].to_ascii_lowercase();
                if w == "inf" || w == "nan" { Tok::Operand } else { Tok::Word(w) }
            }
            _ => {
                const SYMS: [&str; 13] =
                    ["==", "!=", ">=", "<=", "=~", "!~", ">", "<", "+", "-", "*", "/", "%"];
                if let Some(s) = SYMS.iter().find(|s| e[i..].starts_with(**s)) {
                    i += s.len();
                    if *s == "=~" || *s == "!~" { Tok::Punct } else { Tok::Sym(s) }
                } else if c == b'^' {
                    i += 1;
                    Tok::Sym("^")
                } else {
                    i += e[i..].chars().next().map_or(1, char::len_utf8);
                    Tok::Punct
                }
            }
        };
        out.push(Token { kind, start, end: i });
    }
    out
}

/// Removes `# comments`, which run to the end of the line outside strings.
fn strip_comments(e: &str) -> String {
    let mut out = String::with_capacity(e.len());
    let mut quote: Option<char> = None;
    let mut chars = e.chars();
    while let Some(c) = chars.next() {
        match quote {
            Some(q) => {
                out.push(c);
                if c == '\\' && q != '`' {
                    if let Some(n) = chars.next() {
                        out.push(n);
                    }
                } else if c == q {
                    quote = None;
                }
            }
            None if c == '#' => {
                for n in chars.by_ref() {
                    if n == '\n' {
                        out.push('\n');
                        break;
                    }
                }
            }
            None => {
                if matches!(c, '"' | '\'' | '`') {
                    quote = Some(c);
                }
                out.push(c);
            }
        }
    }
    out
}

/// `((x))` is `x`; `(a) / (b)` stays.
fn strip_parens(e: &str) -> &str {
    let mut e = e.trim();
    loop {
        if !(e.starts_with('(') && e.ends_with(')')) {
            return e;
        }
        let toks = lex(e);
        let mut depth = 0;
        for (n, t) in toks.iter().enumerate() {
            match t.kind {
                Tok::Open => depth += 1,
                Tok::Close => {
                    depth -= 1;
                    if depth == 0 && n != toks.len() - 1 {
                        return e;
                    }
                }
                _ => {}
            }
        }
        e = e[1..e.len() - 1].trim();
    }
}

/// The value of a constant operand: `5`, `-1e3`, `0.05 * 100`, `(2)`,
/// `vector(0)`, `5m` (seconds), `Inf`.
pub fn constant(e: &str) -> Option<f64> {
    let toks = lex(e);
    let mut p = ConstParser { e, toks: &toks, i: 0 };
    let v = p.sum()?;
    (p.i == toks.len()).then_some(v)
}

struct ConstParser<'a> {
    e: &'a str,
    toks: &'a [Token],
    i: usize,
}

impl ConstParser<'_> {
    fn peek_sym(&self) -> Option<&'static str> {
        match self.toks.get(self.i)?.kind {
            Tok::Sym(s) => Some(s),
            _ => None,
        }
    }

    fn sum(&mut self) -> Option<f64> {
        let mut v = self.product()?;
        while let Some(s @ ("+" | "-")) = self.peek_sym() {
            self.i += 1;
            let r = self.product()?;
            v = if s == "+" { v + r } else { v - r };
        }
        Some(v)
    }

    fn product(&mut self) -> Option<f64> {
        let mut v = self.power()?;
        while let Some(s @ ("*" | "/" | "%")) = self.peek_sym() {
            self.i += 1;
            let r = self.power()?;
            v = match s {
                "*" => v * r,
                "/" => v / r,
                _ => v % r,
            };
        }
        Some(v)
    }

    fn power(&mut self) -> Option<f64> {
        let base = self.unary()?;
        if self.peek_sym() == Some("^") {
            self.i += 1;
            return Some(base.powf(self.power()?));
        }
        Some(base)
    }

    fn unary(&mut self) -> Option<f64> {
        match self.peek_sym() {
            Some("-") => {
                self.i += 1;
                Some(-self.unary()?)
            }
            Some("+") => {
                self.i += 1;
                self.unary()
            }
            _ => self.atom(),
        }
    }

    fn atom(&mut self) -> Option<f64> {
        let t = self.toks.get(self.i)?;
        self.i += 1;
        match &t.kind {
            Tok::Operand => number(&self.e[t.start..t.end]),
            Tok::Open if &self.e[t.start..t.end] == "(" => {
                let v = self.sum()?;
                let close = self.toks.get(self.i)?;
                self.i += 1;
                (close.kind == Tok::Close).then_some(v)
            }
            Tok::Word(w) if w == "vector" || w == "scalar" => {
                let open = self.toks.get(self.i)?;
                if open.kind != Tok::Open {
                    return None;
                }
                self.atom()
            }
            _ => None,
        }
    }
}

fn number(s: &str) -> Option<f64> {
    let lower = s.to_ascii_lowercase();
    match lower.as_str() {
        "inf" => return Some(f64::INFINITY),
        "nan" => return Some(f64::NAN),
        _ => {}
    }
    if let Some(hex) = lower.strip_prefix("0x") {
        return i64::from_str_radix(hex, 16).ok().map(|v| v as f64);
    }
    if let Ok(v) = lower.parse::<f64>() {
        return Some(v);
    }
    // MetricsQL: durations (seconds) and SI / IEC suffixes.
    if let Some(d) = crate::time::parse_duration(s) {
        return Some(d.as_secs_f64());
    }
    const SUFFIXES: [(&str, f64); 8] = [
        ("ki", 1024.0),
        ("mi", 1048576.0),
        ("gi", 1073741824.0),
        ("ti", 1099511627776.0),
        ("k", 1e3),
        ("m", 1e6),
        ("g", 1e9),
        ("t", 1e12),
    ];
    let trimmed = lower.trim_end_matches('b');
    SUFFIXES
        .iter()
        .find_map(|(suffix, mult)| trimmed.strip_suffix(suffix)?.parse::<f64>().ok().map(|v| v * mult))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scalar(op: CmpOp, value: f64) -> Threshold {
        Threshold::Scalar { op, value }
    }

    #[test]
    fn scalar_on_the_right() {
        let p = plan("up{job=\"a\"} == 0");
        assert_eq!(p.queries, ["up{job=\"a\"}"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Eq, 0.0)]);
    }

    #[test]
    fn every_comparison_operator() {
        for (sym, op) in [
            (">", CmpOp::Gt),
            (">=", CmpOp::Ge),
            ("<", CmpOp::Lt),
            ("<=", CmpOp::Le),
            ("==", CmpOp::Eq),
            ("!=", CmpOp::Ne),
        ] {
            let p = plan(&format!("some_metric {sym} 5"));
            assert_eq!(p.queries, ["some_metric"], "{sym}");
            assert_eq!(p.thresholds, [scalar(op, 5.0)], "{sym}");
        }
    }

    #[test]
    fn scalar_on_the_left_flips_the_operator() {
        let p = plan("0.9 < rate(x[5m])");
        assert_eq!(p.queries, ["rate(x[5m])"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 0.9)]);
        let p = plan("5 >= x");
        assert_eq!(p.thresholds, [scalar(CmpOp::Le, 5.0)]);
    }

    #[test]
    fn constant_expressions() {
        assert_eq!(plan("x > 0.05 * 100").thresholds, [scalar(CmpOp::Gt, 5.0)]);
        assert_eq!(plan("x > -1").thresholds, [scalar(CmpOp::Gt, -1.0)]);
        assert_eq!(plan("x > (2 + 1)").thresholds, [scalar(CmpOp::Gt, 3.0)]);
        assert_eq!(plan("x > 1e3").thresholds, [scalar(CmpOp::Gt, 1000.0)]);
        assert_eq!(plan("x > 5m").thresholds, [scalar(CmpOp::Gt, 300.0)]);
        assert_eq!(plan("x < 2Gi").thresholds, [scalar(CmpOp::Lt, 2147483648.0)]);
        assert_eq!(plan("x > bool 3").thresholds, [scalar(CmpOp::Gt, 3.0)]);
    }

    #[test]
    fn subtraction_binds_tighter_than_the_comparison() {
        let p = plan("a - b > 0");
        assert_eq!(p.queries, ["a - b"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 0.0)]);
    }

    #[test]
    fn vector_on_the_right_with_matching_modifiers_is_a_line() {
        let p = plan("x > on() group_left vector(7)");
        assert_eq!(p.queries, ["x"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 7.0)]);
    }

    #[test]
    fn two_vectors_draw_the_right_side_dashed() {
        let p = plan("max by (m) (running{job=\"g\"})\n  < max by (m) (desired{job=\"g\"})\n");
        assert_eq!(p.queries, ["max by (m) (running{job=\"g\"})"]);
        assert_eq!(
            p.thresholds,
            [Threshold::Series { op: CmpOp::Lt, query: "max by (m) (desired{job=\"g\"})".into() }]
        );
        let p = plan("a * on(x) group_left(y) b > on(x) group_left c");
        assert_eq!(p.queries, ["a * on(x) group_left(y) b"]);
        assert_eq!(p.thresholds, [Threshold::Series { op: CmpOp::Gt, query: "c".into() }]);
    }

    #[test]
    fn and_unless_chart_the_left_comparison() {
        let p = plan(
            "(\n  sum by (namespace, ingress) (rate(r{status=~\"5..\"}[5m]))\n  /\n  sum by (namespace, ingress) (rate(r[5m]))\n) > 0.02\nand on (namespace, ingress)\nsum by (namespace, ingress) (rate(r[5m])) > 1",
        );
        assert_eq!(
            p.queries,
            [
                "sum by (namespace, ingress) (rate(r{status=~\"5..\"}[5m]))\n  /\n  sum by (namespace, ingress) (rate(r[5m]))"
            ]
        );
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 0.02)]);
        let p = plan("(up == 0) unless on(job) maintenance == 1");
        assert_eq!(p.queries, ["up"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Eq, 0.0)]);
    }

    #[test]
    fn or_with_one_shared_threshold_charts_every_branch() {
        let p = plan("(count(a) or vector(0)) < 3\nor (count(b) or vector(0)) < 3");
        assert_eq!(p.queries, ["count(a) or vector(0)", "count(b) or vector(0)"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Lt, 3.0)]);
    }

    #[test]
    fn or_of_comparisons_charts_every_branch_with_every_line() {
        let p = plan("(x > 0.05 and deriv(x[5m]) >= 0) or (x < -0.05 and deriv(x[5m]) <= 0)");
        assert_eq!(p.queries, ["x"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 0.05), scalar(CmpOp::Lt, -0.05)]);
        let p = plan("a > 1 or b > 2");
        assert_eq!(p.queries, ["a", "b"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 1.0), scalar(CmpOp::Gt, 2.0)]);
    }

    #[test]
    fn or_with_a_branch_that_is_not_a_comparison_has_no_threshold() {
        let p = plan("sum(x) == 0 or absent_over_time(x[15m])");
        assert_eq!(p.queries, ["sum(x) == 0 or absent_over_time(x[15m])"]);
        assert!(p.thresholds.is_empty());
    }

    #[test]
    fn no_comparison_no_threshold() {
        for e in ["vector(1)", "absent(up{job=\"x\"})", "rate(x[5m]) * 100", "sum by (a) (x)"] {
            let p = plan(e);
            assert_eq!(p.queries, [e]);
            assert!(p.thresholds.is_empty(), "{e}");
        }
    }

    #[test]
    fn min_and_max_around_a_comparison_keep_the_line() {
        let p = plan("max by (w) (status{state=\"not_ready\"} == 1)");
        assert_eq!(p.queries, ["max by (w) (status{state=\"not_ready\"})"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Eq, 1.0)]);
        let p = plan("min(max_over_time(ready[5m]) == 0) by (namespace)");
        assert_eq!(p.queries, ["min(max_over_time(ready[5m])) by (namespace)"]);
        assert!(plan("sum by (w) (x > 1)").thresholds.is_empty(), "a sum of filtered values is not the sum");
    }

    #[test]
    fn chained_comparisons_keep_both_lines() {
        let p = plan("x > 1 < 5");
        assert_eq!(p.queries, ["x"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 1.0), scalar(CmpOp::Lt, 5.0)]);
    }

    #[test]
    fn operators_inside_strings_braces_and_comments_are_ignored() {
        let p = plan("# a > 1 comment\nx{a!=\"b > c\", d=~\"e|f\"} > 2 # tail < 3");
        assert_eq!(p.queries, ["x{a!=\"b > c\", d=~\"e|f\"}"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 2.0)]);
        let p = plan("x{a=\"#\"} > 2");
        assert_eq!(p.queries, ["x{a=\"#\"}"]);
    }

    #[test]
    fn keywords_inside_identifiers_are_not_operators() {
        let p = plan("android_or_unless_total > 1");
        assert_eq!(p.queries, ["android_or_unless_total"]);
        let p = plan("sum by (namespace) (x) offset 5m > 1");
        assert_eq!(p.queries, ["sum by (namespace) (x) offset 5m"]);
    }

    #[test]
    fn metricsql_default_and_if() {
        let p = plan("(rate(x[5m]) default 0) > 1");
        assert_eq!(p.queries, ["rate(x[5m]) default 0"]);
        let p = plan("x > 1 default 0");
        assert_eq!(p.queries, ["x"]);
        assert_eq!(p.thresholds, [scalar(CmpOp::Gt, 1.0)]);
        let p = plan("x > 1 if y");
        assert_eq!(p.queries, ["x"]);
    }

    /// Every alerting rule in production (hostnames and IPs replaced) gives a
    /// non-empty query whose brackets balance, and the planner never panics.
    #[test]
    fn every_production_rule() {
        #[derive(serde::Deserialize)]
        struct Rule {
            name: String,
            expr: String,
        }
        let rules: Vec<Rule> = serde_json::from_str(include_str!("../testdata/prod-exprs.json")).unwrap();
        assert!(rules.len() > 300);
        let mut with_threshold = 0;
        let mut without = vec![];
        for r in &rules {
            let p = plan(&r.expr);
            assert!(!p.queries.is_empty(), "{}", r.name);
            for q in p.queries.iter().chain(p.thresholds.iter().filter_map(|t| match t {
                Threshold::Series { query, .. } => Some(query),
                Threshold::Scalar { .. } => None,
            })) {
                assert!(!q.trim().is_empty(), "{}: empty query", r.name);
                assert!(balanced(q), "{}: unbalanced {q:?}", r.name);
                assert!(!q.contains('#'), "{}: comment left in {q:?}", r.name);
            }
            if p.thresholds.is_empty() {
                without.push(r.name.as_str());
            } else {
                with_threshold += 1;
            }
        }
        // The rest are absent(), vector(1), and arithmetic over filtered
        // series (2026-11-24: 307 of 329).
        assert!(with_threshold * 100 / rules.len() >= 93, "{with_threshold}/{}: {without:?}", rules.len());
    }

    fn balanced(q: &str) -> bool {
        let mut depth = 0i32;
        for t in lex(q) {
            match t.kind {
                Tok::Open => depth += 1,
                Tok::Close => depth -= 1,
                _ => {}
            }
            if depth < 0 {
                return false;
            }
        }
        depth == 0
    }

    #[test]
    fn production_examples() {
        let rules: Vec<serde_json::Value> =
            serde_json::from_str(include_str!("../testdata/prod-exprs.json")).unwrap();
        let get = |name: &str| {
            let r = rules.iter().find(|r| r["name"] == name).unwrap();
            plan(r["expr"].as_str().unwrap())
        };
        let p = get("GPUStackModelUnderReplicated");
        assert_eq!(p.queries.len(), 1);
        assert!(p.queries[0].contains("gpustack:model_running_instances"));
        assert!(matches!(&p.thresholds[..], [Threshold::Series { op: CmpOp::Lt, query }]
            if query.contains("gpustack:model_desired_instances")));
        let p = get("CrowdSecAgentDown");
        assert_eq!(p.thresholds, [scalar(CmpOp::Eq, 0.0)]);
        assert!(p.queries[0].starts_with("max(up"));
        let p = get("CephOSDNearFull");
        assert_eq!(p.thresholds, [scalar(CmpOp::Ge, 0.75)]);
        let p = get("EdgeRouterNodeMetricsMissing");
        assert_eq!(p.queries.len(), 3);
        assert_eq!(p.thresholds, [scalar(CmpOp::Lt, 3.0)]);
        let p = get("TargetDown");
        assert_eq!(p.queries[0], "up");
        assert_eq!(p.thresholds, [scalar(CmpOp::Eq, 0.0)]);
    }
}
