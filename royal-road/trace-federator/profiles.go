package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Profiles: a passthrough to Pyroscope's Connect API, scoped per namespace.
//
//	GET  /profiles/{namespace}/scopes    the selectors a viewer may narrow
//	POST /profiles/{namespace}/{method}  POST <pyroscope>/querier.v1.QuerierService/{method}
//
// The viewer needs get on pods in {namespace}. Every selector in the request
// must be scoped to {namespace} (checkSelector); requests are rebuilt from
// an allowlist of fields, so nothing outside it reaches Pyroscope. Errors use
// Pyroscope's Connect shape {code, message}. See README.md, "Profiles".

const (
	profilesMaxBody      = 64 << 10
	profilesDefaultNodes = 8192 // Pyroscope's own default
	profilesMaxPoints    = 2000
)

type profilesProxy struct {
	cfg    *ProfilesConfig
	client *http.Client
}

// profileMethods maps each passed-through method to its request builder.
var profileMethods = map[string]func(p *profilesProxy, ns string, body json.RawMessage) (any, error){
	"SelectMergeStacktraces": (*profilesProxy).selectMerge,
	"SelectSeries":           (*profilesProxy).selectSeries,
	"Diff":                   (*profilesProxy).diff,
	"Series":                 (*profilesProxy).series,
	"LabelNames":             (*profilesProxy).labelNames,
	"LabelValues":            (*profilesProxy).labelValues,
}

// connectError is a request the federator answers itself.
type connectError struct {
	status int
	code   string
	msg    string
}

func (e *connectError) Error() string { return e.msg }

func invalid(format string, a ...any) error {
	return &connectError{http.StatusBadRequest, "invalid_argument", fmt.Sprintf(format, a...)}
}

func writeConnectError(w http.ResponseWriter, err error) {
	var ce *connectError
	if !errors.As(err, &ce) {
		ce = &connectError{http.StatusBadGateway, "unavailable", err.Error()}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, ce.status, map[string]string{"code": ce.code, "message": ce.msg})
}

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func isNamespace(s string) bool { return len(s) <= 63 && namespaceRE.MatchString(s) }

// authorize checks the viewer may read {namespace}'s profiles: get on pods
// there.
func (s *server) authorizeProfiles(r *http.Request) (string, error) {
	if s.fed.profiles == nil {
		return "", &connectError{http.StatusNotFound, "not_found", "profiles are not configured"}
	}
	ns := r.PathValue("namespace")
	if !isNamespace(ns) {
		return "", invalid("%q is not a namespace name", ns)
	}
	tok := bearer(r)
	if tok == "" {
		return "", &connectError{http.StatusUnauthorized, "unauthenticated", "missing bearer token"}
	}
	ok, err := s.fed.authz.check(r.Context(), tok, ResourceAttributes{Namespace: ns, Verb: "get", Resource: "pods"})
	switch {
	case errors.Is(err, errUnauthenticated):
		return "", &connectError{http.StatusUnauthorized, "unauthenticated", err.Error()}
	case err != nil:
		return "", &connectError{http.StatusBadGateway, "unavailable", "access review: " + err.Error()}
	case !ok:
		return "", &connectError{http.StatusForbidden, "permission_denied", "no access to pods in " + ns}
	}
	return ns, nil
}

func (s *server) profileScopes(w http.ResponseWriter, r *http.Request) {
	ns, err := s.authorizeProfiles(r)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	type legacy struct {
		Service  string `json:"service"`
		Selector string `json:"selector"`
	}
	out := struct {
		Namespace string   `json:"namespace"`
		Labelled  string   `json:"labelled"`
		Legacy    []legacy `json:"legacy"`
	}{ns, labelSelector{{"namespace", "=", ns}}.String(), []legacy{}}
	for _, svc := range s.fed.profiles.cfg.LegacyServices[ns] {
		out.Legacy = append(out.Legacy, legacy{svc, labelSelector{{"service_name", "=", svc}}.String()})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (s *server) profileQuery(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	build, ok := profileMethods[method]
	if !ok {
		writeConnectError(w, &connectError{http.StatusNotFound, "not_found", "unsupported profiles method " + method})
		return
	}
	ns, err := s.authorizeProfiles(r)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	p := s.fed.profiles
	body, err := readProfileBody(w, r)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	req, err := build(p, ns, body)
	if err != nil {
		var ce *connectError
		if errors.As(err, &ce) && ce.status == http.StatusBadRequest {
			slog.Warn("profiles request rejected", "namespace", ns, "method", method, "err", ce.msg)
		}
		writeConnectError(w, err)
		return
	}
	upstream, _ := json.Marshal(req)
	p.forward(w, r, method, upstream)
}

func readProfileBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return nil, &connectError{http.StatusUnsupportedMediaType, "invalid_argument", "Content-Type must be application/json"}
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, profilesMaxBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, &connectError{http.StatusRequestEntityTooLarge, "resource_exhausted", fmt.Sprintf("request body over %d bytes", profilesMaxBody)}
	}
	if err != nil {
		return nil, invalid("reading body: %v", err)
	}
	return b, nil
}

