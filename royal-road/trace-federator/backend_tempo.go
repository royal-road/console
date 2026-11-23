package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// tempoBackend talks to any Tempo-compatible HTTP API: Grafana Tempo, or
// VictoriaTraces under /select/tempo. Queries pass through unchanged.
type tempoBackend struct {
	name   string
	base   string // URL prefix, e.g. http://victoria-traces:10428/select/tempo
	client *http.Client
	header http.Header
}

func newTempoBackend(cfg BackendConfig, client *http.Client) (*tempoBackend, error) {
	h := http.Header{}
	if cfg.BearerTokenFile != "" {
		tok, err := readSecretFile(cfg.BearerTokenFile)
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Bearer "+tok)
	}
	return &tempoBackend{name: cfg.Name, base: strings.TrimRight(cfg.URL, "/"), client: client, header: h}, nil
}

func (b *tempoBackend) Name() string { return b.name }

func (b *tempoBackend) get(ctx context.Context, path string, params url.Values, out any) (int, error) {
	u := b.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range b.header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	return resp.StatusCode, dec.Decode(out)
}

func pick(params map[string]string, keys ...string) url.Values {
	v := url.Values{}
	for _, k := range keys {
		if s, ok := params[k]; ok && s != "" {
			v.Set(k, s)
		}
	}
	return v
}

func (b *tempoBackend) Search(ctx context.Context, req *SearchRequest) ([]TraceSummary, error) {
	params := pick(req.Params, "q", "start", "end", "spss", "minDuration", "maxDuration", "tags")
	params.Set("limit", strconv.Itoa(req.Limit))
	var resp struct {
		Traces []map[string]any `json:"traces"`
	}
	if _, err := b.get(ctx, "/api/search", params, &resp); err != nil {
		return nil, err
	}
	out := make([]TraceSummary, 0, len(resp.Traces))
	for _, t := range resp.Traces {
		out = append(out, summaryFromTempo(t))
	}
	return out, nil
}

// summaryFromTempo converts a decoded Tempo search row, tolerating numeric or
// string startTimeUnixNano (VictoriaTraces changed it in v0.12).
func summaryFromTempo(t map[string]any) TraceSummary {
	s := TraceSummary{
		TraceID:           normalizeID(str(t["traceID"]), 32),
		RootServiceName:   str(t["rootServiceName"]),
		RootTraceName:     str(t["rootTraceName"]),
		StartTimeUnixNano: str(t["startTimeUnixNano"]),
		DurationMs:        int64(num(t["durationMs"])),
	}
	if raw, ok := t["serviceStats"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &s.ServiceStats)
	}
	if raw, ok := t["spanSet"]; ok && raw != nil {
		s.SpanSet, _ = json.Marshal(raw)
	}
	if raw, ok := t["spanSets"].([]any); ok {
		for _, ss := range raw {
			b, _ := json.Marshal(ss)
			s.SpanSets = append(s.SpanSets, b)
		}
	}
	return s
}

func (b *tempoBackend) Trace(ctx context.Context, req *TraceRequest) ([]map[string]any, error) {
	var resp struct {
		Trace struct {
			ResourceSpans []map[string]any `json:"resourceSpans"`
		} `json:"trace"`
	}
	code, err := b.get(ctx, "/api/v2/traces/"+url.PathEscape(req.TraceID), nil, &resp)
	if err != nil || code == http.StatusNotFound {
		return nil, err
	}
	return resp.Trace.ResourceSpans, nil
}

func (b *tempoBackend) Tags(ctx context.Context, req *TagsRequest) (*TagSet, error) {
	var resp struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	if _, err := b.get(ctx, "/api/v2/search/tags", pick(req.Params, "scope", "start", "end", "q"), &resp); err != nil {
		return nil, err
	}
	ts := &TagSet{}
	for _, s := range resp.Scopes {
		switch s.Name {
		case "resource":
			ts.Resource = append(ts.Resource, s.Tags...)
		case "span":
			ts.Span = append(ts.Span, s.Tags...)
		case "intrinsic":
			ts.Intrinsic = append(ts.Intrinsic, s.Tags...)
		}
	}
	return ts, nil
}

func (b *tempoBackend) TagValues(ctx context.Context, req *TagValuesRequest) ([]TagValue, error) {
	var resp struct {
		TagValues []TagValue `json:"tagValues"`
	}
	path := "/api/v2/search/tag/" + url.PathEscape(req.RawTag) + "/values"
	if _, err := b.get(ctx, path, pick(req.Params, "q", "start", "end", "limit"), &resp); err != nil {
		return nil, err
	}
	return resp.TagValues, nil
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func num(v any) float64 {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
