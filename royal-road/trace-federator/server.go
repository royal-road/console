package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The HTTP surface has two parts:
//
//  1. The distributed-tracing console plugin's backend API, so the bridge's
//     plugin proxy can point straight at the federator:
//     GET /api/v1/list-tempo-resources
//     GET /proxy/{namespace}/{view}/{tenant}/api/...   (Tempo API, below)
//     Each "Tempo instance" the plugin lists is a view: one per backend, plus
//     one that queries all of them.
//  2. The Tempo HTTP API per view: /api/search, /api/v2/traces/{id},
//     /api/traces/{id}, /api/v2/search/tags, /api/search/tags,
//     /api/v2/search/tag/{tag}/values, /api/search/tag/{tag}/values,
//     /api/echo. Responses carry X-Federation-Status (JSON list of
//     {store, status, message}) so the UI can say which stores are missing.

type server struct {
	fed *federator
	cfg *Config
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w)
	})
	mux.HandleFunc("GET /api/v1/list-tempo-resources", s.listInstances)
	mux.HandleFunc("GET /proxy/{namespace}/{view}/{tenant}/{rest...}", s.tempoAPI)
	return mux
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		began := time.Now()
		next.ServeHTTP(rec, r)
		endpoint := endpointLabel(r.URL.Path)
		metricRequests.inc(endpoint, strconv.Itoa(rec.code))
		if endpoint != "healthz" && endpoint != "metrics" {
			slog.Info("request", "endpoint", endpoint, "code", rec.code, "ms", time.Since(began).Milliseconds(),
				"status", rec.Header().Get("X-Federation-Status"))
		}
	})
}

func endpointLabel(p string) string {
	switch {
	case p == "/healthz":
		return "healthz"
	case p == "/metrics":
		return "metrics"
	case p == "/api/v1/list-tempo-resources":
		return "list-instances"
	case strings.Contains(p, "/api/search/tag/") || strings.Contains(p, "/api/v2/search/tag/"):
		return "tag-values"
	case strings.HasSuffix(p, "/search/tags"):
		return "tags"
	case strings.HasSuffix(p, "/api/search"):
		return "search"
	case strings.Contains(p, "/traces/"):
		return "trace"
	}
	return "other"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"status": "error", "error": msg})
}

