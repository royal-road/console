package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// store is a configured backend with its settings.
type store struct {
	Backend
	cfg BackendConfig
}

// StoreStatus says what one backend contributed to a response.
type StoreStatus struct {
	Store   string `json:"store"`
	Status  string `json:"status"` // ok | skipped | unsupported | error | timeout | denied
	Message string `json:"message,omitempty"`
}

// partial reports whether the status means results may be missing.
func (s StoreStatus) partial() bool {
	return s.Status == "unsupported" || s.Status == "error" || s.Status == "timeout"
}

type federator struct {
	stores []*store // sorted by priority, highest first
	authz  *authorizer
	views  map[string][]*store
	order  []string
}

// viewer is the person behind a request: their bearer token, as forwarded by
// the console bridge's plugin proxy (authorize: true).
type viewer struct {
	token string
}

// access is the per-backend authorization outcome.
type access struct {
	allowed bool
	scope   *Scope
	err     error
}

func (f *federator) authorize(ctx context.Context, v *viewer, s *store) access {
	switch s.cfg.Access {
	case "rbac":
		ok, err := f.authz.canReadBackend(ctx, v.token, s.cfg.Name)
		return access{allowed: ok, err: err}
	case "sentry-bindings":
		scope, err := f.authz.sentryScope(ctx, v.token)
		if err != nil {
			return access{err: err}
		}
		return access{allowed: len(scope.Projects) > 0, scope: scope}
	}
	return access{err: fmt.Errorf("unknown access mode %q", s.cfg.Access)}
}

