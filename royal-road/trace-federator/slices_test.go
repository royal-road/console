package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSliceWindows(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	w := sliceWindows(start, start.Add(time.Hour), 12)
	if len(w) != 12 {
		t.Fatalf("got %d windows", len(w))
	}
	for i, win := range w {
		if got := win[1].Sub(win[0]); got != 5*time.Minute {
			t.Errorf("window %d is %v", i, got)
		}
		if i > 0 && !win[0].Equal(w[i-1][1]) {
			t.Errorf("window %d does not start where %d ends", i, i-1)
		}
	}
	if !w[0][0].Equal(start) || !w[11][1].Equal(start.Add(time.Hour)) {
		t.Errorf("windows do not cover the range: %v .. %v", w[0][0], w[11][1])
	}
	// Uneven splits still cover the range exactly.
	w = sliceWindows(start, start.Add(100*time.Second), 7)
	if len(w) != 7 || !w[6][1].Equal(start.Add(100*time.Second)) {
		t.Errorf("uneven: %v", w)
	}
	// Never more windows than seconds; nothing to split for 1 or an empty range.
	if w := sliceWindows(start, start.Add(3*time.Second), 12); len(w) != 3 {
		t.Errorf("3s range: %d windows", len(w))
	}
	for _, n := range []int{0, 1} {
		if w := sliceWindows(start, start.Add(time.Hour), n); w != nil {
			t.Errorf("n=%d: %v", n, w)
		}
	}
	if w := sliceWindows(start, start, 12); w != nil {
		t.Errorf("empty range: %v", w)
	}
}

// recordingBackend answers each search with Limit traces spread over the
// request's window, newest first, and records the requests.
type recordingBackend struct {
	Backend
	mu   sync.Mutex
	reqs []SearchRequest
	fail error
	id   string        // trace ID prefix, so two stores can hold different traces
	skew time.Duration // shifts every start back, so two stores' traces interleave
}

func (b *recordingBackend) Search(_ context.Context, req *SearchRequest) ([]TraceSummary, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, *req)
	b.mu.Unlock()
	if b.fail != nil {
		return nil, b.fail
	}
	var out []TraceSummary
	for i := range req.Limit {
		ts := req.End.Add(-time.Duration(i)*time.Second - b.skew)
		out = append(out, TraceSummary{TraceID: b.id + strconv.FormatInt(ts.Unix(), 16), StartTimeUnixNano: strconv.FormatInt(ts.UnixNano(), 10)})
	}
	return out, nil
}

func allowAll(names ...string) *federator {
	f := &federator{authz: &authorizer{ttl: time.Hour, cache: map[string]cacheEntry{}, resource: "traces"}}
	for _, n := range names {
		f.authz.cache[tokenHash("t")+" "+ResourceAttributes{Verb: "get", Resource: "traces", Name: n}.key()] = cacheEntry{allowed: true, at: time.Now()}
	}
	return f
}