// forward POSTs the rebuilt request to Pyroscope and streams the answer.
func (p *profilesProxy) forward(w http.ResponseWriter, r *http.Request, method string, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.Timeout.Duration)
	defer cancel()
	began := time.Now()
	outcome := "error"
	defer func() {
		metricBackendRequests.inc("pyroscope", outcome)
		metricBackendDuration.observe(time.Since(began).Seconds(), "pyroscope")
	}()
	timedOut := func() bool { return errors.Is(ctx.Err(), context.DeadlineExceeded) }

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL+"/querier.v1.QuerierService/"+method, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if timedOut() {
			outcome = "timeout"
			writeConnectError(w, &connectError{http.StatusGatewayTimeout, "deadline_exceeded", fmt.Sprintf("Pyroscope did not answer within %s", p.cfg.Timeout.Duration)})
			return
		}
		writeConnectError(w, fmt.Errorf("Pyroscope unreachable: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var upstream struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &upstream)
		if resp.StatusCode >= 500 || upstream.Code == "" {
			msg := upstream.Message
			if msg == "" {
				msg = truncate(strings.TrimSpace(string(data)), 200)
			}
			writeConnectError(w, fmt.Errorf("Pyroscope answered HTTP %d: %s", resp.StatusCode, msg))
			return
		}
		outcome = "ok" // Pyroscope answered; the request was wrong
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, resp.StatusCode, upstream)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Add("Vary", "Accept-Encoding")
	var out io.Writer = w
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		out = gz
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(out, resp.Body); err != nil {
		if timedOut() {
			outcome = "timeout"
		}
		slog.Warn("profiles response cut short", "method", method, "err", err)
		return
	}
	outcome = "ok"
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			return strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0"
		}
	}
	return false
}

// checkSelector enforces the namespace scope on one selector and returns it
// re-serialised. Two shapes are allowed:
//   - labelled: exactly one namespace matcher, namespace="<ns>";
//   - legacy: no namespace matcher, and exactly one service_name matcher,
//     service_name="<s>" with s listed in legacyServices[ns]. The federator
//     appends namespace=~"|<ns>", which matches the service's unlabelled
//     profiles and its profiles labelled with ns, never another namespace's.
//
// Every other matcher can only narrow the result.
func (p *profilesProxy) checkSelector(ns, raw string) (string, error) {
	sel, err := parseLabelSelector(raw)
	if err != nil {
		return "", invalid("selector %q: %v", truncate(raw, 100), err)
	}
	nsm := sel.withName("namespace")
	if len(nsm) == 1 && nsm[0].Op == "=" && nsm[0].Value == ns {
		return sel.String(), nil
	}
	legacy := p.cfg.LegacyServices[ns]
	if svc := sel.withName("service_name"); len(nsm) == 0 && len(svc) == 1 && svc[0].Op == "=" && slices.Contains(legacy, svc[0].Value) {
		return append(sel, labelMatcher{"namespace", "=~", "|" + regexp.QuoteMeta(ns)}).String(), nil
	}
	allowed := strconv.Quote(ns)
	if len(legacy) > 0 {
		quoted := make([]string, len(legacy))
		for i, s := range legacy {
			quoted[i] = strconv.Quote(s)
		}
		allowed += " or service_name=" + "<one of " + strings.Join(quoted, ", ") + ">"
	}
	return "", invalid("selector must contain namespace=%s", allowed)
}

// Request bodies are decoded field by field from an allowlist, with exact
// (case-sensitive) names, and rebuilt; unknown fields are rejected so that
// fields Pyroscope adds later cannot bypass these checks.

type fields map[string]json.RawMessage

func decodeFields(raw json.RawMessage, what string, allowed ...string) (fields, error) {
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return nil, invalid("%s must be a JSON object", what)
	}
	for k := range f {
		if !slices.Contains(allowed, k) {
			return nil, invalid("%s: field %q is not allowed (allowed: %s)", what, k, strings.Join(allowed, ", "))
		}
	}
	return f, nil
}

func (f fields) get(name string, into any) error {
	raw, ok := f[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return invalid("field %s: %v", name, err)
	}
	return nil
}

