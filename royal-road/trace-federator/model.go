package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Backend is one trace store. Implementations translate the Tempo HTTP API
// the console plugin speaks into the store's own API.
type Backend interface {
	Name() string
	Search(ctx context.Context, req *SearchRequest) ([]TraceSummary, error)
	// Trace returns the store's spans of one trace as OTLP/JSON resourceSpans
	// (Tempo's /api/v2/traces shape), or nil when the store has none.
	Trace(ctx context.Context, req *TraceRequest) ([]map[string]any, error)
	Tags(ctx context.Context, req *TagsRequest) (*TagSet, error)
	TagValues(ctx context.Context, req *TagValuesRequest) ([]TagValue, error)
}

// ProjectScope is one Sentry project (and optionally environments) a viewer
// may read, from a SentryProjectBinding.
type ProjectScope struct {
	Organization string
	Project      string
	Environments []string // empty: all environments
}

// Scope restricts what a backend may return to the current viewer. Nil
// Projects means "no project restriction" and is only used by backends that
// are not project-scoped.
type Scope struct {
	Projects []ProjectScope
}

type SearchRequest struct {
	Raw        string    // TraceQL as sent by the client
	Query      *Query    // parsed form, nil when ParseErr is set
	ParseErr   error     // why Raw is outside the supported subset
	Start, End time.Time // zero when the client sent none
	Limit      int
	Slices     int               // search this many equal sub-windows; <= 1: one query
	Params     map[string]string // original query parameters (Tempo passthrough)
	Scope      *Scope
}

type TraceRequest struct {
	TraceID string // 32 lowercase hex characters
	Scope   *Scope
}

type TagsRequest struct {
	Scope      *Scope
	Start, End time.Time
	Params     map[string]string
}

type TagValuesRequest struct {
	Tag        Attr   // parsed tag
	RawTag     string // as sent
	Q          string // optional TraceQL that narrows the values
	Start, End time.Time
	Params     map[string]string
	Scope      *Scope
}

// TraceSummary is one row of Tempo's /api/search response.
type TraceSummary struct {
	TraceID           string                  `json:"traceID"`
	RootServiceName   string                  `json:"rootServiceName,omitempty"`
	RootTraceName     string                  `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string                  `json:"startTimeUnixNano"`
	DurationMs        int64                   `json:"durationMs,omitempty"`
	ServiceStats      map[string]ServiceStats `json:"serviceStats,omitempty"`
	SpanSet           json.RawMessage         `json:"spanSet,omitempty"`
	SpanSets          []json.RawMessage       `json:"spanSets,omitempty"`
	// Stores lists the backends that returned this trace. Tempo clients
	// ignore unknown fields.
	Stores []string `json:"federationStores,omitempty"`
}

type ServiceStats struct {
	SpanCount  int `json:"spanCount"`
	ErrorCount int `json:"errorCount,omitempty"`
}

func (t *TraceSummary) startNanos() int64 {
	n, _ := strconv.ParseInt(t.StartTimeUnixNano, 10, 64)
	return n
}

type TagSet struct {
	Resource  []string
	Span      []string
	Intrinsic []string
}

type TagValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

const rootNotReceived = "<root span not yet received>"

// normalizeID returns a lowercase hex ID of the given width (32 for traces,
// 16 for spans) from hex or base64 input, left-padding short hex IDs (Tempo
// trims leading zeros in search results).
func normalizeID(s string, width int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if isHex(s) && len(s) <= width {
		return strings.Repeat("0", width-len(s)) + strings.ToLower(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b)*2 == width {
		return hex.EncodeToString(b)
	}
	return strings.ToLower(s)
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// OTLP/JSON helpers for backends that build spans themselves.

func kvString(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}

func kvAny(k string, v any) map[string]any {
	switch x := v.(type) {
	case string:
		return kvString(k, x)
	case bool:
		return map[string]any{"key": k, "value": map[string]any{"boolValue": x}}
	case float64:
		if x == float64(int64(x)) {
			return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.FormatInt(int64(x), 10)}}
		}
		return map[string]any{"key": k, "value": map[string]any{"doubleValue": x}}
	case int64:
		return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.FormatInt(x, 10)}}
	case int:
		return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.Itoa(x)}}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.FormatInt(i, 10)}}
		}
		f, _ := x.Float64()
		return map[string]any{"key": k, "value": map[string]any{"doubleValue": f}}
	case nil:
		return nil
	}
	b, _ := json.Marshal(v)
	return kvString(k, string(b))
}

// OTLP span kind and status codes.
const (
	kindUnspecified = 0
	kindInternal    = 1
	kindServer      = 2
	kindClient      = 3
	kindProducer    = 4
	kindConsumer    = 5

	statusUnset = 0
	statusOK    = 1
	statusError = 2
)

func resourceSpans(resourceAttrs []any, spans []any) map[string]any {
	return map[string]any{
		"resource":   map[string]any{"attributes": resourceAttrs},
		"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "trace-federator"}, "spans": spans}},
	}
}
