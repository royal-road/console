package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// signozBackend reads SigNoz (checked against v0.144) through its v5 query
// API, POST /api/v5/query_range, with a service-account API key:
//   - search: requestType "raw" for the most recent matching spans' trace
//     IDs, then requestType "trace" for those traces' summaries (root name
//     and service, duration_nano, span_count), then a raw query for the root
//     spans' start times
//   - trace by ID: requestType "raw" filtered on trace_id
//   - tags: GET /api/v1/fields/keys and /api/v1/fields/values
type signozBackend struct {
	name        string
	base        string
	apiKey      string
	client      *http.Client
	maxLookback time.Duration
}

func newSignozBackend(cfg BackendConfig, client *http.Client) (*signozBackend, error) {
	key, err := readSecretFile(cfg.APIKeyFile)
	if err != nil {
		return nil, err
	}
	return &signozBackend{
		name: cfg.Name, base: strings.TrimRight(cfg.URL, "/"), apiKey: key,
		client: client, maxLookback: cfg.MaxLookback.Duration,
	}, nil
}

func (b *signozBackend) Name() string { return b.name }

func (b *signozBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("SIGNOZ-API-KEY", b.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(data)), 200))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

type signozRows struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				Rows []struct {
					Data map[string]any `json:"data"`
				} `json:"rows"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func (r *signozRows) rows() []map[string]any {
	var out []map[string]any
	for _, res := range r.Data.Data.Results {
		for _, row := range res.Rows {
			out = append(out, row.Data)
		}
	}
	return out
}

func (b *signozBackend) query(ctx context.Context, requestType string, start, end time.Time, spec map[string]any) ([]map[string]any, error) {
	spec["name"] = "A"
	spec["signal"] = "traces"
	body := map[string]any{
		"start":       start.UnixMilli(),
		"end":         end.UnixMilli(),
		"requestType": requestType,
		"compositeQuery": map[string]any{
			"queries": []any{map[string]any{"type": "builder_query", "spec": spec}},
		},
	}
	var resp signozRows
	if err := b.do(ctx, http.MethodPost, "/api/v5/query_range", body, &resp); err != nil {
		return nil, err
	}
	return resp.rows(), nil
}

func (b *signozBackend) Search(ctx context.Context, req *SearchRequest) ([]TraceSummary, error) {
	if req.Query == nil {
		return nil, req.ParseErr
	}
	conds, minD, maxD, err := req.Query.TraceDurationFilter()
	if err != nil {
		return nil, err
	}
	expr, err := signozExpr(conds)
	if err != nil {
		return nil, err
	}
	start, end := defaultWindow(req.Start, req.End)
	limit := req.Limit
	if minD > 0 || maxD > 0 {
		limit *= 3 // the summaries below are post-filtered on duration
	}
	candidates, err := b.candidates(ctx, start, end, expr, minD, maxD, limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return []TraceSummary{}, nil
	}
	quotedCandidates := make([]string, len(candidates))
	for i, id := range candidates {
		quotedCandidates[i] = sqlQuote(id)
	}
	// 2. Summaries (root name and service, duration, span count) of those.
	rows, err := b.query(ctx, "trace", start.Add(-time.Hour), end, map[string]any{
		"filter": map[string]any{"expression": "trace_id IN (" + strings.Join(quotedCandidates, ", ") + ")"},
		"limit":  len(candidates),
	})
	if err != nil {
		return nil, err
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[normalizeID(str(r["trace_id"]), 32)] = r
	}
	var out []TraceSummary
	var ids []string
	for _, id := range candidates {
		r, ok := byID[id]
		if !ok {
			continue
		}
		d := time.Duration(int64(num(r["duration_nano"])))
		if (minD > 0 && d < minD) || (maxD > 0 && d > maxD) {
			continue
		}
		svc := str(r["service.name"])
		t := TraceSummary{
			TraceID:         id,
			RootServiceName: svc,
			RootTraceName:   str(r["name"]),
			DurationMs:      d.Milliseconds(),
		}
		if svc != "" {
			t.ServiceStats = map[string]ServiceStats{svc: {SpanCount: int(num(r["span_count"]))}}
		}
		out = append(out, t)
		ids = append(ids, t.TraceID)
		if len(out) >= req.Limit {
			break
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	// The trace rows carry no start time; read it from the root spans.
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = sqlQuote(id)
	}
	roots, err := b.query(ctx, "raw", start.Add(-time.Hour), end, map[string]any{
		"filter":       map[string]any{"expression": "trace_id IN (" + strings.Join(quoted, ", ") + ") AND isRoot = true"},
		"selectFields": []any{map[string]any{"name": "trace_id", "fieldContext": "span"}, map[string]any{"name": "timestamp", "fieldContext": "span"}},
		"limit":        len(ids) * 2,
	})
	if err != nil {
		return nil, err
	}
	startOf := map[string]int64{}
	for _, r := range roots {
		if ts, err := time.Parse(time.RFC3339Nano, str(r["timestamp"])); err == nil {
			startOf[normalizeID(str(r["trace_id"]), 32)] = ts.UnixNano()
		}
	}
	for i := range out {
		s, ok := startOf[out[i].TraceID]
		if !ok {
			// Root span not in SigNoz: the end of the window is the best
			// upper bound we have without another query.
			s = end.UnixNano()
			out[i].RootServiceName = rootNotReceived
		}
		out[i].StartTimeUnixNano = strconv.FormatInt(s, 10)
	}
	return out, nil
}

// candidates returns up to limit trace IDs, most recent first.
//
// Without a trace duration condition, the most recent matching spans give the
// most recent matching traces (Tempo semantics: a trace matches if any span
// does). The "trace" request type cannot do this: it ignores order and returns
// the longest traces first.
//
// With one, recent spans would mostly belong to short traces, so candidates
// come from root spans with a matching duration instead (root span duration
// stands in for trace duration), narrowed by the other conditions.
func (b *signozBackend) candidates(ctx context.Context, start, end time.Time, expr string, minD, maxD time.Duration, limit int) ([]string, error) {
	recent := func(filter string, n int) ([]string, error) {
		spec := map[string]any{
			"selectFields": []any{map[string]any{"name": "trace_id", "fieldContext": "span"}},
			"order":        []any{map[string]any{"key": map[string]any{"name": "timestamp"}, "direction": "desc"}},
			"limit":        min(n*50, 10000), // big traces repeat one trace ID many times
		}
		if filter != "" {
			spec["filter"] = map[string]any{"expression": filter}
		}
		rows, err := b.query(ctx, "raw", start, end, spec)
		if err != nil {
			return nil, err
		}
		var ids []string
		seen := map[string]bool{}
		for _, r := range rows {
			id := normalizeID(str(r["trace_id"]), 32)
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
				if len(ids) >= n {
					break
				}
			}
		}
		return ids, nil
	}
	if minD == 0 && maxD == 0 {
		if expr == "" {
			return recent("isRoot = true", limit)
		}
		return recent(expr, limit)
	}
	rootFilter := "isRoot = true"
	if minD > 0 {
		rootFilter += " AND duration_nano >= " + strconv.FormatInt(minD.Nanoseconds(), 10)
	}
	if maxD > 0 {
		rootFilter += " AND duration_nano <= " + strconv.FormatInt(maxD.Nanoseconds(), 10)
	}
	if expr == "" {
		return recent(rootFilter, limit)
	}
	roots, err := recent(rootFilter, limit*4)
	if err != nil || len(roots) == 0 {
		return roots, err
	}
	quoted := make([]string, len(roots))
	for i, id := range roots {
		quoted[i] = sqlQuote(id)
	}
	matching, err := recent("("+expr+") AND trace_id IN ("+strings.Join(quoted, ", ")+")", len(roots))
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, id := range matching {
		keep[id] = true
	}
	var out []string
	for _, id := range roots {
		if keep[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// signozExpr translates conditions into the SigNoz v5 filter expression
// language.
func signozExpr(conds []Cond) (string, error) {
	var parts []string
	for _, c := range conds {
		s, err := signozCond(c)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " AND "), nil
}

func signozCond(c Cond) (string, error) {
	if c.Or != nil {
		var alts []string
		for _, a := range c.Or {
			s, err := signozCond(a)
			if err != nil {
				return "", err
			}
			alts = append(alts, s)
		}
		return "(" + strings.Join(alts, " OR ") + ")", nil
	}
	a := c.Attr
	if a.Scope == ScopeIntrinsic {
		switch a.Name {
		case "status":
			if c.Value.Kind != ValEnum || (c.Op != "=" && c.Op != "!=") {
				return "", errUnsupported("status %s %s", c.Op, c.Value)
			}
			var s string
			switch c.Value.Str {
			case "error":
				s = "has_error = true"
			case "ok":
				s = "status_code_string = 'Ok'"
			case "unset":
				s = "status_code_string = 'Unset'"
			default:
				return "", errUnsupported("status %s", c.Value.Str)
			}
			if c.Op == "!=" {
				s = "NOT (" + s + ")"
			}
			return s, nil
		case "kind":
			if c.Value.Kind != ValEnum || (c.Op != "=" && c.Op != "!=") {
				return "", errUnsupported("kind %s %s", c.Op, c.Value)
			}
			return "kind_string " + c.Op + " " + sqlQuote(capitalize(c.Value.Str)), nil
		case "duration":
			if c.Value.Kind != ValDuration {
				return "", errUnsupported("duration needs a duration value")
			}
			return comparison("duration_nano", c.Op, strconv.FormatInt(c.Value.Duration.Nanoseconds(), 10))
		case "name":
			return stringCond("name", c)
		case "statusMessage":
			return stringCond("status_message", c)
		}
		return "", errUnsupported("intrinsic %s", a.Name)
	}
	field := a.Name
	switch a.Scope {
	case ScopeResource:
		field = "resource." + a.Name
	case ScopeSpan:
		field = "attribute." + a.Name
	}
	switch c.Value.Kind {
	case ValString:
		return stringCond(field, c)
	case ValNumber:
		return comparison(field, c.Op, strconv.FormatFloat(c.Value.Num, 'f', -1, 64))
	case ValBool:
		if c.Op != "=" && c.Op != "!=" {
			return "", errUnsupported("%s on a bool", c.Op)
		}
		return field + " " + c.Op + " " + strconv.FormatBool(c.Value.Bool), nil
	case ValDuration:
		return comparison(field, c.Op, strconv.FormatInt(c.Value.Duration.Nanoseconds(), 10))
	}
	return "", errUnsupported("value %s for %s", c.Value, a)
}

func stringCond(field string, c Cond) (string, error) {
	if c.Value.Kind != ValString {
		return "", errUnsupported("%s needs a string", field)
	}
	switch c.Op {
	case "=", "!=":
		return field + " " + c.Op + " " + sqlQuote(c.Value.Str), nil
	case "=~", "!~":
		not := ""
		if c.Op == "!~" {
			not = "NOT "
		}
		if c.Alternatives != nil {
			q := make([]string, len(c.Alternatives))
			for i, s := range c.Alternatives {
				q[i] = sqlQuote(s)
			}
			return field + " " + not + "IN (" + strings.Join(q, ", ") + ")", nil
		}
		// TraceQL regexes match the whole value; ClickHouse's do not.
		return field + " " + not + "REGEXP " + sqlQuote("^(?:"+c.Value.Str+")$"), nil
	}
	return comparison(field, c.Op, sqlQuote(c.Value.Str))
}

func comparison(field, op, value string) (string, error) {
	switch op {
	case "=", "!=", ">", ">=", "<", "<=":
		return field + " " + op + " " + value, nil
	}
	return "", errUnsupported("%s %s", field, op)
}

func sqlQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func defaultWindow(start, end time.Time) (time.Time, time.Time) {
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		start = end.Add(-time.Hour)
	}
	return start, end
}

func (b *signozBackend) Trace(ctx context.Context, req *TraceRequest) ([]map[string]any, error) {
	end := time.Now()
	rows, err := b.query(ctx, "raw", end.Add(-b.maxLookback), end, map[string]any{
		"filter": map[string]any{"expression": "trace_id = " + sqlQuote(req.TraceID)},
		"limit":  10000,
	})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return signozRowsToOTLP(rows), nil
}

// signozRowsToOTLP groups raw span rows by resource into OTLP/JSON
// resourceSpans.
func signozRowsToOTLP(rows []map[string]any) []map[string]any {
	type group struct {
		resource []any
		spans    []any
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range rows {
		res, _ := r["resource"].(map[string]any)
		key := canonicalJSON(res)
		g, ok := groups[key]
		if !ok {
			g = &group{resource: attrsFromMap(res)}
			groups[key] = g
			order = append(order, key)
		}
		g.spans = append(g.spans, signozSpan(r))
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		out = append(out, resourceSpans(groups[k].resource, groups[k].spans))
	}
	return out
}

func signozSpan(r map[string]any) map[string]any {
	ts, _ := time.Parse(time.RFC3339Nano, str(r["timestamp"]))
	start := ts.UnixNano()
	dur := int64(num(r["duration_nano"]))
	span := map[string]any{
		"traceId":           normalizeID(str(r["trace_id"]), 32),
		"spanId":            normalizeID(str(r["span_id"]), 16),
		"name":              str(r["name"]),
		"kind":              int(num(r["kind"])),
		"startTimeUnixNano": strconv.FormatInt(start, 10),
		"endTimeUnixNano":   strconv.FormatInt(start+dur, 10),
		"status":            map[string]any{"code": int(num(r["status_code"])), "message": str(r["status_message"])},
	}
	if p := str(r["parent_span_id"]); p != "" {
		span["parentSpanId"] = normalizeID(p, 16)
	}
	attrs, _ := r["attributes"].(map[string]any)
	span["attributes"] = attrsFromMap(attrs)
	var events []any
	if evs, ok := r["events"].([]any); ok {
		for _, e := range evs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ev := map[string]any{"name": str(em["name"]), "timeUnixNano": str(em["timeUnixNano"])}
			if am, ok := em["attributeMap"].(map[string]any); ok {
				ev["attributes"] = attrsFromMap(am)
			}
			events = append(events, ev)
		}
	}
	if events != nil {
		span["events"] = events
	}
	var links []any
	if ls, ok := r["links"].([]any); ok {
		for _, l := range ls {
			if lm, ok := l.(map[string]any); ok {
				links = append(links, map[string]any{
					"traceId": normalizeID(str(lm["traceId"]), 32),
					"spanId":  normalizeID(str(lm["spanId"]), 16),
				})
			}
		}
	}
	if links != nil {
		span["links"] = links
	}
	return span
}

func attrsFromMap(m map[string]any) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		if kv := kvAny(k, m[k]); kv != nil {
			out = append(out, kv)
		}
	}
	return out
}

func canonicalJSON(v any) string {
	b, _ := json.Marshal(v) // map keys are sorted by encoding/json
	return string(b)
}

var signozIntrinsics = []string{"duration", "kind", "name", "status", "statusMessage", "traceDuration"}

func (b *signozBackend) Tags(ctx context.Context, req *TagsRequest) (*TagSet, error) {
	var resp struct {
		Data struct {
			Keys map[string][]struct {
				Name         string `json:"name"`
				FieldContext string `json:"fieldContext"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := b.do(ctx, http.MethodGet, "/api/v1/fields/keys?signal=traces&searchText=&limit=1000", nil, &resp); err != nil {
		return nil, err
	}
	ts := &TagSet{Intrinsic: signozIntrinsics}
	for _, list := range resp.Data.Keys {
		for _, k := range list {
			switch k.FieldContext {
			case "resource":
				ts.Resource = append(ts.Resource, k.Name)
			case "attribute":
				ts.Span = append(ts.Span, k.Name)
			}
		}
	}
	return ts, nil
}

