package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func span(id, parent, name string) map[string]any {
	s := map[string]any{"traceId": "7a1b2c3d4e5f60718293a4b5c6d7e8f9", "spanId": id, "name": name}
	if parent != "" {
		s["parentSpanId"] = parent
	}
	return s
}

func rs(svc string, spans ...map[string]any) map[string]any {
	var list []any
	for _, s := range spans {
		list = append(list, s)
	}
	return resourceSpans([]any{kvString("service.name", svc)}, list)
}

func TestMergeTraceDedupesByPriority(t *testing.T) {
	// The hot store returns base64 IDs (as VictoriaTraces does), the sampled
	// store hex; span ...09 is in both and must be kept once, from the first.
	hot := []map[string]any{rs("web",
		span("EAAAAAAAAAE=", "", "GET /fiction"),
		span("EAAAAAAAAAk=", "EAAAAAAAAAE=", "cache.get"))}
	sampled := []map[string]any{rs("api",
		span("1000000000000003", "1000000000000001", "POST /rank"),
		span("1000000000000009", "1000000000000001", "cache.get (dup)"))}
	out := mergeTrace([]string{"victoria", "signoz"}, [][]map[string]any{hot, sampled})
	var names []string
	stores := map[string]string{}
	for _, r := range out {
		var store string
		for _, a := range r["resource"].(map[string]any)["attributes"].([]any) {
			if a.(map[string]any)["key"] == "federation.store" {
				store = a.(map[string]any)["value"].(map[string]any)["stringValue"].(string)
			}
		}
		for _, ss := range r["scopeSpans"].([]any) {
			for _, sp := range ss.(map[string]any)["spans"].([]any) {
				m := sp.(map[string]any)
				names = append(names, m["name"].(string))
				stores[m["spanId"].(string)] = store
			}
		}
	}
	if strings.Join(names, ",") != "GET /fiction,cache.get,POST /rank" {
		t.Errorf("spans = %v", names)
	}
	if stores["1000000000000009"] != "victoria" || stores["1000000000000003"] != "signoz" {
		t.Errorf("stores = %v", stores)
	}
}

func TestMergeSearch(t *testing.T) {
	a := []TraceSummary{
		{TraceID: "abc", RootServiceName: rootNotReceived, StartTimeUnixNano: "2000", DurationMs: 5},
		{TraceID: "def", RootServiceName: "web", StartTimeUnixNano: "1000", DurationMs: 1},
	}
	b := []TraceSummary{
		{TraceID: "00000000000000000000000000000abc", RootServiceName: "api", RootTraceName: "GET /", StartTimeUnixNano: "1500", DurationMs: 9},
		{TraceID: "fff", RootServiceName: "x", StartTimeUnixNano: "3000"},
	}
	out := mergeSearch([]string{"hot", "cold"}, [][]TraceSummary{a, b}, 2)
	if len(out) != 2 || out[0].TraceID != normalizeID("fff", 32) {
		t.Fatalf("order/limit: %+v", out)
	}
	m := out[1]
	if m.RootServiceName != "api" || m.StartTimeUnixNano != "1500" || m.DurationMs != 9 || strings.Join(m.Stores, ",") != "hot,cold" {
		t.Errorf("merged = %+v", m)
	}
}

func TestStatusHeaderIsASCIIJSON(t *testing.T) {
	h := statusHeader([]StoreStatus{{Store: "s", Status: "error", Message: "bad \"thing\" – é"}})
	var out []StoreStatus
	if err := json.Unmarshal([]byte(h), &out); err != nil || out[0].Message != "bad \"thing\" – é" {
		t.Fatalf("%s: %v %+v", h, err, out)
	}
	for _, r := range h {
		if r > 0x7e {
			t.Fatalf("non-ASCII in header: %q", h)
		}
	}
}