// listInstances answers the plugin's TempoStack discovery with the views the
// viewer can read anything from.
func (s *server) listInstances(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token (is the plugin proxy configured with authorize: true?)")
		return
	}
	views, err := s.fed.visibleViews(r.Context(), &viewer{token: tok})
	if errors.Is(err, errUnauthenticated) {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	type instance struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	data := []instance{}
	for _, v := range views {
		data = append(data, instance{Kind: "TempoMonolithic", Namespace: s.cfg.InstanceNamespace, Name: v})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": data})
}

func params(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func unixParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (s *server) tempoAPI(w http.ResponseWriter, r *http.Request) {
	view, ok := s.fed.views[r.PathValue("view")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown trace store view "+r.PathValue("view"))
		return
	}
	tok := bearer(r)
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	v := &viewer{token: tok}
	rest := "/" + r.PathValue("rest")
	p := params(r)
	start, end := unixParam(p["start"]), unixParam(p["end"])

	var statuses []StoreStatus
	var err error
	defer func() {
		if errors.Is(err, errUnauthenticated) {
			return
		}
		if partialMessage(statuses) != "" {
			metricPartial.inc(endpointLabel(rest))
		}
	}()
	setStatus := func() { w.Header().Set("X-Federation-Status", statusHeader(statuses)) }
	fail := func() bool {
		if errors.Is(err, errUnauthenticated) {
			writeError(w, http.StatusUnauthorized, err.Error())
			return true
		}
		if allFailed(statuses) {
			setStatus()
			writeError(w, http.StatusBadGateway, "no trace store answered: "+partialMessage(statuses))
			return true
		}
		return false
	}

	switch {
	case rest == "/api/echo":
		_, _ = w.Write([]byte("echo"))

	case rest == "/api/search":
		q := p["q"]
		if strings.TrimSpace(q) == "" {
			q = "{}"
		}
		limit, _ := strconv.Atoi(p["limit"])
		if limit <= 0 {
			limit = 20
		}
		limit = min(limit, 1000)
		slices, _ := strconv.Atoi(p["slices"])
		req := SearchRequest{Raw: q, Start: start, End: end, Limit: limit, Slices: min(max(slices, 1), maxSlices), Params: p}
		req.Params["q"] = q
		req.Query, req.ParseErr = ParseTraceQL(q)
		var traces []TraceSummary
		traces, statuses, err = s.fed.search(r.Context(), v, view, req)
		if fail() {
			return
		}
		setStatus()
		if traces == nil {
			traces = []TraceSummary{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"traces": traces})

	case strings.HasPrefix(rest, "/api/v2/traces/") || strings.HasPrefix(rest, "/api/traces/"):
		raw := rest[strings.LastIndex(rest, "/")+1:]
		id := normalizeID(raw, 32)
		if len(id) != 32 || !isHex(id) {
			writeError(w, http.StatusBadRequest, "invalid trace ID")
			return
		}
		var rs []map[string]any
		rs, statuses, err = s.fed.trace(r.Context(), v, view, id)
		if fail() {
			return
		}
		setStatus()
		msg := partialMessage(statuses)
		if strings.HasPrefix(rest, "/api/traces/") { // Tempo v1 shape
			if len(rs) == 0 {
				writeError(w, http.StatusNotFound, "trace not found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"batches": rs})
			return
		}
		trace := map[string]any{}
		if len(rs) > 0 {
			trace["resourceSpans"] = rs
		}
		body := map[string]any{"trace": trace}
		if msg != "" {
			body["message"] = msg // shown by Perses as a warning notice
		}
		writeJSON(w, http.StatusOK, body)

	case rest == "/api/v2/search/tags" || rest == "/api/search/tags":
		var ts *TagSet
		ts, statuses, err = s.fed.tags(r.Context(), v, view, TagsRequest{Start: start, End: end, Params: p})
		if fail() {
			return
		}
		setStatus()
		if rest == "/api/search/tags" {
			all := sortedUnique(append(append(append([]string{}, ts.Resource...), ts.Span...), ts.Intrinsic...))
			writeJSON(w, http.StatusOK, map[string]any{"tagNames": all})
			return
		}
		scopes := []map[string]any{}
		want := p["scope"]
		for _, sc := range []struct {
			name string
			tags []string
		}{{"resource", ts.Resource}, {"span", ts.Span}, {"intrinsic", ts.Intrinsic}} {
			if want != "" && want != "all" && want != sc.name {
				continue
			}
			tags := sc.tags
			if tags == nil {
				tags = []string{}
			}
			scopes = append(scopes, map[string]any{"name": sc.name, "tags": tags})
		}
		writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes})

	case strings.HasPrefix(rest, "/api/v2/search/tag/") || strings.HasPrefix(rest, "/api/search/tag/"):
		tag := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(rest, "/api/v2/search/tag/"), "/api/search/tag/"), "/values")
		attr, perr := parseAttr(tag)
		if perr != nil {
			attr = Attr{Scope: ScopeAny, Name: tag}
		}
		var vals []TagValue
		vals, statuses, err = s.fed.tagValues(r.Context(), v, view, TagValuesRequest{Tag: attr, RawTag: tag, Q: p["q"], Start: start, End: end, Params: p})
		if fail() {
			return
		}
		setStatus()
		if vals == nil {
			vals = []TagValue{}
		}
		if strings.HasPrefix(rest, "/api/search/tag/") {
			plain := make([]string, len(vals))
			for i, tv := range vals {
				plain[i] = tv.Value
			}
			writeJSON(w, http.StatusOK, map[string]any{"tagValues": plain})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tagValues": vals})

	default:
		writeError(w, http.StatusNotFound, "unsupported Tempo API path "+rest)
	}
}

// allFailed: at least one store was asked and none answered.
func allFailed(statuses []StoreStatus) bool {
	asked, ok := 0, 0
	for _, s := range statuses {
		switch s.Status {
		case "ok":
			ok++
			asked++
		case "error", "timeout", "unsupported":
			asked++
		}
	}
	return asked > 0 && ok == 0
}
