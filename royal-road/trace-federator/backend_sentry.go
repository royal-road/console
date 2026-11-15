package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sentryBackend reads span data from Sentry (checked against self-hosted
// 26.10.1, Event Analytics Platform "spans" dataset) with an API token:
//   - search:      GET /api/0/organizations/{org}/traces/
//   - trace by ID: GET /api/0/organizations/{org}/trace/{id}/
//   - tags:        GET /api/0/organizations/{org}/trace-items/attributes/...
//
// Every request is limited to the viewer's Scope (projects and environments
// from SentryProjectBindings). Sentry's trace endpoint ignores the project
// parameter, so trace-by-ID results are also filtered here, span by span.
type sentryBackend struct {
	name        string
	base        string // API base, e.g. http://sentry-web.sentry.svc:9000
	publicURL   string // for links shown to users
	org         string
	token       string
	client      *http.Client
	maxLookback time.Duration
	spanName    string
	allowlist   []string
	unsupported map[string]bool

	mu        sync.Mutex
	projectID map[string]string // slug -> numeric ID
	projectAt time.Time
}

func newSentryBackend(cfg BackendConfig, client *http.Client) (*sentryBackend, error) {
	tok, err := readSecretFile(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	if cfg.Organization == "" {
		return nil, fmt.Errorf("backend %s: organization is required", cfg.Name)
	}
	b := &sentryBackend{
		name: cfg.Name, base: strings.TrimRight(cfg.URL, "/"), publicURL: strings.TrimRight(cfg.PublicURL, "/"),
		org: cfg.Organization, token: tok, client: client, maxLookback: cfg.MaxLookback.Duration,
		spanName: cfg.SpanNameField, allowlist: cfg.AttributeAllowlist, unsupported: map[string]bool{},
	}
	if b.spanName == "" {
		b.spanName = "span.name"
	}
	if b.publicURL == "" {
		b.publicURL = b.base
	}
	for _, a := range cfg.UnsupportedAttributes {
		b.unsupported[a] = true
	}
	return b, nil
}

func (b *sentryBackend) Name() string { return b.name }

func (b *sentryBackend) get(ctx context.Context, path string, params url.Values, out any) (http.Header, error) {
	u := b.base + "/api/0/organizations/" + url.PathEscape(b.org) + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(data)), 200))
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	return resp.Header, dec.Decode(out)
}

// projectIDs maps slugs to Sentry's numeric project IDs, refreshed every 10
// minutes.
func (b *sentryBackend) projectIDs(ctx context.Context) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.projectID != nil && time.Since(b.projectAt) < 10*time.Minute {
		return b.projectID, nil
	}
	ids := map[string]string{}
	cursor := ""
	for page := 0; page < 20; page++ {
		params := url.Values{"per_page": {"100"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var list []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		}
		hdr, err := b.get(ctx, "/projects/", params, &list)
		if err != nil {
			if b.projectID != nil {
				return b.projectID, nil // keep the stale map on errors
			}
			return nil, err
		}
		for _, p := range list {
			ids[p.Slug] = p.ID
		}
		cursor = nextCursor(hdr.Get("Link"))
		if cursor == "" {
			break
		}
	}
	b.projectID, b.projectAt = ids, time.Now()
	return ids, nil
}

var linkNext = regexp.MustCompile(`<[^>]*>;\s*rel="next";\s*results="true";\s*cursor="([^"]+)"`)