// fakeKube is a Kubernetes API that answers SelfSubjectAccessReviews from a
// table keyed by token and lists SentryProjectBindings.
type fakeKube struct {
	allow    map[string]map[string]bool // token -> ResourceAttributes.key() -> allowed
	bindings []SentryProjectBinding
}

func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		perms, ok := k.allow[tok]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Spec struct {
				ResourceAttributes ResourceAttributes `json:"resourceAttributes"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		allowed := perms[body.Spec.ResourceAttributes.key()] || perms["*"]
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": allowed}})
	case bindingsPath:
		if tok != "sa" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": k.bindings})
	default:
		http.NotFound(w, r)
	}
}

func binding(name, project string, envs []string, targets ...BindingTarget) SentryProjectBinding {
	var b SentryProjectBinding
	b.Metadata.Name = name
	b.Spec.Sentry.Organization = "org"
	b.Spec.Sentry.Project = project
	b.Spec.Sentry.Environments = envs
	b.Spec.Targets = targets
	return b
}

// fakeSentry serves the trace endpoint with spans from three projects and
// records the traces/ search requests.
type fakeSentry struct {
	mu       sync.Mutex
	searches []string
}

func (s *fakeSentry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/0/organizations/org/projects/":
		_, _ = io.WriteString(w, `[{"id":"1","slug":"web"},{"id":"2","slug":"api"},{"id":"3","slug":"secret"}]`)
	case strings.HasPrefix(r.URL.Path, "/api/0/organizations/org/trace/"):
		_, _ = io.WriteString(w, `[{"event_id":"1000000000000001","parent_span_id":null,"project_slug":"web","project_id":1,"op":"http.server","transaction":"GET /","is_transaction":true,"start_timestamp":1.5,"end_timestamp":2.0,
		  "additional_attributes":{"environment":"production","span.status":"internal_error","user.id":"must-not-leak"},"errors":[{"issue_id":42,"event_id":"e1"}],
		  "children":[
		    {"event_id":"1000000000000002","parent_span_id":"1000000000000001","project_slug":"api","project_id":2,"op":"db.query","description":"SELECT 1","start_timestamp":1.6,"end_timestamp":1.7,"additional_attributes":{"environment":"staging"},"children":[]},
		    {"event_id":"1000000000000003","parent_span_id":"1000000000000001","project_slug":"secret","project_id":3,"op":"http.client","description":"GET secret","start_timestamp":1.6,"end_timestamp":1.8,"additional_attributes":{"environment":"production"},"children":[]}
		  ]}]`)
	case r.URL.Path == "/api/0/organizations/org/traces/":
		s.mu.Lock()
		s.searches = append(s.searches, r.URL.RawQuery)
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"data":[{"trace":"7a1b2c3d4e5f60718293a4b5c6d7e8f9","name":"GET /secret","project":"secret","start":1700000000000,"end":1700000000100,"duration":100,"numSpans":3,"numErrors":0,"breakdowns":[{"project":"secret"},{"project":"web"}]}]}`)
	default:
		http.NotFound(w, r)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestServer(t *testing.T, kube *fakeKube, sentry *fakeSentry, tempoURL string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	kubeSrv := httptest.NewServer(kube)
	t.Cleanup(kubeSrv.Close)
	sentrySrv := httptest.NewServer(sentry)
	t.Cleanup(sentrySrv.Close)
	cfg := map[string]any{
		"kubernetes": map[string]any{"apiServer": kubeSrv.URL, "tokenFile": writeFile(t, dir, "sa", "sa")},
		"backends": []any{
			map[string]any{"name": "victoria", "type": "tempo", "url": tempoURL, "priority": 100, "maxLookback": "2d"},
			map[string]any{"name": "sentry", "type": "sentry", "url": sentrySrv.URL, "organization": "org",
				"tokenFile": writeFile(t, dir, "st", "sentry-token"), "maxLookback": "90d", "priority": 10,
				"attributeAllowlist": []string{"http.response_status_code"}},
		},
	}
	b, _ := json.Marshal(cfg)
	c, err := loadConfig(writeFile(t, dir, "config.json", string(b)))
	if err != nil {
		t.Fatal(err)
	}
	kc, err := newKubeClient(c.Kubernetes)
	if err != nil {
		t.Fatal(err)
	}
	fed, err := newFederator(c, kc)
	if err != nil {
		t.Fatal(err)
	}
	if err := fed.authz.bindings.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&server{fed: fed, cfg: c}).routes())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string, out any) (int, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, resp.Header
}