func (f fields) getAll(into map[string]any) error {
	for name, ptr := range into {
		if err := f.get(name, ptr); err != nil {
			return err
		}
	}
	return nil
}

// int64JSON reads an int64 sent as a JSON number or, as protojson writes
// it, a string.
type int64JSON int64

func (n *int64JSON) UnmarshalJSON(b []byte) error {
	s := string(b)
	if uq, err := strconv.Unquote(s); err == nil {
		s = uq
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not an integer: %s", truncate(string(b), 30))
	}
	*n = int64JSON(v)
	return nil
}

func checkWindow(start, end int64JSON, limit time.Duration) error {
	if start <= 0 || end <= start {
		return invalid("start and end are required, in Unix milliseconds, with end after start")
	}
	if time.Duration(end-start)*time.Millisecond > limit {
		return invalid("window over %s (Pyroscope's limit)", formatDuration(limit))
	}
	return nil
}

func formatDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return d.String()
}

type location struct {
	Name string `json:"name"`
}

type stackTraceSelector struct {
	CallSite []location `json:"callSite"`
}

type selectMergeRequest struct {
	ProfileTypeID      string              `json:"profileTypeID"`
	LabelSelector      string              `json:"labelSelector"`
	Start              int64JSON           `json:"start"`
	End                int64JSON           `json:"end"`
	MaxNodes           int64JSON           `json:"maxNodes"`
	StackTraceSelector *stackTraceSelector `json:"stackTraceSelector,omitempty"`
}

func (p *profilesProxy) selectMerge(ns string, body json.RawMessage) (any, error) {
	return p.selectMergeSide(ns, body, "request")
}

func (p *profilesProxy) selectMergeSide(ns string, body json.RawMessage, what string) (*selectMergeRequest, error) {
	f, err := decodeFields(body, what, "profileTypeID", "labelSelector", "start", "end", "maxNodes", "stackTraceSelector")
	if err != nil {
		return nil, err
	}
	var req selectMergeRequest
	if err := f.getAll(map[string]any{"profileTypeID": &req.ProfileTypeID, "labelSelector": &req.LabelSelector,
		"start": &req.Start, "end": &req.End, "maxNodes": &req.MaxNodes}); err != nil {
		return nil, err
	}
	if req.ProfileTypeID == "" {
		return nil, invalid("%s: profileTypeID is required", what)
	}
	if req.LabelSelector, err = p.checkSelector(ns, req.LabelSelector); err != nil {
		return nil, err
	}
	if err := checkWindow(req.Start, req.End, p.cfg.MaxWindow.Duration); err != nil {
		return nil, err
	}
	switch {
	case req.MaxNodes < 0:
		return nil, invalid("%s: maxNodes must not be negative", what)
	case req.MaxNodes == 0:
		req.MaxNodes = int64JSON(min(profilesDefaultNodes, p.cfg.MaxNodes))
	default:
		req.MaxNodes = min(req.MaxNodes, int64JSON(p.cfg.MaxNodes))
	}
	if raw, ok := f["stackTraceSelector"]; ok {
		sf, err := decodeFields(raw, "stackTraceSelector", "callSite")
		if err != nil {
			return nil, err
		}
		var sites []json.RawMessage
		if err := sf.get("callSite", &sites); err != nil {
			return nil, err
		}
		req.StackTraceSelector = &stackTraceSelector{CallSite: []location{}}
		for _, site := range sites {
			lf, err := decodeFields(site, "callSite entry", "name")
			if err != nil {
				return nil, err
			}
			var loc location
			if err := lf.get("name", &loc.Name); err != nil {
				return nil, err
			}
			req.StackTraceSelector.CallSite = append(req.StackTraceSelector.CallSite, loc)
		}
	}
	return &req, nil
}

func (p *profilesProxy) diff(ns string, body json.RawMessage) (any, error) {
	f, err := decodeFields(body, "request", "left", "right")
	if err != nil {
		return nil, err
	}
	if f["left"] == nil || f["right"] == nil {
		return nil, invalid("left and right are required")
	}
	var out struct {
		Left  *selectMergeRequest `json:"left"`
		Right *selectMergeRequest `json:"right"`
	}
	if out.Left, err = p.selectMergeSide(ns, f["left"], "left"); err != nil {
		return nil, err
	}
	if out.Right, err = p.selectMergeSide(ns, f["right"], "right"); err != nil {
		return nil, err
	}
	return out, nil
}

var aggregations = []string{"TIME_SERIES_AGGREGATION_TYPE_SUM", "TIME_SERIES_AGGREGATION_TYPE_AVERAGE"}