func (b *signozBackend) TagValues(ctx context.Context, req *TagValuesRequest) ([]TagValue, error) {
	params := url.Values{"signal": {"traces"}, "searchText": {""}, "limit": {"1000"}}
	switch {
	case req.Tag.Scope == ScopeResource:
		params.Set("name", req.Tag.Name)
		params.Set("fieldContext", "resource")
	case req.Tag.Scope == ScopeSpan:
		params.Set("name", req.Tag.Name)
		params.Set("fieldContext", "attribute")
	case req.Tag.Scope == ScopeAny:
		params.Set("name", req.Tag.Name)
	case req.Tag.Is(ScopeIntrinsic, "name"):
		params.Set("name", "name")
	case req.Tag.Is(ScopeIntrinsic, "status"):
		return []TagValue{{"keyword", "error"}, {"keyword", "ok"}, {"keyword", "unset"}}, nil
	case req.Tag.Is(ScopeIntrinsic, "kind"):
		return []TagValue{{"keyword", "server"}, {"keyword", "client"}, {"keyword", "producer"}, {"keyword", "consumer"}, {"keyword", "internal"}}, nil
	default:
		return nil, nil
	}
	var resp struct {
		Data struct {
			Values struct {
				StringValues []string  `json:"stringValues"`
				NumberValues []float64 `json:"numberValues"`
				BoolValues   []bool    `json:"boolValues"`
			} `json:"values"`
		} `json:"data"`
	}
	if err := b.do(ctx, http.MethodGet, "/api/v1/fields/values?"+params.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	var out []TagValue
	for _, s := range resp.Data.Values.StringValues {
		out = append(out, TagValue{"string", s})
	}
	for _, n := range resp.Data.Values.NumberValues {
		out = append(out, TagValue{"int", strconv.FormatFloat(n, 'f', -1, 64)})
	}
	for _, v := range resp.Data.Values.BoolValues {
		out = append(out, TagValue{"bool", strconv.FormatBool(v)})
	}
	return out, nil
}