// visibleViews lists the views with at least one backend the viewer may read.
func (f *federator) visibleViews(ctx context.Context, v *viewer) ([]string, error) {
	readable := map[string]bool{}
	for _, s := range f.stores {
		a := f.authorize(ctx, v, s)
		if errors.Is(a.err, errUnauthenticated) {
			return nil, a.err
		}
		readable[s.cfg.Name] = a.allowed
	}
	var out []string
	for _, name := range f.order {
		for _, s := range f.views[name] {
			if readable[s.cfg.Name] {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// fanOut runs fn on every store of the view the viewer may read, in
// parallel, each under its own timeout.
func (f *federator) fanOut(ctx context.Context, v *viewer, stores []*store, timeout func(*store) time.Duration,
	skip func(*store) string, fn func(ctx context.Context, s *store, scope *Scope) error) ([]StoreStatus, error) {
	statuses := make([]StoreStatus, len(stores))
	var wg sync.WaitGroup
	var authErr error
	var mu sync.Mutex
	for i, s := range stores {
		statuses[i].Store = s.cfg.Name
		if reason := skip(s); reason != "" {
			statuses[i].Status, statuses[i].Message = "skipped", reason
			continue
		}
		wg.Add(1)
		go func(i int, s *store) {
			defer wg.Done()
			a := f.authorize(ctx, v, s)
			st := &statuses[i]
			switch {
			case errors.Is(a.err, errUnauthenticated):
				mu.Lock()
				authErr = a.err
				mu.Unlock()
				st.Status = "denied"
				return
			case a.err != nil:
				st.Status, st.Message = "error", "authorization: "+a.err.Error()
				return
			case !a.allowed:
				st.Status = "denied"
				return
			}
			cctx, cancel := context.WithTimeout(ctx, timeout(s))
			defer cancel()
			began := time.Now()
			err := fn(cctx, s, a.scope)
			metricBackendDuration.observe(time.Since(began).Seconds(), s.cfg.Name)
			switch {
			case err == nil:
				st.Status = "ok"
			case isUnsupported(err):
				st.Status, st.Message = "unsupported", err.Error()
			case errors.Is(cctx.Err(), context.DeadlineExceeded):
				st.Status, st.Message = "timeout", "no answer within "+timeout(s).String()
			default:
				st.Status, st.Message = "error", err.Error()
			}
			metricBackendRequests.inc(s.cfg.Name, st.Status)
		}(i, s)
	}
	wg.Wait()
	return statuses, authErr
}

// outOfRange reports why a store cannot hold data for [start, end].
func outOfRange(s *store, start, end time.Time) string {
	if end.IsZero() || s.cfg.MaxLookback.Duration == 0 {
		return ""
	}
	if end.Before(time.Now().Add(-s.cfg.MaxLookback.Duration)) {
		return "time range is older than this store's retention (" + s.cfg.MaxLookback.String() + ")"
	}
	return ""
}

func (f *federator) search(ctx context.Context, v *viewer, view []*store, req SearchRequest) ([]TraceSummary, []StoreStatus, error) {
	var mu sync.Mutex
	perStore := map[string][]TraceSummary{}
	statuses, err := f.fanOut(ctx, v, view,
		func(s *store) time.Duration { return s.cfg.SearchTimeout.Duration },
		func(s *store) string { return outOfRange(s, req.Start, req.End) },
		func(ctx context.Context, s *store, scope *Scope) error {
			r := req
			r.Scope = scope
			if lb := s.cfg.MaxLookback.Duration; lb > 0 && !r.Start.IsZero() && r.Start.Before(time.Now().Add(-lb)) {
				r.Start = time.Now().Add(-lb)
			}
			if s.cfg.Type != "tempo" && r.ParseErr != nil {
				return r.ParseErr
			}
			res, err := searchSliced(ctx, s, r)
			if err != nil {
				return err
			}
			mu.Lock()
			perStore[s.cfg.Name] = res
			mu.Unlock()
			return nil
		})
	if err != nil {
		return nil, statuses, err
	}
	var ordered [][]TraceSummary
	var names []string
	for _, s := range view {
		ordered = append(ordered, perStore[s.cfg.Name])
		names = append(names, s.cfg.Name)
	}
	return mergeSearch(names, ordered, req.Limit), statuses, nil
}

// maxSlices caps the search's slices parameter.
const maxSlices = 24

// sliceWindows splits [start, end] into n equal windows, oldest first, on
// whole seconds (Tempo's start and end are Unix seconds). It returns nil
// when there is nothing to split.
func sliceWindows(start, end time.Time, n int) [][2]time.Time {
	secs := end.Unix() - start.Unix()
	n = int(min(int64(n), secs))
	if n <= 1 {
		return nil
	}
	out := make([][2]time.Time, n)
	for i := range n {
		out[i] = [2]time.Time{
			time.Unix(start.Unix()+secs*int64(i)/int64(n), 0),
			time.Unix(start.Unix()+secs*int64(i+1)/int64(n), 0),
		}
	}
	return out
}

// searchSliced runs one store's search. With req.Slices > 1 and a bounded
// time range, it searches each of req.Slices equal windows in parallel with
// limit ceil(Limit/Slices), so the samples spread over the range instead of
// all coming from its most recent minutes. Window results are concatenated;
// mergeSearch de-duplicates them.
func searchSliced(ctx context.Context, s *store, req SearchRequest) ([]TraceSummary, error) {
	windows := sliceWindows(req.Start, req.End, req.Slices)
	if req.Start.IsZero() || req.End.IsZero() || windows == nil {
		return s.Search(ctx, &req)
	}
	perWindow := (req.Limit + len(windows) - 1) / len(windows)
	results := make([][]TraceSummary, len(windows))
	errs := make([]error, len(windows))
	var wg sync.WaitGroup
	for i, w := range windows {
		r := req
		r.Start, r.End, r.Limit = w[0], w[1], perWindow
		r.Params = map[string]string{}
		maps.Copy(r.Params, req.Params)
		r.Params["start"] = strconv.FormatInt(w[0].Unix(), 10)
		r.Params["end"] = strconv.FormatInt(w[1].Unix(), 10)
		r.Params["limit"] = strconv.Itoa(perWindow)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Search(ctx, &r)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err // a store missing some windows would skew the spread
		}
	}
	return slices.Concat(results...), nil
}

// mergeSearch unions per-store results (stores in priority order) by trace
// ID: earliest start, longest duration, the first real root name, union of
// span sets and service stats. Newest first, cut to limit.
func mergeSearch(stores []string, results [][]TraceSummary, limit int) []TraceSummary {
	byID := map[string]*TraceSummary{}
	var order []string
	for i, res := range results {
		for _, t := range res {
			t := t
			id := normalizeID(t.TraceID, 32)
			t.TraceID = id
			prev, ok := byID[id]
			if !ok {
				t.Stores = []string{stores[i]}
				byID[id] = &t
				order = append(order, id)
				continue
			}
			if !slices.Contains(prev.Stores, stores[i]) { // a sliced search can repeat a trace
				prev.Stores = append(prev.Stores, stores[i])
			}
			if ts := t.startNanos(); ts != 0 && (prev.startNanos() == 0 || ts < prev.startNanos()) {
				prev.StartTimeUnixNano = t.StartTimeUnixNano
			}
			if t.DurationMs > prev.DurationMs {
				prev.DurationMs = t.DurationMs
			}
			if (prev.RootServiceName == "" || prev.RootServiceName == rootNotReceived) && t.RootServiceName != "" && t.RootServiceName != rootNotReceived {
				prev.RootServiceName, prev.RootTraceName = t.RootServiceName, t.RootTraceName
			}
			for svc, st := range t.ServiceStats {
				if prev.ServiceStats == nil {
					prev.ServiceStats = map[string]ServiceStats{}
				}
				cur := prev.ServiceStats[svc]
				cur.SpanCount = max(cur.SpanCount, st.SpanCount)
				cur.ErrorCount = max(cur.ErrorCount, st.ErrorCount)
				prev.ServiceStats[svc] = cur
			}
			if prev.SpanSet == nil {
				prev.SpanSet = t.SpanSet
			}
			prev.SpanSets = append(prev.SpanSets, t.SpanSets...)
		}
	}
	out := make([]TraceSummary, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].startNanos() > out[j].startNanos() })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *federator) trace(ctx context.Context, v *viewer, view []*store, traceID string) ([]map[string]any, []StoreStatus, error) {
	var mu sync.Mutex
	perStore := map[string][]map[string]any{}
	statuses, err := f.fanOut(ctx, v, view,
		func(s *store) time.Duration { return s.cfg.TraceTimeout.Duration },
		func(*store) string { return "" },
		func(ctx context.Context, s *store, scope *Scope) error {
			rs, err := s.Trace(ctx, &TraceRequest{TraceID: traceID, Scope: scope})
			if err != nil {
				return err
			}
			mu.Lock()
			perStore[s.cfg.Name] = rs
			mu.Unlock()
			return nil
		})
	if err != nil {
		return nil, statuses, err
	}
	var names []string
	var ordered [][]map[string]any
	for _, s := range view { // view is in priority order
		names = append(names, s.cfg.Name)
		ordered = append(ordered, perStore[s.cfg.Name])
	}
	return mergeTrace(names, ordered), statuses, nil
}

// mergeTrace concatenates resourceSpans from the stores (priority order),
// tags each resource with federation.store, converts IDs to hex, and keeps
// the first copy of every span ID.
func mergeTrace(stores []string, results [][]map[string]any) []map[string]any {
	seen := map[string]bool{}
	var out []map[string]any
	for i, rss := range results {
		for _, rs := range rss {
			res, _ := rs["resource"].(map[string]any)
			if res == nil {
				res = map[string]any{}
				rs["resource"] = res
			}
			attrs, _ := res["attributes"].([]any)
			res["attributes"] = append(attrs, kvString("federation.store", stores[i]))
			scopes, _ := rs["scopeSpans"].([]any)
			var keptScopes []any
			for _, ssAny := range scopes {
				ss, ok := ssAny.(map[string]any)
				if !ok {
					continue
				}
				spans, _ := ss["spans"].([]any)
				var kept []any
				for _, spAny := range spans {
					sp, ok := spAny.(map[string]any)
					if !ok {
						continue
					}
					normalizeSpanIDs(sp)
					id := str(sp["spanId"])
					if seen[id] {
						metricDedupedSpans.inc(stores[i])
						continue
					}
					seen[id] = true
					kept = append(kept, sp)
				}
				if len(kept) > 0 {
					ss["spans"] = kept
					keptScopes = append(keptScopes, ss)
				}
			}
			if len(keptScopes) > 0 {
				rs["scopeSpans"] = keptScopes
				out = append(out, rs)
			}
		}
	}
	return out
}

func normalizeSpanIDs(sp map[string]any) {
	sp["traceId"] = normalizeID(str(sp["traceId"]), 32)
	sp["spanId"] = normalizeID(str(sp["spanId"]), 16)
	if p := str(sp["parentSpanId"]); p != "" {
		sp["parentSpanId"] = normalizeID(p, 16)
	}
	if links, ok := sp["links"].([]any); ok {
		for _, l := range links {
			if lm, ok := l.(map[string]any); ok {
				lm["traceId"] = normalizeID(str(lm["traceId"]), 32)
				lm["spanId"] = normalizeID(str(lm["spanId"]), 16)
			}
		}
	}
}

func (f *federator) tags(ctx context.Context, v *viewer, view []*store, req TagsRequest) (*TagSet, []StoreStatus, error) {
	var mu sync.Mutex
	merged := &TagSet{}
	statuses, err := f.fanOut(ctx, v, view,
		func(s *store) time.Duration { return s.cfg.SearchTimeout.Duration },
		func(s *store) string { return outOfRange(s, req.Start, req.End) },
		func(ctx context.Context, s *store, scope *Scope) error {
			r := req
			r.Scope = scope
			ts, err := s.Tags(ctx, &r)
			if err != nil || ts == nil {
				return err
			}
			mu.Lock()
			merged.Resource = append(merged.Resource, ts.Resource...)
			merged.Span = append(merged.Span, ts.Span...)
			merged.Intrinsic = append(merged.Intrinsic, ts.Intrinsic...)
			mu.Unlock()
			return nil
		})
	merged.Resource = sortedUnique(merged.Resource)
	merged.Span = sortedUnique(merged.Span)
	merged.Intrinsic = sortedUnique(merged.Intrinsic)
	return merged, statuses, err
}

func (f *federator) tagValues(ctx context.Context, v *viewer, view []*store, req TagValuesRequest) ([]TagValue, []StoreStatus, error) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var out []TagValue
	statuses, err := f.fanOut(ctx, v, view,
		func(s *store) time.Duration { return s.cfg.SearchTimeout.Duration },
		func(s *store) string { return outOfRange(s, req.Start, req.End) },
		func(ctx context.Context, s *store, scope *Scope) error {
			r := req
			r.Scope = scope
			vals, err := s.TagValues(ctx, &r)
			if err != nil {
				return err
			}
			mu.Lock()
			for _, tv := range vals {
				if !seen[tv.Value] {
					seen[tv.Value] = true
					out = append(out, tv)
				}
			}
			mu.Unlock()
			return nil
		})
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	if len(out) > 1000 {
		out = out[:1000]
	}
	return out, statuses, err
}

func sortedUnique(in []string) []string {
	out := dedupe(in)
	sort.Strings(out)
	return out
}

// statusHeader renders statuses for the X-Federation-Status header: JSON,
// ASCII only.
func statusHeader(statuses []StoreStatus) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range statuses {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"store":%s,"status":%s`, asciiJSON(s.Store), asciiJSON(s.Status))
		if s.Message != "" {
			fmt.Fprintf(&b, `,"message":%s`, asciiJSON(truncate(s.Message, 300)))
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	return b.String()
}

func asciiJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&b, `\u%04x`, r&0xffff)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func partialMessage(statuses []StoreStatus) string {
	var parts []string
	for _, s := range statuses {
		if s.partial() {
			parts = append(parts, s.Store+": "+s.Status)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Partial result, some trace stores did not answer (" + strings.Join(parts, ", ") + ")"
}
