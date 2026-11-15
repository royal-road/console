package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This file parses the subset of TraceQL that the distributed-tracing console
// plugin generates (Toolbar/Filter/traceql_from_filter.ts), plus simple
// hand-written variations of it:
//
//	{}
//	{ C && C && ... }            one spanset, conjunction of conditions
//	C := attr op value | ( C || C ... )
//	attr := name | status | kind | duration | traceDuration | rootName |
//	        rootServiceName | statusMessage | .x | span.x | resource.x
//	op := = != =~ !~ > >= < <=
//	| select(...)                tolerated and ignored by non-Tempo backends
//
// Backends that speak TraceQL natively (type tempo) get the raw query. The
// others translate the parsed form and refuse anything outside it with
// errUnsupported, which the federator reports as a partial result.

// Scope of an attribute reference.
type AttrScope string

const (
	ScopeIntrinsic AttrScope = "intrinsic"
	ScopeSpan      AttrScope = "span"
	ScopeResource  AttrScope = "resource"
	ScopeAny       AttrScope = "any" // ".x": span or resource
)

type Attr struct {
	Scope AttrScope
	Name  string
}

func (a Attr) String() string {
	switch a.Scope {
	case ScopeIntrinsic:
		return a.Name
	case ScopeAny:
		return "." + a.Name
	}
	return string(a.Scope) + "." + a.Name
}

// Is reports whether the attribute is the given scoped name, treating ".x" as
// matching both span.x and resource.x.
func (a Attr) Is(scope AttrScope, name string) bool {
	if a.Name != name {
		return false
	}
	return a.Scope == scope || (a.Scope == ScopeAny && scope != ScopeIntrinsic)
}

type ValueKind int

const (
	ValString ValueKind = iota
	ValNumber
	ValBool
	ValDuration
	ValEnum // status and kind values: error, ok, unset, server, client, ...
)

type Value struct {
	Kind     ValueKind
	Str      string
	Num      float64
	Bool     bool
	Duration time.Duration
}

func (v Value) String() string {
	switch v.Kind {
	case ValString:
		return strconv.Quote(v.Str)
	case ValNumber:
		return strconv.FormatFloat(v.Num, 'f', -1, 64)
	case ValBool:
		return strconv.FormatBool(v.Bool)
	case ValDuration:
		return v.Duration.String()
	}
	return v.Str
}

// Cond is either a comparison (Attr/Op/Value) or a disjunction (Or).
type Cond struct {
	Attr  Attr
	Op    string
	Value Value
	// Alternatives holds the literal strings of a regex that is a plain
	// alternation, e.g. =~ "a|b|c". Nil for any other regex.
	Alternatives []string
	Or           []Cond
}

// Query is a parsed TraceQL query. Conds are ANDed.
type Query struct {
	Raw   string
	Conds []Cond
	// Select lists attributes from "| select(...)"; translating backends
	// ignore it.
	Select []string
}

type unsupportedError struct{ msg string }

func (e *unsupportedError) Error() string { return "unsupported query: " + e.msg }

func errUnsupported(format string, a ...any) error {
	return &unsupportedError{fmt.Sprintf(format, a...)}
}

func isUnsupported(err error) bool {
	_, ok := err.(*unsupportedError)
	return ok
}

var intrinsics = map[string]bool{
	"name": true, "status": true, "kind": true, "duration": true,
	"traceDuration": true, "rootName": true, "rootServiceName": true,
	"statusMessage": true,
}

