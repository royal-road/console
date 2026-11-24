package main

import (
	"fmt"
	"strconv"
	"strings"
)

// A Pyroscope label selector, parsed strictly: { m (, m)* } where
// m = label op "string", op is one of = != =~ !~, and the string is a
// double-quoted Go string. Selectors are always re-serialised from the parsed
// form before they reach Pyroscope, so nothing the caller wrote is forwarded
// verbatim.

type labelMatcher struct {
	Name, Op, Value string
}

type labelSelector []labelMatcher

func (s labelSelector) String() string {
	parts := make([]string, len(s))
	for i, m := range s {
		parts[i] = m.Name + m.Op + strconv.Quote(m.Value)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// withName returns the matchers on one label.
func (s labelSelector) withName(name string) []labelMatcher {
	var out []labelMatcher
	for _, m := range s {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

func parseLabelSelector(in string) (labelSelector, error) {
	p := selectorParser{s: in}
	p.space()
	if !p.eat("{") {
		return nil, fmt.Errorf("selector must start with {")
	}
	var out labelSelector
	for {
		p.space()
		name := p.labelName()
		if name == "" {
			return nil, fmt.Errorf("expected a label name at offset %d", p.i)
		}
		p.space()
		op := p.op()
		if op == "" {
			return nil, fmt.Errorf("expected = != =~ or !~ after %s", name)
		}
		p.space()
		val, err := p.quoted()
		if err != nil {
			return nil, fmt.Errorf("value of %s: %w", name, err)
		}
		out = append(out, labelMatcher{Name: name, Op: op, Value: val})
		p.space()
		if p.eat(",") {
			continue
		}
		if p.eat("}") {
			break
		}
		return nil, fmt.Errorf("expected , or } at offset %d", p.i)
	}
	p.space()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("unexpected text after }")
	}
	return out, nil
}

type selectorParser struct {
	s string
	i int
}

func (p *selectorParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *selectorParser) eat(tok string) bool {
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *selectorParser) labelName() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || p.i > start && c >= '0' && c <= '9' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

func (p *selectorParser) op() string {
	for _, op := range []string{"=~", "!~", "!=", "="} {
		if p.eat(op) {
			return op
		}
	}
	return ""
}

func (p *selectorParser) quoted() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", fmt.Errorf("expected a double-quoted string")
	}
	for j := p.i + 1; j < len(p.s); j++ {
		switch p.s[j] {
		case '\\':
			j++
		case '"':
			v, err := strconv.Unquote(p.s[p.i : j+1])
			if err != nil {
				return "", err
			}
			p.i = j + 1
			return v, nil
		}
	}
	return "", fmt.Errorf("unterminated string")
}