func nextCursor(link string) string {
	if m := linkNext.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

// projectGroup is a set of projects queried together: same environment
// restriction.
type projectGroup struct {
	slugs        []string
	environments []string
}

// groups splits the allowed projects into request groups, after narrowing them
// with a resource.service.name condition (service name = project slug).
func groupProjects(scope *Scope, org string, onlySlugs map[string]bool, excludeSlugs map[string]bool) []projectGroup {
	byEnv := map[string]*projectGroup{}
	var keys []string
	seen := map[string]bool{}
	for _, p := range scope.Projects {
		if p.Organization != org || (onlySlugs != nil && !onlySlugs[p.Project]) || excludeSlugs[p.Project] {
			continue
		}
		envs := append([]string(nil), p.Environments...)
		sort.Strings(envs)
		key := strings.Join(envs, ",")
		if seen[p.Project+"|"+key] {
			continue
		}
		seen[p.Project+"|"+key] = true
		g, ok := byEnv[key]
		if !ok {
			g = &projectGroup{environments: envs}
			byEnv[key] = g
			keys = append(keys, key)
		}
		g.slugs = append(g.slugs, p.Project)
	}
	sort.Strings(keys)
	out := make([]projectGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byEnv[k])
	}
	return out
}

// allowed reports whether a span from project/environment is in scope.
func allowed(scope *Scope, org, project, env string) bool {
	for _, p := range scope.Projects {
		if p.Organization != org || p.Project != project {
			continue
		}
		if len(p.Environments) == 0 {
			return true
		}
		for _, e := range p.Environments {
			if e == env {
				return true
			}
		}
	}
	return false
}