func TestSearchSlices(t *testing.T) {
	end := time.Now().Truncate(time.Second)
	start := end.Add(-time.Hour)
	b := &recordingBackend{}
	s := &store{Backend: b, cfg: BackendConfig{Name: "hot", Access: "rbac", SearchTimeout: Duration{time.Second}}}
	f := allowAll("hot")
	params := map[string]string{"q": "{}", "start": strconv.FormatInt(start.Unix(), 10), "end": strconv.FormatInt(end.Unix(), 10), "limit": "100", "slices": "12"}
	out, st, err := f.search(context.Background(), &viewer{token: "t"}, []*store{s},
		SearchRequest{Raw: "{}", Start: start, End: end, Limit: 100, Slices: 12, Params: params})
	if err != nil || st[0].Status != "ok" {
		t.Fatalf("%v %+v", err, st)
	}
	if len(b.reqs) != 12 {
		t.Fatalf("%d store queries, want 12", len(b.reqs))
	}
	seen := map[string]bool{}
	for _, r := range b.reqs {
		if r.Limit != 9 || r.Params["limit"] != "9" { // ceil(100/12)
			t.Errorf("window limit %d / %q", r.Limit, r.Params["limit"])
		}
		if r.Params["start"] != strconv.FormatInt(r.Start.Unix(), 10) || r.Params["end"] != strconv.FormatInt(r.End.Unix(), 10) {
			t.Errorf("passthrough params %v do not match window %v..%v", r.Params, r.Start, r.End)
		}
		seen[r.Params["start"]] = true
	}
	if len(seen) != 12 {
		t.Errorf("windows not distinct: %v", seen)
	}
	if params["start"] != strconv.FormatInt(start.Unix(), 10) || params["limit"] != "100" {
		t.Errorf("caller's params were modified: %v", params)
	}
	// 12 windows x 9 = 108 traces, newest first, cut to the limit; the
	// oldest window still contributes, which a single query would not.
	if len(out) != 100 {
		t.Fatalf("%d traces", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].startNanos() > out[i-1].startNanos() {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if oldest := time.Unix(0, out[len(out)-1].startNanos()); oldest.After(start.Add(10 * time.Minute)) {
		t.Errorf("oldest sample %v is not from the first window", end.Sub(oldest))
	}
}

// Two stores holding different traces return about twice the limit between
// them; the cut must keep every window, not just the newest half of the range.
func TestSearchSlicesKeepsSpreadAcrossStores(t *testing.T) {
	end := time.Now().Truncate(time.Second)
	start := end.Add(-time.Hour)
	var view []*store
	for _, b := range []*recordingBackend{{id: "a"}, {id: "b", skew: 500 * time.Millisecond}} {
		view = append(view, &store{Backend: b, cfg: BackendConfig{Name: b.id, Access: "rbac", SearchTimeout: Duration{time.Second}}})
	}
	out, st, err := allowAll("a", "b").search(context.Background(), &viewer{token: "t"}, view,
		SearchRequest{Raw: "{}", Start: start, End: end, Limit: 480, Slices: 12, Params: map[string]string{}})
	if err != nil || st[0].Status != "ok" || st[1].Status != "ok" {
		t.Fatalf("%v %+v", err, st)
	}
	if len(out) != 480 {
		t.Fatalf("%d traces", len(out))
	}
	perWindow := map[int]int{}
	for _, tr := range out {
		perWindow[int(time.Unix(0, tr.startNanos()).Sub(start)/(5*time.Minute))]++
	}
	for w := range 12 {
		if perWindow[w] == 0 {
			t.Errorf("window %d has no traces: %v", w, perWindow)
		}
	}
	for i := 1; i < len(out); i++ {
		if out[i].startNanos() > out[i-1].startNanos() {
			t.Fatalf("not newest first at %d", i)
		}
	}
}

func TestSearchSlicesDedupesAcrossWindowsAndStores(t *testing.T) {
	dup := func(id string, ns int64) TraceSummary {
		return TraceSummary{TraceID: id, StartTimeUnixNano: strconv.FormatInt(ns, 10)}
	}
	// A trace that spans a window boundary comes back from both windows.
	hot := []TraceSummary{dup("aa", 2000), dup("bb", 1000), dup("aa", 2000)}
	cold := []TraceSummary{dup("aa", 1500)}
	out := mergeSearch([]string{"hot", "cold"}, [][]TraceSummary{hot, cold}, 10)
	if len(out) != 2 {
		t.Fatalf("%+v", out)
	}
	if strings.Join(out[0].Stores, ",") != "hot,cold" || out[0].StartTimeUnixNano != "1500" {
		t.Errorf("merged = %+v", out[0])
	}
}

func TestSearchWithoutSlicesIsOneQuery(t *testing.T) {
	end := time.Now().Truncate(time.Second)
	for _, req := range []SearchRequest{
		{Raw: "{}", Start: end.Add(-time.Hour), End: end, Limit: 20},            // param absent
		{Raw: "{}", Start: end.Add(-time.Hour), End: end, Limit: 20, Slices: 1}, // slices=1
		{Raw: "{}", Limit: 20, Slices: 12},                                      // no range to split
	} {
		b := &recordingBackend{}
		s := &store{Backend: b, cfg: BackendConfig{Name: "hot", Access: "rbac", SearchTimeout: Duration{time.Second}}}
		if _, _, err := allowAll("hot").search(context.Background(), &viewer{token: "t"}, []*store{s}, req); err != nil {
			t.Fatal(err)
		}
		if len(b.reqs) != 1 || b.reqs[0].Limit != 20 || !b.reqs[0].Start.Equal(req.Start) {
			t.Errorf("slices=%d: %d queries %+v", req.Slices, len(b.reqs), b.reqs)
		}
	}
}

func TestSearchSlicesWindowErrorFailsStore(t *testing.T) {
	end := time.Now().Truncate(time.Second)
	b := &recordingBackend{fail: errors.New("boom")}
	s := &store{Backend: b, cfg: BackendConfig{Name: "hot", Access: "rbac", SearchTimeout: Duration{time.Second}}}
	_, st, _ := allowAll("hot").search(context.Background(), &viewer{token: "t"}, []*store{s},
		SearchRequest{Raw: "{}", Start: end.Add(-time.Hour), End: end, Limit: 20, Slices: 4, Params: map[string]string{}})
	if st[0].Status != "error" || st[0].Message != "boom" {
		t.Errorf("%+v", st)
	}
}

func TestSlicesParamReachesTempoStore(t *testing.T) {
	var mu sync.Mutex
	var got []url.Values
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"traces":[]}`))
	}))
	defer tempo.Close()
	kube := &fakeKube{allow: map[string]map[string]bool{"admin": {"*": true}}}
	srv := newTestServer(t, kube, &fakeSentry{}, tempo.URL)
	end := time.Now().Unix()
	u := srv.URL + "/proxy/tracing/all-stores/x/api/search?q=%7B%7D&limit=50&start=" + strconv.FormatInt(end-3600, 10) + "&end=" + strconv.FormatInt(end, 10)
	for _, tc := range []struct {
		slices string
		want   int
	}{{"", 1}, {"6", 6}, {"99", maxSlices}} {
		got = nil
		q := u
		if tc.slices != "" {
			q += "&slices=" + tc.slices
		}
		if code, _ := get(t, q, "admin", nil); code != 200 {
			t.Fatalf("slices=%s: %d", tc.slices, code)
		}
		if len(got) != tc.want {
			t.Errorf("slices=%q: tempo got %d queries, want %d", tc.slices, len(got), tc.want)
		}
		for _, v := range got {
			if v.Get("slices") != "" {
				t.Errorf("slices leaked to the store: %v", v)
			}
		}
	}
}
