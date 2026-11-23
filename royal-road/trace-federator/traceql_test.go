package main

import (
	"strings"
	"testing"
	"time"
)

// Queries in the shapes traceql_from_filter.ts produces.
func TestParsePluginQueries(t *testing.T) {
	q, err := ParseTraceQL(`{ resource.service.name =~ "a|b\\.c" && name = "GET /x" && resource.k8s.namespace.name = "ns" && (status = error || status = unset) && duration >= 100ms && traceDuration <= 1.5s && span.http.status_code>=500 }`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Conds) != 7 {
		t.Fatalf("got %d conditions", len(q.Conds))
	}
	if got := q.Conds[0].Alternatives; len(got) != 2 || got[0] != "a" || got[1] != "b.c" {
		t.Errorf("alternatives = %q", got)
	}
	if c := q.Conds[3]; len(c.Or) != 2 || c.Or[0].Value.Str != "error" {
		t.Errorf("or group = %+v", c)
	}
	if c := q.Conds[4]; c.Value.Kind != ValDuration || c.Value.Duration != 100*time.Millisecond {
		t.Errorf("duration = %+v", c.Value)
	}
	if c := q.Conds[6]; !c.Attr.Is(ScopeSpan, "http.status_code") || c.Value.Num != 500 || c.Op != ">=" {
		t.Errorf("custom matcher = %+v", c)
	}
	rest, minD, maxD, err := q.TraceDurationFilter()
	if err != nil || len(rest) != 6 || minD != 0 || maxD != 1500*time.Millisecond {
		t.Errorf("traceDuration filter: %d %v %v %v", len(rest), minD, maxD, err)
	}
	for _, s := range []string{"{}", "{ }", `{ .foo = true } | select(span.a, resource.b)`} {
		if _, err := ParseTraceQL(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestParseRejectsWhatOnlyTempoCanRun(t *testing.T) {
	for _, s := range []string{
		`{ .a = "x" } >> { .b = "y" }`,
		`{ .a = "x" } && { .b = "y" }`,
		`{ .a = "x" } | count() > 2`,
		`{ .a = "x" || .b = "y" }`,
		`{ event.name = "x" }`,
		`{ .a = "unterminated }`,
		`{ .a =~ "(" }`,
	} {
		_, err := ParseTraceQL(s)
		if err == nil || !isUnsupported(err) {
			t.Errorf("%s: want unsupported, got %v", s, err)
		}
	}
}

func TestLiteralAlternation(t *testing.T) {
	if got := literalAlternation(`a|b\.c`); strings.Join(got, ",") != "a,b.c" {
		t.Errorf("got %q", got)
	}
	if got := literalAlternation(`a.*|b`); got != nil {
		t.Errorf("regex with metacharacters: got %q", got)
	}
}

func TestSignozExpr(t *testing.T) {
	q, err := ParseTraceQL(`{ resource.service.name =~ "A|B" && name =~ "GET.*" && (status = error || status = ok) && duration >= 1s && span.db.system = "postgresql" && .x != 3 && kind = server && name != "it's" }`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := signozExpr(q.Conds)
	if err != nil {
		t.Fatal(err)
	}
	want := `resource.service.name IN ('A', 'B') AND name REGEXP '^(?:GET.*)$' AND (has_error = true OR status_code_string = 'Ok') AND duration_nano >= 1000000000 AND attribute.db.system = 'postgresql' AND x != 3 AND kind_string = 'Server' AND name != 'it\'s'`
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	q, _ = ParseTraceQL(`{ rootName = "x" }`)
	if _, err := signozExpr(q.Conds); !isUnsupported(err) {
		t.Errorf("rootName: want unsupported, got %v", err)
	}
}

func TestSentryQuery(t *testing.T) {
	b := &sentryBackend{spanName: "span.name", unsupported: map[string]bool{"resource.k8s.namespace.name": true}}
	q, _ := ParseTraceQL(`{ resource.service.name =~ "web|api" && name = "GET /" && status = error && duration > 250ms && resource.deployment.environment = "production" && .http.response_status_code >= 500 && name =~ "POST.*" }`)
	got, only, exclude, err := b.sentryQuery(q.Conds)
	if err != nil {
		t.Fatal(err)
	}
	want := `span.name:"GET /" (has:span.status !span.status:ok) span.duration:>250ms environment:"production" http.response_status_code:>=500 span.name:"POST*"`
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	if len(only) != 2 || !only["web"] || !only["api"] || exclude != nil {
		t.Errorf("project selection only=%v exclude=%v", only, exclude)
	}
	for _, s := range []string{`{ resource.k8s.namespace.name = "x" }`, `{ .k8s.namespace.name = "x" }`, `{ name =~ "a+b" }`, `{ kind = server }`} {
		q, _ := ParseTraceQL(s)
		if _, _, _, err := b.sentryQuery(q.Conds); !isUnsupported(err) {
			t.Errorf("%s: want unsupported, got %v", s, err)
		}
	}
}

func TestNormalizeID(t *testing.T) {
	for in, want := range map[string]string{
		"ABC":                              "00000000000000000000000000000abc",
		"7a1b2c3d4e5f60718293a4b5c6d7e8f9": "7a1b2c3d4e5f60718293a4b5c6d7e8f9",
		"ehssPU5fYHGCk6S1xtfo+Q==":         "7a1b2c3d4e5f60718293a4b5c6d7e8f9",
	} {
		if got := normalizeID(in, 32); got != want {
			t.Errorf("normalizeID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeID("EAAAAAAAAAE=", 16); got != "1000000000000001" {
		t.Errorf("span ID: %q", got)
	}
}