// ParseTraceQL parses q. An error from it is always an unsupportedError: the
// query may still be valid TraceQL for a native backend.
func ParseTraceQL(q string) (*Query, error) {
	p := &parser{toks: nil}
	toks, err := tokenize(q)
	if err != nil {
		return nil, err
	}
	p.toks = toks
	query := &Query{Raw: q}
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	if !p.peekIs("}") {
		conds, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		query.Conds = conds
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	for p.peekIs("|") {
		p.next()
		t := p.next()
		if t.text != "select" {
			return nil, errUnsupported("pipeline %q", t.text)
		}
		if err := p.expect("("); err != nil {
			return nil, err
		}
		for !p.peekIs(")") {
			t := p.next()
			if t.kind == tokEOF {
				return nil, errUnsupported("unterminated select")
			}
			if t.text != "," {
				query.Select = append(query.Select, t.text)
			}
		}
		p.next()
	}
	if t := p.next(); t.kind != tokEOF {
		return nil, errUnsupported("%q after the spanset (structural operators and multiple spansets need a Tempo backend)", t.text)
	}
	return query, nil
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokPunct
	tokIdent
	tokString
	tokNumber
)

type token struct {
	kind tokKind
	text string
}

func tokenize(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, errUnsupported("unterminated string")
			}
			toks = append(toks, token{tokString, b.String()})
			i = j + 1
		case strings.ContainsRune("{}(),", rune(c)):
			toks = append(toks, token{tokPunct, string(c)})
			i++
		case strings.HasPrefix(s[i:], "&&") || strings.HasPrefix(s[i:], "||") ||
			strings.HasPrefix(s[i:], "=~") || strings.HasPrefix(s[i:], "!~") ||
			strings.HasPrefix(s[i:], "!=") || strings.HasPrefix(s[i:], ">=") ||
			strings.HasPrefix(s[i:], "<=") || strings.HasPrefix(s[i:], ">>") ||
			strings.HasPrefix(s[i:], "<<"):
			toks = append(toks, token{tokPunct, s[i : i+2]})
			i += 2
		case strings.ContainsRune("=<>|~!&", rune(c)):
			toks = append(toks, token{tokPunct, string(c)})
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(s) && (s[j] == '.' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			toks = append(toks, token{tokNumber, s[i:j]})
			i = j
		case c == '.' || c == '_' || unicode.IsLetter(rune(c)):
			j := i + 1
			for j < len(s) && (s[j] == '.' || s[j] == '_' || s[j] == '-' || s[j] == '/' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			toks = append(toks, token{tokIdent, s[i:j]})
			i = j
		default:
			return nil, errUnsupported("unexpected character %q", c)
		}
	}
	return toks, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) next() token {
	if p.pos >= len(p.toks) {
		return token{kind: tokEOF}
	}
	t := p.toks[p.pos]
	p.pos++
	return t
}

func (p *parser) peekIs(text string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == tokPunct && p.toks[p.pos].text == text
}

func (p *parser) expect(text string) error {
	t := p.next()
	if t.kind != tokPunct || t.text != text {
		return errUnsupported("expected %q, got %q", text, t.text)
	}
	return nil
}

func (p *parser) parseAnd() ([]Cond, error) {
	var conds []Cond
	for {
		c, err := p.parseCond()
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
		if p.peekIs("&&") {
			p.next()
			continue
		}
		if p.peekIs("||") {
			return nil, errUnsupported("|| outside parentheses")
		}
		return conds, nil
	}
}

func (p *parser) parseCond() (Cond, error) {
	if p.peekIs("(") {
		p.next()
		var alts []Cond
		for {
			c, err := p.parseCond()
			if err != nil {
				return Cond{}, err
			}
			alts = append(alts, c)
			if p.peekIs("||") {
				p.next()
				continue
			}
			if p.peekIs("&&") {
				return Cond{}, errUnsupported("&& inside a parenthesised group")
			}
			break
		}
		if err := p.expect(")"); err != nil {
			return Cond{}, err
		}
		if len(alts) == 1 {
			return alts[0], nil
		}
		return Cond{Or: alts}, nil
	}
	t := p.next()
	if t.kind != tokIdent {
		return Cond{}, errUnsupported("expected an attribute, got %q", t.text)
	}
	attr, err := parseAttr(t.text)
	if err != nil {
		return Cond{}, err
	}
	opTok := p.next()
	op := opTok.text
	switch op {
	case "=", "!=", "=~", "!~", ">", ">=", "<", "<=":
	default:
		return Cond{}, errUnsupported("operator %q", op)
	}
	vt := p.next()
	val, err := parseValue(vt)
	if err != nil {
		return Cond{}, err
	}
	c := Cond{Attr: attr, Op: op, Value: val}
	if op == "=~" || op == "!~" {
		if val.Kind != ValString {
			return Cond{}, errUnsupported("regex needs a string")
		}
		if _, err := regexp.Compile(val.Str); err != nil {
			return Cond{}, errUnsupported("invalid regex %q", val.Str)
		}
		c.Alternatives = literalAlternation(val.Str)
	}
	return c, nil
}