// sentryQuery translates conditions into Sentry's search syntax. Conditions
// on resource.service.name are returned separately: they select projects.
func (b *sentryBackend) sentryQuery(conds []Cond) (q string, only, exclude map[string]bool, err error) {
	var parts []string
	for _, c := range conds {
		if c.Or == nil && c.Attr.Is(ScopeResource, "service.name") {
			vals := []string{c.Value.Str}
			if c.Op == "=~" || c.Op == "!~" {
				if c.Alternatives == nil {
					return "", nil, nil, errUnsupported("regex on service name")
				}
				vals = c.Alternatives
			}
			set := map[string]bool{}
			for _, v := range vals {
				set[v] = true
			}
			switch c.Op {
			case "=", "=~":
				if only == nil {
					only = set
				} else {
					for k := range only {
						if !set[k] {
							delete(only, k)
						}
					}
				}
			case "!=", "!~":
				if exclude == nil {
					exclude = map[string]bool{}
				}
				for k := range set {
					exclude[k] = true
				}
			default:
				return "", nil, nil, errUnsupported("service.name %s", c.Op)
			}
			continue
		}
		s, err := b.sentryCond(c)
		if err != nil {
			return "", nil, nil, err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " "), only, exclude, nil
}

func (b *sentryBackend) sentryCond(c Cond) (string, error) {
	if c.Or != nil {
		var alts []string
		for _, a := range c.Or {
			s, err := b.sentryCond(a)
			if err != nil {
				return "", err
			}
			alts = append(alts, "("+s+")")
		}
		return "(" + strings.Join(alts, " OR ") + ")", nil
	}
	a := c.Attr
	if b.unsupported[a.String()] || (a.Scope == ScopeAny && (b.unsupported["resource."+a.Name] || b.unsupported["span."+a.Name])) {
		return "", errUnsupported("%s is not recorded in Sentry", a)
	}
	if a.Scope == ScopeIntrinsic {
		switch a.Name {
		case "status":
			if c.Value.Kind != ValEnum || (c.Op != "=" && c.Op != "!=") {
				return "", errUnsupported("status %s %s", c.Op, c.Value)
			}
			var s string
			switch c.Value.Str {
			case "error":
				s = "(has:span.status !span.status:ok)"
			case "ok":
				s = "span.status:ok"
			case "unset":
				s = "!has:span.status"
			default:
				return "", errUnsupported("status %s", c.Value.Str)
			}
			if c.Op == "!=" {
				switch c.Value.Str {
				case "error":
					s = "(!has:span.status OR span.status:ok)"
				case "ok":
					s = "!span.status:ok"
				case "unset":
					s = "has:span.status"
				}
			}
			return s, nil
		case "duration":
			if c.Value.Kind != ValDuration {
				return "", errUnsupported("duration needs a duration value")
			}
			ms := strconv.FormatFloat(float64(c.Value.Duration)/float64(time.Millisecond), 'f', -1, 64) + "ms"
			switch c.Op {
			case ">", ">=", "<", "<=":
				return "span.duration:" + c.Op + ms, nil
			case "=":
				return "span.duration:" + ms, nil
			}
			return "", errUnsupported("duration %s", c.Op)
		case "name":
			return sentryString(b.spanName, c)
		}
		return "", errUnsupported("intrinsic %s on Sentry", a.Name)
	}
	key := a.Name
	if a.Scope == ScopeResource {
		switch a.Name {
		case "deployment.environment":
			key = "environment"
		case "service.version":
			key = "release"
		}
	}
	switch c.Value.Kind {
	case ValString:
		return sentryString(key, c)
	case ValNumber:
		v := strconv.FormatFloat(c.Value.Num, 'f', -1, 64)
		switch c.Op {
		case "=":
			return key + ":" + v, nil
		case "!=":
			return "!" + key + ":" + v, nil
		case ">", ">=", "<", "<=":
			return key + ":" + c.Op + v, nil
		}
	case ValBool:
		v := strconv.FormatBool(c.Value.Bool)
		switch c.Op {
		case "=":
			return key + ":" + v, nil
		case "!=":
			return "!" + key + ":" + v, nil
		}
	}
	return "", errUnsupported("%s %s %s on Sentry", a, c.Op, c.Value)
}

var simpleWildcard = regexp.MustCompile(`^[^.+*?()\[\]{}^$|\\]*(\.\*)?$`)

func sentryString(key string, c Cond) (string, error) {
	if c.Value.Kind != ValString {
		return "", errUnsupported("%s needs a string", key)
	}
	switch c.Op {
	case "=":
		return key + ":" + sentryQuote(c.Value.Str), nil
	case "!=":
		return "!" + key + ":" + sentryQuote(c.Value.Str), nil
	case "=~", "!~":
		neg := ""
		if c.Op == "!~" {
			neg = "!"
		}
		if c.Alternatives != nil {
			q := make([]string, len(c.Alternatives))
			for i, s := range c.Alternatives {
				q[i] = sentryQuote(s)
			}
			return neg + key + ":[" + strings.Join(q, ",") + "]", nil
		}
		// "prefix.*" is the only regex Sentry's wildcard syntax can express.
		if simpleWildcard.MatchString(c.Value.Str) {
			return neg + key + ":" + sentryQuote(strings.TrimSuffix(c.Value.Str, ".*")+"*"), nil
		}
		return "", errUnsupported("regex %q on Sentry (only literals, alternations and prefix.*)", c.Value.Str)
	}
	return "", errUnsupported("%s %s", key, c.Op)
}

func sentryQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (b *sentryBackend) Search(ctx context.Context, req *SearchRequest) ([]TraceSummary, error) {
	if req.Query == nil {
		return nil, req.ParseErr
	}
	conds, minD, maxD, err := req.Query.TraceDurationFilter()
	if err != nil {
		return nil, err
	}
	q, only, exclude, err := b.sentryQuery(conds)
	if err != nil {
		return nil, err
	}
	groups := groupProjects(req.Scope, b.org, only, exclude)
	if len(groups) == 0 {
		return nil, nil
	}
	ids, err := b.projectIDs(ctx)
	if err != nil {
		return nil, err
	}
	start, end := defaultWindow(req.Start, req.End)
	limit := req.Limit
	if minD > 0 || maxD > 0 {
		limit *= 3
	}
	var out []TraceSummary
	for _, g := range groups {
		params := url.Values{
			"start":    {start.UTC().Format("2006-01-02T15:04:05")},
			"end":      {end.UTC().Format("2006-01-02T15:04:05")},
			"per_page": {strconv.Itoa(min(limit, 100))},
			"sort":     {"-timestamp"},
		}
		if q != "" {
			params.Set("query", q)
		}
		n := 0
		for _, s := range g.slugs {
			if id, ok := ids[s]; ok {
				params.Add("project", id)
				n++
			}
		}
		if n == 0 {
			continue
		}
		for _, e := range g.environments {
			params.Add("environment", e)
		}
		var resp struct {
			Data []struct {
				Trace      string  `json:"trace"`
				Name       *string `json:"name"`
				Project    *string `json:"project"`
				Start      float64 `json:"start"`
				End        float64 `json:"end"`
				Duration   float64 `json:"duration"`
				NumSpans   int     `json:"numSpans"`
				NumErrors  int     `json:"numErrors"`
				Breakdowns []struct {
					Project *string `json:"project"`
				} `json:"breakdowns"`
			} `json:"data"`
		}
		if _, err := b.get(ctx, "/traces/", params, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Data {
			d := time.Duration(r.Duration * float64(time.Millisecond))
			if (minD > 0 && d < minD) || (maxD > 0 && d > maxD) {
				continue
			}
			t := TraceSummary{
				TraceID:           normalizeID(r.Trace, 32),
				StartTimeUnixNano: strconv.FormatInt(int64(r.Start*1e6), 10),
				DurationMs:        int64(r.Duration),
			}
			// The trace list may name a root project the viewer is not
			// bound to; never show its name or transaction.
			if r.Project != nil && inGroup(g, *r.Project) {
				t.RootServiceName = *r.Project
				if r.Name != nil {
					t.RootTraceName = *r.Name
				}
				t.ServiceStats = map[string]ServiceStats{*r.Project: {SpanCount: r.NumSpans, ErrorCount: r.NumErrors}}
			} else {
				t.RootServiceName = rootNotReceived
				for _, bd := range r.Breakdowns {
					if bd.Project != nil && inGroup(g, *bd.Project) {
						t.ServiceStats = map[string]ServiceStats{*bd.Project: {}}
						break
					}
				}
			}
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].startNanos() > out[j].startNanos() })
	if len(out) > req.Limit {
		out = out[:req.Limit]
	}
	return out, nil
}

func inGroup(g projectGroup, slug string) bool {
	for _, s := range g.slugs {
		if s == slug {
			return true
		}
	}
	return false
}

type sentryTraceNode struct {
	EventID              string            `json:"event_id"`
	ParentSpanID         *string           `json:"parent_span_id"`
	TransactionID        string            `json:"transaction_id"`
	ProjectSlug          string            `json:"project_slug"`
	ProjectID            json.Number       `json:"project_id"`
	Op                   string            `json:"op"`
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	Transaction          string            `json:"transaction"`
	IsTransaction        bool              `json:"is_transaction"`
	SDKName              string            `json:"sdk_name"`
	StartTimestamp       float64           `json:"start_timestamp"`
	EndTimestamp         float64           `json:"end_timestamp"`
	AdditionalAttributes map[string]any    `json:"additional_attributes"`
	Errors               []map[string]any  `json:"errors"`
	Occurrences          []map[string]any  `json:"occurrences"`
	Children             []sentryTraceNode `json:"children"`
}

func (b *sentryBackend) Trace(ctx context.Context, req *TraceRequest) ([]map[string]any, error) {
	if req.Scope == nil || len(req.Scope.Projects) == 0 {
		return nil, nil
	}
	params := url.Values{"statsPeriod": {sentryPeriod(b.maxLookback)}}
	extra := append([]string{"environment", "release", "span.status"}, b.allowlist...)
	for _, a := range dedupe(extra) {
		params.Add("additional_attributes", a)
	}
	var roots []sentryTraceNode
	if _, err := b.get(ctx, "/trace/"+url.PathEscape(req.TraceID)+"/", params, &roots); err != nil {
		return nil, err
	}
	return b.traceToOTLP(req.TraceID, roots, req.Scope), nil
}

func (b *sentryBackend) traceToOTLP(traceID string, roots []sentryTraceNode, scope *Scope) []map[string]any {
	type group struct {
		resource []any
		spans    []any
	}
	groups := map[string]*group{}
	var order []string
	var walk func(n *sentryTraceNode)
	walk = func(n *sentryTraceNode) {
		for i := range n.Children {
			walk(&n.Children[i])
		}
		env := str(n.AdditionalAttributes["environment"])
		if !allowed(scope, b.org, n.ProjectSlug, env) {
			return
		}
		release := str(n.AdditionalAttributes["release"])
		key := n.ProjectSlug + "\x00" + env + "\x00" + release + "\x00" + n.SDKName
		g, ok := groups[key]
		if !ok {
			res := []any{kvString("service.name", n.ProjectSlug), kvString("sentry.project_id", n.ProjectID.String())}
			if env != "" {
				res = append(res, kvString("deployment.environment", env))
			}
			if release != "" {
				res = append(res, kvString("service.version", release))
			}
			if n.SDKName != "" {
				res = append(res, kvString("telemetry.sdk.name", n.SDKName))
			}
			g = &group{resource: res}
			groups[key] = g
			order = append(order, key)
		}
		g.spans = append(g.spans, b.sentrySpan(traceID, n))
	}
	for i := range roots {
		walk(&roots[i])
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		out = append(out, resourceSpans(groups[k].resource, groups[k].spans))
	}
	return out
}

func (b *sentryBackend) sentrySpan(traceID string, n *sentryTraceNode) map[string]any {
	name := n.Description
	if n.IsTransaction && n.Transaction != "" {
		name = n.Transaction
	}
	if name == "" {
		name = n.Name
	}
	if name == "" {
		name = n.Op
	}
	attrs := []any{kvString("sentry.op", n.Op)}
	if n.Description != "" && n.Description != name {
		attrs = append(attrs, kvString("sentry.description", n.Description))
	}
	if n.Transaction != "" {
		attrs = append(attrs, kvString("sentry.transaction", n.Transaction))
	}
	attrs = append(attrs, kvString("sentry.url", b.publicURL+"/organizations/"+b.org+"/explore/traces/trace/"+traceID+"/"))
	status := map[string]any{"code": statusUnset}
	for _, k := range sortedKeys(n.AdditionalAttributes) {
		v := n.AdditionalAttributes[k]
		switch k {
		case "environment", "release":
			continue
		case "span.status":
			s := str(v)
			switch s {
			case "":
			case "ok":
				status = map[string]any{"code": statusOK}
			default:
				status = map[string]any{"code": statusError, "message": s}
			}
			continue
		}
		// Sentry fills attributes a span does not have with "" or 0.
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if n, ok := v.(json.Number); ok && n.String() == "0" {
			continue
		}
		if kv := kvAny(k, v); kv != nil {
			attrs = append(attrs, kv)
		}
	}
	start := int64(n.StartTimestamp * 1e9)
	end := int64(n.EndTimestamp * 1e9)
	span := map[string]any{
		"traceId":           traceID,
		"spanId":            normalizeID(n.EventID, 16),
		"name":              name,
		"kind":              kindFromOp(n.Op),
		"startTimeUnixNano": strconv.FormatInt(start, 10),
		"endTimeUnixNano":   strconv.FormatInt(end, 10),
		"status":            status,
		"attributes":        attrs,
	}
	if n.ParentSpanID != nil && *n.ParentSpanID != "" {
		span["parentSpanId"] = normalizeID(*n.ParentSpanID, 16)
	}
	var events []any
	for _, e := range append(append([]map[string]any(nil), n.Errors...), n.Occurrences...) {
		ev := []any{}
		if id := str(e["issue_id"]); id != "" {
			ev = append(ev, kvString("sentry.issue_id", id), kvString("sentry.url", b.publicURL+"/organizations/"+b.org+"/issues/"+id+"/"))
		}
		if id := str(e["event_id"]); id != "" {
			ev = append(ev, kvString("sentry.event_id", id))
		}
		events = append(events, map[string]any{"name": "exception", "timeUnixNano": strconv.FormatInt(start, 10), "attributes": ev})
	}
	if events != nil {
		span["events"] = events
	}
	return span
}

func kindFromOp(op string) int {
	switch {
	case op == "http.server" || strings.HasSuffix(op, ".server"):
		return kindServer
	case op == "http.client" || strings.HasPrefix(op, "db") || strings.HasPrefix(op, "cache") || strings.HasSuffix(op, ".client"):
		return kindClient
	case strings.HasPrefix(op, "queue.publish"):
		return kindProducer
	case strings.HasPrefix(op, "queue.process"):
		return kindConsumer
	}
	return kindInternal
}

// sentryPeriod formats a lookback for statsPeriod, e.g. 90d or 12h.
func sentryPeriod(d time.Duration) string {
	if d <= 0 {
		d = 90 * 24 * time.Hour
	}
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(int(d.Hours()+0.5)) + "h"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (b *sentryBackend) Tags(ctx context.Context, req *TagsRequest) (*TagSet, error) {
	if req.Scope == nil || len(req.Scope.Projects) == 0 {
		return &TagSet{}, nil
	}
	// Only allowlisted attributes: Sentry spans carry user data (user.id,
	// user.geo.*) that must not leak through autocomplete.
	return &TagSet{
		Resource:  []string{"service.name", "deployment.environment", "service.version"},
		Span:      append([]string{"sentry.op"}, b.allowlist...),
		Intrinsic: []string{"duration", "name", "status", "traceDuration"},
	}, nil
}

func (b *sentryBackend) TagValues(ctx context.Context, req *TagValuesRequest) ([]TagValue, error) {
	if req.Scope == nil || len(req.Scope.Projects) == 0 {
		return nil, nil
	}
	t := req.Tag
	var key string
	switch {
	case t.Is(ScopeResource, "service.name"):
		var out []TagValue
		for _, g := range groupProjects(req.Scope, b.org, nil, nil) {
			for _, s := range g.slugs {
				out = append(out, TagValue{"string", s})
			}
		}
		return out, nil
	case t.Is(ScopeResource, "deployment.environment"):
		key = "environment"
	case t.Is(ScopeResource, "service.version"):
		key = "release"
	case t.Is(ScopeIntrinsic, "name"):
		key = b.spanName
	case t.Is(ScopeSpan, "sentry.op"):
		key = "span.op"
	case t.Is(ScopeIntrinsic, "status"):
		return []TagValue{{"keyword", "error"}, {"keyword", "ok"}, {"keyword", "unset"}}, nil
	default:
		for _, a := range b.allowlist {
			if t.Is(ScopeSpan, a) {
				key = a
			}
		}
	}
	if key == "" {
		return nil, nil
	}
	ids, err := b.projectIDs(ctx)
	if err != nil {
		return nil, err
	}
	start, end := defaultWindow(req.Start, req.End)
	// One request per environment group, so values from environments a
	// project is not bound for never show up.
	seen := map[string]bool{}
	var out []TagValue
	for _, g := range groupProjects(req.Scope, b.org, nil, nil) {
		params := url.Values{
			"itemType": {"spans"}, "attributeType": {"string"},
			"start": {start.UTC().Format("2006-01-02T15:04:05")},
			"end":   {end.UTC().Format("2006-01-02T15:04:05")},
		}
		n := 0
		for _, s := range g.slugs {
			if id, ok := ids[s]; ok {
				params.Add("project", id)
				n++
			}
		}
		if n == 0 {
			continue
		}
		envs := map[string]bool{}
		for _, e := range g.environments {
			params.Add("environment", e)
			envs[e] = true
		}
		var values []struct {
			Value string `json:"value"`
		}
		if _, err := b.get(ctx, "/trace-items/attributes/"+url.PathEscape(key)+"/values/", params, &values); err != nil {
			return nil, err
		}
		for _, v := range values {
			if key == "environment" && len(envs) > 0 && !envs[v.Value] {
				continue
			}
			if !seen[v.Value] {
				seen[v.Value] = true
				out = append(out, TagValue{"string", v.Value})
			}
		}
	}
	return out, nil
}