func (p *profilesProxy) selectSeries(ns string, body json.RawMessage) (any, error) {
	var probe fields
	if json.Unmarshal(body, &probe) == nil && probe["stackTraceSelector"] != nil {
		return nil, invalid("SelectSeries ignores stackTraceSelector; use SelectMergeStacktraces per time bucket")
	}
	f, err := decodeFields(body, "request", "profileTypeID", "labelSelector", "start", "end", "step", "groupBy", "aggregation", "limit")
	if err != nil {
		return nil, err
	}
	var req struct {
		ProfileTypeID string    `json:"profileTypeID"`
		LabelSelector string    `json:"labelSelector"`
		Start         int64JSON `json:"start"`
		End           int64JSON `json:"end"`
		Step          float64   `json:"step"`
		GroupBy       []string  `json:"groupBy,omitempty"`
		Aggregation   string    `json:"aggregation,omitempty"`
		Limit         int64JSON `json:"limit,omitempty"`
	}
	if err := f.getAll(map[string]any{"profileTypeID": &req.ProfileTypeID, "labelSelector": &req.LabelSelector,
		"start": &req.Start, "end": &req.End, "step": &req.Step, "groupBy": &req.GroupBy,
		"aggregation": &req.Aggregation, "limit": &req.Limit}); err != nil {
		return nil, err
	}
	if req.ProfileTypeID == "" {
		return nil, invalid("profileTypeID is required")
	}
	if req.LabelSelector, err = p.checkSelector(ns, req.LabelSelector); err != nil {
		return nil, err
	}
	if err := checkWindow(req.Start, req.End, p.cfg.MaxWindow.Duration); err != nil {
		return nil, err
	}
	if !(req.Step >= 1) {
		return nil, invalid("step is required, in seconds, at least 1")
	}
	if float64(req.End-req.Start)/1000/req.Step > profilesMaxPoints {
		return nil, invalid("more than %d points: raise step", profilesMaxPoints)
	}
	for _, l := range req.GroupBy {
		if !isLabelName(l) {
			return nil, invalid("groupBy: %q is not a label name", l)
		}
	}
	if req.Aggregation != "" && !slices.Contains(aggregations, req.Aggregation) {
		return nil, invalid("aggregation must be one of %s", strings.Join(aggregations, ", "))
	}
	if req.Limit < 0 {
		return nil, invalid("limit must not be negative")
	}
	return req, nil
}

// metadataRequest is Series, LabelNames and LabelValues. Pyroscope answers
// nothing when given more than one matcher (they are not ORed), so exactly
// one is required.
type metadataRequest struct {
	Name       string    `json:"name,omitempty"`
	Matchers   []string  `json:"matchers"`
	LabelNames []string  `json:"labelNames,omitempty"`
	Start      int64JSON `json:"start"`
	End        int64JSON `json:"end"`
}

func (p *profilesProxy) metadata(ns string, body json.RawMessage, allowed ...string) (*metadataRequest, error) {
	f, err := decodeFields(body, "request", append(allowed, "matchers", "start", "end")...)
	if err != nil {
		return nil, err
	}
	var req metadataRequest
	if err := f.getAll(map[string]any{"name": &req.Name, "matchers": &req.Matchers, "labelNames": &req.LabelNames,
		"start": &req.Start, "end": &req.End}); err != nil {
		return nil, err
	}
	if len(req.Matchers) != 1 {
		return nil, invalid("exactly one matcher is required (Pyroscope does not OR them)")
	}
	if req.Matchers[0], err = p.checkSelector(ns, req.Matchers[0]); err != nil {
		return nil, err
	}
	// max_query_length applies to the metadata calls too (checked live).
	if err := checkWindow(req.Start, req.End, p.cfg.MaxWindow.Duration); err != nil {
		return nil, err
	}
	for _, l := range req.LabelNames {
		if !isLabelName(l) {
			return nil, invalid("labelNames: %q is not a label name", l)
		}
	}
	return &req, nil
}

func (p *profilesProxy) series(ns string, body json.RawMessage) (any, error) {
	return p.metadata(ns, body, "labelNames")
}

func (p *profilesProxy) labelNames(ns string, body json.RawMessage) (any, error) {
	return p.metadata(ns, body)
}

func (p *profilesProxy) labelValues(ns string, body json.RawMessage) (any, error) {
	req, err := p.metadata(ns, body, "name")
	if err != nil {
		return nil, err
	}
	if !isLabelName(req.Name) {
		return nil, invalid("name must be a label name")
	}
	return req, nil
}