func parseAttr(s string) (Attr, error) {
	switch {
	case strings.HasPrefix(s, "resource."):
		return Attr{ScopeResource, strings.TrimPrefix(s, "resource.")}, nil
	case strings.HasPrefix(s, "span."):
		return Attr{ScopeSpan, strings.TrimPrefix(s, "span.")}, nil
	case strings.HasPrefix(s, "."):
		return Attr{ScopeAny, strings.TrimPrefix(s, ".")}, nil
	case intrinsics[s]:
		return Attr{ScopeIntrinsic, s}, nil
	case strings.HasPrefix(s, "event.") || strings.HasPrefix(s, "link.") || strings.HasPrefix(s, "instrumentation.") || strings.HasPrefix(s, "trace:") || strings.HasPrefix(s, "span:"):
		return Attr{}, errUnsupported("attribute %q", s)
	}
	return Attr{}, errUnsupported("unknown attribute %q", s)
}

func parseValue(t token) (Value, error) {
	switch t.kind {
	case tokString:
		return Value{Kind: ValString, Str: t.text}, nil
	case tokNumber:
		if f, err := strconv.ParseFloat(t.text, 64); err == nil {
			return Value{Kind: ValNumber, Num: f}, nil
		}
		if d, err := time.ParseDuration(t.text); err == nil {
			return Value{Kind: ValDuration, Duration: d}, nil
		}
		return Value{}, errUnsupported("bad number or duration %q", t.text)
	case tokIdent:
		switch t.text {
		case "true", "false":
			return Value{Kind: ValBool, Bool: t.text == "true"}, nil
		case "error", "ok", "unset", "server", "client", "producer", "consumer", "internal", "unspecified":
			return Value{Kind: ValEnum, Str: t.text}, nil
		}
		if d, err := time.ParseDuration(t.text); err == nil {
			return Value{Kind: ValDuration, Duration: d}, nil
		}
	}
	return Value{}, errUnsupported("value %q", t.text)
}

// literalAlternation returns the literal strings of a regex that is only an
// alternation of literals ("a|b|c", with regexp.QuoteMeta escapes allowed),
// or nil.
func literalAlternation(re string) []string {
	var out []string
	for _, part := range strings.Split(re, "|") {
		var b strings.Builder
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c == '\\' && i+1 < len(part) {
				i++
				b.WriteByte(part[i])
				continue
			}
			if strings.IndexByte(`.+*?()[]{}^$`, c) >= 0 {
				return nil
			}
			b.WriteByte(c)
		}
		out = append(out, b.String())
	}
	return out
}

// TraceDurationFilter extracts traceDuration conditions, which every
// translating backend applies after fetching (as a post-filter), and returns
// the remaining conditions.
func (q *Query) TraceDurationFilter() (rest []Cond, min, max time.Duration, err error) {
	for _, c := range q.Conds {
		if c.Or == nil && c.Attr.Scope == ScopeIntrinsic && c.Attr.Name == "traceDuration" {
			if c.Value.Kind != ValDuration {
				return nil, 0, 0, errUnsupported("traceDuration needs a duration")
			}
			switch c.Op {
			case ">", ">=":
				min = c.Value.Duration
			case "<", "<=":
				max = c.Value.Duration
			default:
				return nil, 0, 0, errUnsupported("traceDuration %s", c.Op)
			}
			continue
		}
		rest = append(rest, c)
	}
	return rest, min, max, nil
}
