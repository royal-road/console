package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePyroscope records the requests it gets and answers from handle, or
// with a small flame graph.
type fakePyroscope struct {
	mu     sync.Mutex
	calls  []pyroCall
	handle func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

type pyroCall struct {
	Path, ContentType, Authorization string
	Body                             map[string]any
}

func (f *fakePyroscope) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	f.mu.Lock()
	f.calls = append(f.calls, pyroCall{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), parsed})
	h := f.handle
	f.mu.Unlock()
	if h != nil && h(w, r, body) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"flamegraph":{"names":["total","main"],"levels":[{"values":["0","100","0","0"]},{"values":["0","100","100","1"]}],"total":"100","maxSelf":"100"}}`)
}

func (f *fakePyroscope) last(t *testing.T) pyroCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("Pyroscope was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakePyroscope) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Tokens: "viewer" may get pods in royal-road, "outsider" in other-ns only,
// anything else is rejected by the API server.
func profilesKube() *fakeKube {
	return &fakeKube{allow: map[string]map[string]bool{
		"viewer":   {"get /pods royal-road/": true},
		"outsider": {"get /pods other-ns/": true},
	}}
}

func newProfilesServer(t *testing.T, kube http.Handler, pyroURL string, profiles map[string]any) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	kubeSrv := httptest.NewServer(kube)
	t.Cleanup(kubeSrv.Close)
	cfg := map[string]any{
		"kubernetes": map[string]any{"apiServer": kubeSrv.URL, "tokenFile": writeFile(t, dir, "sa", "sa")},
		"backends":   []any{map[string]any{"name": "victoria", "type": "tempo", "url": "http://127.0.0.1:1"}},
	}
	if profiles != nil {
		if _, ok := profiles["url"]; !ok {
			profiles["url"] = pyroURL
		}
		cfg["profiles"] = profiles
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
	srv := httptest.NewServer(logRequests((&server{fed: fed, cfg: c}).routes()))
	t.Cleanup(srv.Close)
	return srv
}

func defaultProfiles() map[string]any {
	return map[string]any{"legacyServices": map[string]any{"royal-road": []string{"royalroad.website"}}}
}

type profResp struct {
	Code   int
	Header http.Header
	Body   []byte
}

func (r profResp) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	return out
}

func doProfiles(t *testing.T, method, url, token string, body any, header ...string) profResp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, url, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	// No transparent gzip: tests look at the encoding.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return profResp{resp.StatusCode, resp.Header, data}
}

var hourEnd = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).UnixMilli()

func mergeBody(selector string) map[string]any {
	return map[string]any{
		"profileTypeID": "process_cpu:cpu:nanoseconds:cpu:nanoseconds",
		"labelSelector": selector,
		"start":         hourEnd - time.Hour.Milliseconds(),
		"end":           hourEnd,
	}
}

func with(m map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(out, kv[i].(string))
			continue
		}
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestProfilesAuthorization(t *testing.T) {
	pyro := &fakePyroscope{}
	pyroSrv := httptest.NewServer(pyro)
	defer pyroSrv.Close()
	srv := newProfilesServer(t, profilesKube(), pyroSrv.URL, defaultProfiles())
	sel := `{namespace="royal-road"}`

	cases := []struct {
		name, method, path, token string
		body                      any
		code                      int
		connect                   string
	}{
		{"scopes, allowed", "GET", "/profiles/royal-road/scopes", "viewer", nil, 200, ""},
		{"query, allowed", "POST", "/profiles/royal-road/SelectMergeStacktraces", "viewer", mergeBody(sel), 200, ""},
		{"scopes, other namespace", "GET", "/profiles/royal-road/scopes", "outsider", nil, 403, "permission_denied"},
		{"query, other namespace", "POST", "/profiles/royal-road/SelectMergeStacktraces", "outsider", mergeBody(sel), 403, "permission_denied"},
		{"outsider in own namespace with royal-road selector", "POST", "/profiles/other-ns/SelectMergeStacktraces", "outsider", mergeBody(sel), 400, "invalid_argument"},
		{"no token", "POST", "/profiles/royal-road/SelectMergeStacktraces", "", mergeBody(sel), 401, "unauthenticated"},
		{"forged token", "GET", "/profiles/royal-road/scopes", "forged", nil, 401, "unauthenticated"},
		{"bad namespace", "GET", "/profiles/Royal_Road/scopes", "viewer", nil, 400, "invalid_argument"},
		{"unknown method", "POST", "/profiles/royal-road/ProfileTypes", "viewer", map[string]any{}, 404, "not_found"},
		{"unknown method before authz", "POST", "/profiles/royal-road/SelectMergeSpanProfile", "", map[string]any{}, 404, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := pyro.count()
			r := doProfiles(t, tc.method, srv.URL+tc.path, tc.token, tc.body)
			if r.Code != tc.code {
				t.Fatalf("code %d, want %d: %s", r.Code, tc.code, r.Body)
			}
			if tc.connect != "" {
				if got := r.json(t)["code"]; got != tc.connect {
					t.Errorf("connect code %v, want %s", got, tc.connect)
				}
				if pyro.count() != before {
					t.Error("Pyroscope was called for a refused request")
				}
			}
		})
	}

	r := doProfiles(t, "GET", srv.URL+"/profiles/royal-road/scopes", "viewer", nil)
	if got := string(r.Body); strings.TrimSpace(got) != `{"namespace":"royal-road","labelled":"{namespace=\"royal-road\"}","legacy":[{"service":"royalroad.website","selector":"{service_name=\"royalroad.website\"}"}]}` {
		t.Errorf("scopes = %s", got)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Error("scopes is cacheable")
	}
	r = doProfiles(t, "GET", srv.URL+"/profiles/other-ns/scopes", "outsider", nil)
	if got := r.json(t)["legacy"]; fmt.Sprint(got) != "[]" {
		t.Errorf("other-ns legacy = %v", got)
	}
	if r := doProfiles(t, "GET", srv.URL+"/profiles/royal-road/SelectSeries", "viewer", nil); r.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a method: %d", r.Code)
	}
}

func TestProfilesAccessReviewFailure(t *testing.T) {
	kube := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "etcd down", 500) })
	srv := newProfilesServer(t, kube, "http://127.0.0.1:1", defaultProfiles())
	r := doProfiles(t, "GET", srv.URL+"/profiles/royal-road/scopes", "viewer", nil)
	if r.Code != http.StatusBadGateway || r.json(t)["code"] != "unavailable" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestProfilesNotConfigured(t *testing.T) {
	srv := newProfilesServer(t, profilesKube(), "", nil)
	for _, path := range []string{"/profiles/royal-road/scopes", "/profiles/royal-road/SelectSeries"} {
		method := "GET"
		var body any
		if !strings.HasSuffix(path, "scopes") {
			method, body = "POST", mergeBody(`{namespace="royal-road"}`)
		}
		r := doProfiles(t, method, srv.URL+path, "viewer", body)
		if r.Code != 404 || r.json(t)["code"] != "not_found" {
			t.Errorf("%s: %d %s", path, r.Code, r.Body)
		}
	}
}

// The selector rule is the security boundary: every request must stay in
// its namespace whatever the caller writes.
func TestProfilesSelectorRule(t *testing.T) {
	pyro := &fakePyroscope{}
	pyroSrv := httptest.NewServer(pyro)
	defer pyroSrv.Close()
	srv := newProfilesServer(t, profilesKube(), pyroSrv.URL, defaultProfiles())
	url := srv.URL + "/profiles/royal-road/SelectMergeStacktraces"

	allowed := []struct{ in, out string }{
		{`{namespace="royal-road"}`, `{namespace="royal-road"}`},
		{` { namespace = "royal-road" , k8s_deployment_name="royalroad" } `, `{namespace="royal-road",k8s_deployment_name="royalroad"}`},
		{`{namespace="royal-road",service_name="anything",pod=~".*"}`, `{namespace="royal-road",service_name="anything",pod=~".*"}`},
		{`{namespace="royal-road"}`, `{namespace="royal-road"}`},
		{`{service_name="royalroad.website"}`, `{service_name="royalroad.website",namespace=~"|royal-road"}`},
		{`{service_name="royalroad.website",pod=~"royalroad-(7d9f|5c4b)-[a-z0-9]{5}"}`,
			`{service_name="royalroad.website",pod=~"royalroad-(7d9f|5c4b)-[a-z0-9]{5}",namespace=~"|royal-road"}`},
	}
	for _, tc := range allowed {
		r := doProfiles(t, "POST", url, "viewer", mergeBody(tc.in))
		if r.Code != 200 {
			t.Errorf("%s: %d %s", tc.in, r.Code, r.Body)
			continue
		}
		if got := pyro.last(t).Body["labelSelector"]; got != tc.out {
			t.Errorf("%s: forwarded %v, want %s", tc.in, got, tc.out)
		}
	}

	rejected := []string{
		``,
		`{}`,
		`{namespace="other-ns"}`,
		`{namespace=~"royal-road|other-ns"}`,
		`{namespace=~".*"}`,
		`{namespace!="royal-road"}`,
		`{namespace="royal-road",namespace="other-ns"}`,
		`{namespace="royal-road",namespace!="other-ns"}`,
		`{namespace="royal-road\",namespace=\"other-ns"}`,
		`{namespace='royal-road'}`,
		"{namespace=`royal-road`}",
		`{"namespace"="royal-road"}`,
		`{namespace="royal-road",}`,
		`{namespace="royal-road"`,
		`{namespace="royal-road"} or {namespace="other-ns"}`,
		`{namespace="royal-road"}{namespace="other-ns"}`,
		`process_cpu{namespace="royal-road"}`,
		`{service_name="other.service"}`,
		`{service_name=~"royalroad.website|other.service"}`,
		`{service_name=~"royalroad\\.website"}`,
		`{service_name="royalroad.website",service_name="other.service"}`,
		`{service_name="royalroad.website",namespace="other-ns"}`,
		`{service_name="royalroad.website",namespace=""}`,
		`{service_name="royalroad.website",namespace=~"|other-ns"}`,
		`{pod=~"royalroad-.*"}`,
	}
	before := pyro.count()
	for _, sel := range rejected {
		r := doProfiles(t, "POST", url, "viewer", mergeBody(sel))
		if r.Code != 400 || r.json(t)["code"] != "invalid_argument" {
			t.Errorf("%s: %d %s", sel, r.Code, r.Body)
		}
	}
	// The legacy list is per namespace.
	r := doProfiles(t, "POST", srv.URL+"/profiles/other-ns/SelectMergeStacktraces", "outsider", mergeBody(`{service_name="royalroad.website"}`))
	if r.Code != 400 {
		t.Errorf("legacy service from another namespace: %d %s", r.Code, r.Body)
	}
	// Every selector field is checked: each side of a Diff, and matchers.
	good, bad := mergeBody(`{namespace="royal-road"}`), mergeBody(`{namespace="other-ns"}`)
	for name, body := range map[string]map[string]any{
		"Diff right": {"left": good, "right": bad},
		"Diff left":  {"left": bad, "right": good},
	} {
		if r := doProfiles(t, "POST", srv.URL+"/profiles/royal-road/Diff", "viewer", body); r.Code != 400 {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	for _, m := range []string{"Series", "LabelNames", "LabelValues"} {
		body := map[string]any{"name": "pod", "matchers": []string{`{namespace="other-ns"}`}, "start": hourEnd - 1000, "end": hourEnd}
		if m != "LabelValues" {
			delete(body, "name")
		}
		if r := doProfiles(t, "POST", srv.URL+"/profiles/royal-road/"+m, "viewer", body); r.Code != 400 {
			t.Errorf("%s: %d %s", m, r.Code, r.Body)
		}
	}
	if got := pyro.count(); got != before {
		t.Errorf("Pyroscope got %d rejected requests", got-before)
	}

	// Diff and metadata selectors are rewritten like the others.
	r = doProfiles(t, "POST", srv.URL+"/profiles/royal-road/Diff", "viewer", map[string]any{"left": mergeBody(`{service_name="royalroad.website"}`), "right": good})
	if r.Code != 200 {
		t.Fatalf("Diff: %d %s", r.Code, r.Body)
	}
	if got := pyro.last(t).Body["left"].(map[string]any)["labelSelector"]; got != `{service_name="royalroad.website",namespace=~"|royal-road"}` {
		t.Errorf("Diff left forwarded %v", got)
	}
	r = doProfiles(t, "POST", srv.URL+"/profiles/royal-road/Series", "viewer", map[string]any{
		"matchers": []string{`{service_name="royalroad.website"}`}, "labelNames": []string{"pod", "__profile_type__"},
		"start": hourEnd - 24*time.Hour.Milliseconds(), "end": hourEnd})
	if r.Code != 200 {
		t.Fatalf("Series: %d %s", r.Code, r.Body)
	}
	if got := fmt.Sprint(pyro.last(t).Body["matchers"]); got != `[{service_name="royalroad.website",namespace=~"|royal-road"}]` {
		t.Errorf("Series forwarded %s", got)
	}
}

func TestProfilesRequestLimits(t *testing.T) {
	pyro := &fakePyroscope{}
	pyroSrv := httptest.NewServer(pyro)
	defer pyroSrv.Close()
	srv := newProfilesServer(t, profilesKube(), pyroSrv.URL, defaultProfiles())
	base := mergeBody(`{namespace="royal-road"}`)
	day := 24 * time.Hour.Milliseconds()
	seriesBase := with(base, "step", 15)
	meta := map[string]any{"matchers": []string{`{namespace="royal-road"}`}, "start": hourEnd - day, "end": hourEnd}

	cases := []struct {
		name, method string
		body         any
		code         int
		check        func(t *testing.T, sent map[string]any)
	}{
		{"maxNodes missing becomes 8192", "SelectMergeStacktraces", base, 200, wantField("maxNodes", 8192.0)},
		{"maxNodes 0 becomes 8192", "SelectMergeStacktraces", with(base, "maxNodes", 0), 200, wantField("maxNodes", 8192.0)},
		{"maxNodes clamped", "SelectMergeStacktraces", with(base, "maxNodes", 100000), 200, wantField("maxNodes", 16384.0)},
		{"maxNodes 1 kept", "SelectMergeStacktraces", with(base, "maxNodes", "1"), 200, wantField("maxNodes", 1.0)},
		{"maxNodes negative", "SelectMergeStacktraces", with(base, "maxNodes", -1), 400, nil},
		{"int64 strings", "SelectMergeStacktraces", with(base, "start", fmt.Sprint(hourEnd-1000), "end", fmt.Sprint(hourEnd)), 200, wantField("start", float64(hourEnd-1000))},
		{"window 24h", "SelectMergeStacktraces", with(base, "start", hourEnd-day), 200, nil},
		{"window over 24h", "SelectMergeStacktraces", with(base, "start", hourEnd-day-1), 400, nil},
		{"end before start", "SelectMergeStacktraces", with(base, "start", hourEnd+1), 400, nil},
		{"no start", "SelectMergeStacktraces", with(base, "start", nil), 400, nil},
		{"no profile type", "SelectMergeStacktraces", with(base, "profileTypeID", nil), 400, nil},
		{"callSite", "SelectMergeStacktraces", with(base, "stackTraceSelector", map[string]any{"callSite": []any{map[string]any{"name": "main"}, map[string]any{"name": "Handle"}}}), 200,
			func(t *testing.T, sent map[string]any) {
				if got := fmt.Sprint(sent["stackTraceSelector"]); got != "map[callSite:[map[name:main] map[name:Handle]]]" {
					t.Errorf("stackTraceSelector = %s", got)
				}
			}},
		{"goPgo", "SelectMergeStacktraces", with(base, "stackTraceSelector", map[string]any{"goPgo": map[string]any{"keepLocations": 1}}), 400, nil},
		{"callSite extra field", "SelectMergeStacktraces", with(base, "stackTraceSelector", map[string]any{"callSite": []any{map[string]any{"name": "a", "line": 3}}}), 400, nil},
		{"unknown field", "SelectMergeStacktraces", with(base, "format", "PROFILE_FORMAT_TREE"), 400, nil},
		{"case-folded field", "SelectMergeStacktraces", with(base, "labelSelector", nil, "LabelSelector", `{namespace="other-ns"}`), 400, nil},
		{"proto field name", "SelectMergeStacktraces", with(base, "labelSelector", nil, "label_selector", `{namespace="other-ns"}`), 400, nil},
		{"not an object", "SelectMergeStacktraces", `[1]`, 400, nil},
		{"invalid JSON", "SelectMergeStacktraces", `{"start":`, 400, nil},
		{"Diff window per side", "Diff", map[string]any{"left": base, "right": with(base, "start", hourEnd-2*day)}, 400, nil},
		{"Diff side missing", "Diff", map[string]any{"left": base}, 400, nil},
		{"Diff clamps both sides", "Diff", map[string]any{"left": with(base, "maxNodes", 99999), "right": base}, 200,
			func(t *testing.T, sent map[string]any) {
				if l, r := sent["left"].(map[string]any)["maxNodes"], sent["right"].(map[string]any)["maxNodes"]; l != 16384.0 || r != 8192.0 {
					t.Errorf("maxNodes left %v right %v", l, r)
				}
			}},
		{"SelectSeries", "SelectSeries", with(seriesBase, "groupBy", []string{"pod"}, "aggregation", "TIME_SERIES_AGGREGATION_TYPE_SUM", "limit", 10), 200, wantField("step", 15.0)},
		{"SelectSeries stackTraceSelector", "SelectSeries", with(seriesBase, "stackTraceSelector", map[string]any{"callSite": []any{}}), 400, nil},
		{"SelectSeries no step", "SelectSeries", base, 400, nil},
		{"SelectSeries 2000 points", "SelectSeries", with(seriesBase, "step", 1, "start", hourEnd-2000_000), 200, nil},
		{"SelectSeries 2001 points", "SelectSeries", with(seriesBase, "step", 1, "start", hourEnd-2001_000), 400, nil},
		{"SelectSeries bad aggregation", "SelectSeries", with(seriesBase, "aggregation", "MAX"), 400, nil},
		{"SelectSeries bad groupBy", "SelectSeries", with(seriesBase, "groupBy", []string{"pod name"}), 400, nil},
		{"Series", "Series", with(meta, "labelNames", []string{"pod", "__profile_type__"}), 200, nil},
		{"Series over 24h", "Series", with(meta, "start", hourEnd-day-1), 400, nil},
		{"LabelValues over 24h", "LabelValues", with(meta, "name", "pod", "start", hourEnd-day-1), 400, nil},
		{"Series two matchers", "Series", with(meta, "matchers", []string{`{namespace="royal-road"}`, `{namespace="royal-road",pod="a"}`}), 400, nil},
		{"Series no matcher", "Series", with(meta, "matchers", []string{}), 400, nil},
		{"LabelNames name not allowed", "LabelNames", with(meta, "name", "pod"), 400, nil},
		{"LabelValues", "LabelValues", with(meta, "name", "pod"), 200, nil},
		{"LabelValues no name", "LabelValues", meta, 400, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := pyro.count()
			r := doProfiles(t, "POST", srv.URL+"/profiles/royal-road/"+tc.method, "viewer", tc.body)
			if r.Code != tc.code {
				t.Fatalf("code %d, want %d: %s", r.Code, tc.code, r.Body)
			}
			if tc.code != 200 {
				if pyro.count() != before {
					t.Error("Pyroscope was called")
				}
				if r.json(t)["code"] != "invalid_argument" {
					t.Errorf("body %s", r.Body)
				}
				return
			}
			if tc.check != nil {
				tc.check(t, pyro.last(t).Body)
			}
		})
	}

	url := srv.URL + "/profiles/royal-road/SelectMergeStacktraces"
	big := with(base, "profileTypeID", strings.Repeat("x", profilesMaxBody))
	if r := doProfiles(t, "POST", url, "viewer", big); r.Code != 413 || r.json(t)["code"] != "resource_exhausted" {
		t.Errorf("large body: %d %s", r.Code, r.Body)
	}
	if r := doProfiles(t, "POST", url, "viewer", base, "Content-Type", "text/plain"); r.Code != 415 || r.json(t)["code"] != "invalid_argument" {
		t.Errorf("text/plain: %d %s", r.Code, r.Body)
	}
	if r := doProfiles(t, "POST", url, "viewer", base, "Content-Type", "application/json; charset=utf-8"); r.Code != 200 {
		t.Errorf("json with charset: %d %s", r.Code, r.Body)
	}
}

func wantField(name string, want any) func(t *testing.T, sent map[string]any) {
	return func(t *testing.T, sent map[string]any) {
		t.Helper()
		if sent[name] != want {
			t.Errorf("%s = %v (%T), want %v", name, sent[name], sent[name], want)
		}
	}
}

func TestProfilesPassthrough(t *testing.T) {
	flame := `{"flamegraph":{"names":["total"` + strings.Repeat(`,"System.Threading.Tasks.Task.ExecuteWithThreadLocal"`, 2000) + `],"levels":[],"total":"12345678901234","maxSelf":"1"}}`
	pyro := &fakePyroscope{handle: func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, flame)
		return true
	}}
	pyroSrv := httptest.NewServer(pyro)
	defer pyroSrv.Close()
	srv := newProfilesServer(t, profilesKube(), pyroSrv.URL, defaultProfiles())
	url := srv.URL + "/profiles/royal-road/SelectMergeStacktraces"
	body := mergeBody(`{namespace="royal-road"}`)

	r := doProfiles(t, "POST", url, "viewer", body)
	if r.Code != 200 || string(r.Body) != flame {
		t.Fatalf("plain: %d, %d bytes", r.Code, len(r.Body))
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Content-Encoding") != "" {
		t.Errorf("headers %v", r.Header)
	}
	call := pyro.last(t)
	if call.Path != "/querier.v1.QuerierService/SelectMergeStacktraces" || call.ContentType != "application/json" {
		t.Errorf("upstream %s %s", call.Path, call.ContentType)
	}
	if call.Authorization != "" {
		t.Error("the viewer's token reached Pyroscope")
	}

	r = doProfiles(t, "POST", url, "viewer", body, "Accept-Encoding", "br, gzip")
	if r.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v", r.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.Body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != flame || len(r.Body) >= len(flame)/4 {
		t.Errorf("gzip: %d bytes for %d", len(r.Body), len(flame))
	}
	if r := doProfiles(t, "POST", url, "viewer", body, "Accept-Encoding", "gzip;q=0"); r.Header.Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 was gzipped")
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := string(data)
	for _, want := range []string{
		`tracefed_requests_total{endpoint="profiles-SelectMergeStacktraces",code="200"} `,
		`tracefed_backend_requests_total{store="pyroscope",outcome="ok"}`,
		`tracefed_backend_duration_seconds_count{store="pyroscope"}`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestProfilesUpstreamErrors(t *testing.T) {
	var mode string
	pyro := &fakePyroscope{handle: func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		switch mode {
		case "invalid":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"code":"invalid_argument","message":"invalid query: profile type not found"}`)
		case "5xx":
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"code":"unavailable","message":"no healthy querier"}`)
		case "html":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `<html>not here</html>`)
		case "slow":
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		default:
			return false
		}
		return true
	}}
	pyroSrv := httptest.NewServer(pyro)
	defer pyroSrv.Close()
	srv := newProfilesServer(t, profilesKube(), pyroSrv.URL, with(defaultProfiles(), "timeout", "200ms"))
	url := srv.URL + "/profiles/royal-road/SelectMergeStacktraces"
	body := mergeBody(`{namespace="royal-road"}`)

	for _, tc := range []struct {
		mode, code string
		status     int
		message    string
	}{
		{"invalid", "invalid_argument", 400, "invalid query: profile type not found"},
		{"5xx", "unavailable", 502, "Pyroscope answered HTTP 503: no healthy querier"},
		{"html", "unavailable", 502, "Pyroscope answered HTTP 404: <html>not here</html>"},
		{"slow", "deadline_exceeded", 504, "Pyroscope did not answer within 200ms"},
	} {
		mode = tc.mode
		r := doProfiles(t, "POST", url, "viewer", body)
		got := r.json(t)
		if r.Code != tc.status || got["code"] != tc.code || got["message"] != tc.message {
			t.Errorf("%s: %d %s", tc.mode, r.Code, r.Body)
		}
	}

	down := newProfilesServer(t, profilesKube(), "http://127.0.0.1:1", defaultProfiles())
	r := doProfiles(t, "POST", down.URL+"/profiles/royal-road/SelectMergeStacktraces", "viewer", body)
	if r.Code != 502 || r.json(t)["code"] != "unavailable" {
		t.Errorf("unreachable: %d %s", r.Code, r.Body)
	}
}

func TestParseLabelSelectorRoundTrip(t *testing.T) {
	for in, want := range map[string]string{
		`{a="1"}`: `{a="1"}`,
		`{ a = "1" , b!="2",c=~"x|y", d!~"\\d+"}`: `{a="1",b!="2",c=~"x|y",d!~"\\d+"}`,
		`{_x9="tab\there"}`:                       `{_x9="tab\there"}`,
		`{a="é"}`:                                 `{a="é"}`,
	} {
		sel, err := parseLabelSelector(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if sel.String() != want {
			t.Errorf("%s -> %s, want %s", in, sel, want)
		}
		again, err := parseLabelSelector(sel.String())
		if err != nil || again.String() != want {
			t.Errorf("%s does not round-trip: %v", want, err)
		}
	}
	for _, in := range []string{`{9a="1"}`, `{a=="1"}`, `{a="1" b="2"}`, `{a=1}`, `{a="\q"}`, `{a="1"}x`} {
		if _, err := parseLabelSelector(in); err == nil {
			t.Errorf("%s parsed", in)
		}
	}
}

func TestProfilesConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(profiles string) (*Config, error) {
		return loadConfig(writeFile(t, dir, "c.json", `{"backends":[{"name":"v","type":"tempo","url":"http://x"}],"profiles":`+profiles+`}`))
	}
	c, err := load(`{"url":"http://pyroscope:4040/"}`)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles
	if p.URL != "http://pyroscope:4040" || p.Timeout.Duration != 20*time.Second || p.MaxWindow.Duration != 24*time.Hour || p.MaxNodes != 16384 {
		t.Errorf("defaults %+v", p)
	}
	for _, bad := range []string{`{}`, `{"url":"http://x","legacyServices":{"Not A Namespace":["s"]}}`, `{"url":"http://x","legacyServices":{"ns":[""]}}`, `{"url":"http://x","maxNodes":-1}`, `{"url":"http://x","typo":1}`} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestEndpointLabelProfiles(t *testing.T) {
	for path, want := range map[string]string{
		"/profiles/royal-road/scopes":       "profiles-scopes",
		"/profiles/royal-road/Diff":         "profiles-Diff",
		"/profiles/royal-road/ProfileTypes": "profiles-other",
		"/profiles/royal-road/a/b/c":        "profiles-other",
	} {
		if got := endpointLabel(path); got != want {
			t.Errorf("%s: %s", path, got)
		}
	}
}