func spanNames(t *testing.T, body map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	trace, _ := body["trace"].(map[string]any)
	rss, _ := trace["resourceSpans"].([]any)
	for _, r := range rss {
		store := ""
		for _, a := range r.(map[string]any)["resource"].(map[string]any)["attributes"].([]any) {
			if a.(map[string]any)["key"] == "federation.store" {
				store = a.(map[string]any)["value"].(map[string]any)["stringValue"].(string)
			}
		}
		for _, ss := range r.(map[string]any)["scopeSpans"].([]any) {
			for _, sp := range ss.(map[string]any)["spans"].([]any) {
				out[sp.(map[string]any)["name"].(string)] = store
			}
		}
	}
	return out
}

func TestSentryVisibilityFollowsBindings(t *testing.T) {
	// Tempo store: one span of the same trace, plus a dead store to check
	// partial results.
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"trace":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"web"}}]},"scopeSpans":[{"spans":[{"traceId":"ehssPU5fYHGCk6S1xtfo+Q==","spanId":"EAAAAAAAAAE=","name":"GET / (otlp)"}]}]}]}}`)
	}))
	defer tempo.Close()

	kube := &fakeKube{
		allow: map[string]map[string]bool{
			"admin": {"*": true},
			// dev may read the "web" Deployment and pods in "api-ns", and
			// the tempo store.
			"dev": {
				"get apps/deployments web-ns/web":            true,
				"get /pods api-ns/":                          true,
				"get tracing.royalroad.com/traces /victoria": true,
			},
			"nobody": {},
		},
		bindings: []SentryProjectBinding{
			binding("web", "web", nil, BindingTarget{Namespace: "web-ns", Kind: "Deployment", Name: "web"}),
			binding("api", "api", []string{"production"}, BindingTarget{Namespace: "api-ns"}),
			binding("secret", "secret", nil, BindingTarget{Namespace: "secret-ns", Kind: "StatefulSet", Name: "db"}),
		},
	}
	sentry := &fakeSentry{}
	srv := newTestServer(t, kube, sentry, tempo.URL)
	traceURL := srv.URL + "/proxy/tracing/all-stores/single-tenant/api/v2/traces/7a1b2c3d4e5f60718293a4b5c6d7e8f9"

	var body map[string]any
	code, _ := get(t, traceURL, "admin", &body)
	got := spanNames(t, body)
	// The root span is in both stores: the tempo copy wins (priority).
	// Admins see every bound project, within the bound environments: the
	// api span is from staging and api is bound for production only.
	if code != 200 || got["GET / (otlp)"] != "victoria" || got["GET secret"] != "sentry" || len(got) != 2 {
		t.Errorf("admin: %d %v", code, got)
	}

	body = nil
	code, _ = get(t, traceURL, "dev", &body)
	got = spanNames(t, body)
	// dev: web (bound via Deployment) but not "secret"; api is bound for
	// production only and the api span is staging.
	if code != 200 || len(got) != 1 || got["GET / (otlp)"] != "victoria" {
		t.Errorf("dev: %d %v", code, got)
	}
	if strings.Contains(mustJSON(body), "must-not-leak") {
		t.Error("a non-allowlisted attribute reached the response")
	}

	// Search: the root project "secret" is not visible to dev, so the row
	// must not show its name.
	var search struct {
		Traces []TraceSummary `json:"traces"`
	}
	now := time.Now().Unix()
	sURL := srv.URL + "/proxy/tracing/sentry/single-tenant/api/search?q=%7B%7D&start=" + strconv.FormatInt(now-3600, 10) + "&end=" + strconv.FormatInt(now, 10) + "&limit=20"
	code, _ = get(t, sURL, "dev", &search)
	if code != 200 || len(search.Traces) != 1 || search.Traces[0].RootTraceName != "" || search.Traces[0].RootServiceName != rootNotReceived {
		t.Errorf("dev search: %d %+v", code, search.Traces)
	}
	sentry.mu.Lock()
	queries := strings.Join(sentry.searches, " | ")
	sentry.mu.Unlock()
	if strings.Contains(queries, "project=3") || !strings.Contains(queries, "project=1") || !strings.Contains(queries, "environment=production") {
		t.Errorf("sentry requests not scoped: %s", queries)
	}

	// nobody: no store readable; listing shows nothing.
	var list struct {
		Data []map[string]string `json:"data"`
	}
	code, _ = get(t, srv.URL+"/api/v1/list-tempo-resources", "nobody", &list)
	if code != 200 || len(list.Data) != 0 {
		t.Errorf("nobody list: %d %+v", code, list)
	}
	code, _ = get(t, srv.URL+"/api/v1/list-tempo-resources", "dev", &list)
	if code != 200 || len(list.Data) != 3 {
		t.Errorf("dev list: %d %+v", code, list)
	}
	// Unknown token: 401, missing token: 401.
	if code, _ := get(t, traceURL, "forged", nil); code != 401 {
		t.Errorf("forged token: %d", code)
	}
	if code, _ := get(t, traceURL, "", nil); code != 401 {
		t.Errorf("no token: %d", code)
	}
}

func TestPartialResults(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()
	kube := &fakeKube{
		allow:    map[string]map[string]bool{"admin": {"*": true}},
		bindings: []SentryProjectBinding{binding("web", "web", nil, BindingTarget{Namespace: "web-ns"})},
	}
	srv := newTestServer(t, kube, &fakeSentry{}, slow.URL)
	var body map[string]any
	code, hdr := get(t, srv.URL+"/proxy/tracing/all-stores/x/api/search?q=%7B%7D%20%3E%3E%20%7B%7D&limit=5", "admin", &body)
	// The structural query is unsupported for Sentry and reported as such;
	// the Tempo store gets it unchanged (and answers garbage here).
	var statuses []StoreStatus
	_ = json.Unmarshal([]byte(hdr.Get("X-Federation-Status")), &statuses)
	found := false
	for _, s := range statuses {
		if s.Store == "sentry" && s.Status == "unsupported" {
			found = true
		}
	}
	if !found {
		t.Errorf("code %d statuses %+v", code, statuses)
	}
}

func TestTimeout(t *testing.T) {
	s := &store{Backend: slowBackend{}, cfg: BackendConfig{Name: "slow", Access: "rbac", SearchTimeout: Duration{50 * time.Millisecond}}}
	f := &federator{stores: []*store{s}, authz: &authorizer{ttl: time.Minute, cache: map[string]cacheEntry{}}}
	f.authz.cache[tokenHash("t")+" "+ResourceAttributes{Verb: "get", Resource: "traces", Name: "slow"}.key()] = cacheEntry{allowed: true, at: time.Now()}
	f.authz.resource = "traces"
	_, statuses, err := f.search(context.Background(), &viewer{token: "t"}, f.stores, SearchRequest{Raw: "{}", Limit: 5})
	if err != nil || statuses[0].Status != "timeout" {
		t.Errorf("%v %+v", err, statuses)
	}
}

type slowBackend struct{ Backend }

func (slowBackend) Search(ctx context.Context, _ *SearchRequest) ([]TraceSummary, error) {
	<-ctx.Done()
	return nil, errors.New("cancelled")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
