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
	"math"
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
// typed structs, so nothing outside them reaches Pyroscope. Errors use
// Pyroscope's Connect shape {code, message}. See README.md, "Profiles".

const (
	profilesMaxBody      = 64 << 10
	profilesDefaultNodes = 8192 // Pyroscope's own default
	profilesMaxPoints    = 2000
	// profilesMaxAnswer bounds the buffered answer; the maxNodes ceiling
	// keeps real ones near 1.6 MB.
	profilesMaxAnswer = 32 << 20
)

type profilesProxy struct {
	cfg    *ProfilesConfig
	client *http.Client
}

// profileMethods maps each passed-through method to its request builder.
var profileMethods = map[string]func(p *profilesProxy, ns string, body []byte) (any, error){
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

func readProfileBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
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

// forward POSTs the rebuilt request to Pyroscope and relays the answer. The
// viewer's Accept-Encoding goes upstream, so Pyroscope's gzip reaches the
// viewer as is. The answer is read in full before the status is written, so
// a timeout mid-body is a 504, not a truncated 200.
func (p *profilesProxy) forward(w http.ResponseWriter, r *http.Request, method string, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.Timeout.Duration)
	defer cancel()
	began := time.Now()
	outcome := "error"
	defer func() {
		metricBackendRequests.inc("pyroscope", outcome)
		metricBackendDuration.observe(time.Since(began).Seconds(), "pyroscope")
	}()
	fail := func(err error) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "timeout"
			err = &connectError{http.StatusGatewayTimeout, "deadline_exceeded", fmt.Sprintf("Pyroscope did not answer within %s", p.cfg.Timeout.Duration)}
		}
		writeConnectError(w, err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL+"/querier.v1.QuerierService/"+method, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ae := r.Header.Get("Accept-Encoding"); ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		fail(fmt.Errorf("Pyroscope unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, profilesMaxAnswer+1))
	if err != nil {
		fail(fmt.Errorf("reading Pyroscope's answer: %v", err))
		return
	}
	if len(data) > profilesMaxAnswer {
		writeConnectError(w, fmt.Errorf("Pyroscope's answer is over %d bytes", profilesMaxAnswer))
		return
	}

	if resp.StatusCode/100 != 2 {
		if resp.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
				data, _ = io.ReadAll(io.LimitReader(zr, 64<<10))
			}
		}
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
	h.Set("Vary", "Accept-Encoding")
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		h.Set("Content-Encoding", ce)
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	outcome = "ok"
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

// decodeRequest decodes a request into its typed struct. Unknown fields
// (matched as encoding/json does) get 400, so the plugin hears about a field
// that would otherwise be dropped; the request is then rebuilt from the
// struct, so nothing outside it reaches Pyroscope.
func decodeRequest(body []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalid("request: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("request: data after the JSON object")
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

// maxUnixMilli keeps end within what Pyroscope can turn into nanoseconds.
const maxUnixMilli = int64JSON(math.MaxInt64 / int64(time.Millisecond))

func checkWindow(start, end int64JSON, limit time.Duration) error {
	if start <= 0 || end <= start || end > maxUnixMilli {
		return invalid("start and end are required, in Unix milliseconds, with end after start")
	}
	if int64(end-start) > limit.Milliseconds() {
		return invalid("window over %s (profiles.maxWindow)", formatDuration(limit))
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

func (p *profilesProxy) selectMerge(ns string, body []byte) (any, error) {
	var req selectMergeRequest
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	return &req, p.checkMerge(ns, &req, "request")
}

func (p *profilesProxy) checkMerge(ns string, req *selectMergeRequest, what string) error {
	if req.ProfileTypeID == "" {
		return invalid("%s: profileTypeID is required", what)
	}
	var err error
	if req.LabelSelector, err = p.checkSelector(ns, req.LabelSelector); err != nil {
		return err
	}
	if err := checkWindow(req.Start, req.End, p.cfg.MaxWindow.Duration); err != nil {
		return err
	}
	if req.MaxNodes <= 0 {
		req.MaxNodes = int64JSON(min(profilesDefaultNodes, p.cfg.MaxNodes))
	}
	req.MaxNodes = min(req.MaxNodes, int64JSON(p.cfg.MaxNodes))
	return nil
}

func (p *profilesProxy) diff(ns string, body []byte) (any, error) {
	var req struct {
		Left  *selectMergeRequest `json:"left"`
		Right *selectMergeRequest `json:"right"`
	}
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	if req.Left == nil || req.Right == nil {
		return nil, invalid("left and right are required")
	}
	if err := p.checkMerge(ns, req.Left, "left"); err != nil {
		return nil, err
	}
	return req, p.checkMerge(ns, req.Right, "right")
}

var aggregations = []string{"TIME_SERIES_AGGREGATION_TYPE_SUM", "TIME_SERIES_AGGREGATION_TYPE_AVERAGE"}

// selectSeriesRequest has no stackTraceSelector: Pyroscope ignores it here,
// so it gets 400 as an unknown field.
type selectSeriesRequest struct {
	ProfileTypeID string    `json:"profileTypeID"`
	LabelSelector string    `json:"labelSelector"`
	Start         int64JSON `json:"start"`
	End           int64JSON `json:"end"`
	Step          float64   `json:"step"`
	GroupBy       []string  `json:"groupBy,omitempty"`
	Aggregation   string    `json:"aggregation,omitempty"`
	Limit         int64JSON `json:"limit,omitempty"`
}

func (p *profilesProxy) selectSeries(ns string, body []byte) (any, error) {
	var req selectSeriesRequest
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	if req.ProfileTypeID == "" {
		return nil, invalid("profileTypeID is required")
	}
	var err error
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
	if req.Aggregation != "" && !slices.Contains(aggregations, req.Aggregation) {
		return nil, invalid("aggregation must be one of %s", strings.Join(aggregations, ", "))
	}
	if req.Limit < 0 {
		return nil, invalid("limit must not be negative")
	}
	return &req, nil
}

// metadataRequest is what Series, LabelNames and LabelValues share.
// Pyroscope answers nothing when given more than one matcher (they are not
// ORed), so exactly one is required.
type metadataRequest struct {
	Matchers []string  `json:"matchers"`
	Start    int64JSON `json:"start"`
	End      int64JSON `json:"end"`
}

func (p *profilesProxy) checkMetadata(ns string, req *metadataRequest) error {
	if len(req.Matchers) != 1 {
		return invalid("exactly one matcher is required (Pyroscope does not OR them)")
	}
	var err error
	if req.Matchers[0], err = p.checkSelector(ns, req.Matchers[0]); err != nil {
		return err
	}
	// max_query_length applies to the metadata calls too (checked live).
	return checkWindow(req.Start, req.End, p.cfg.MaxWindow.Duration)
}

func (p *profilesProxy) series(ns string, body []byte) (any, error) {
	var req struct {
		metadataRequest
		LabelNames []string `json:"labelNames,omitempty"`
	}
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	return &req, p.checkMetadata(ns, &req.metadataRequest)
}

func (p *profilesProxy) labelNames(ns string, body []byte) (any, error) {
	var req metadataRequest
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	return &req, p.checkMetadata(ns, &req)
}

func (p *profilesProxy) labelValues(ns string, body []byte) (any, error) {
	var req struct {
		Name string `json:"name"`
		metadataRequest
	}
	if err := decodeRequest(body, &req); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, invalid("name is required")
	}
	return &req, p.checkMetadata(ns, &req.metadataRequest)
}
