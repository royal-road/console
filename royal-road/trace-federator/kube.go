package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// kubeClient is a minimal Kubernetes API client: SelfSubjectAccessReviews
// with the viewer's own token, and listing SentryProjectBindings with the
// federator's ServiceAccount token. Using the viewer's token for access
// reviews means the federator needs no tokenreviews or subjectaccessreviews
// permission, and the API server authenticates the token (Dex OIDC included).
type kubeClient struct {
	server    string
	tokenFile string
	client    *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

const (
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

func newKubeClient(cfg KubernetesConfig) (*kubeClient, error) {
	server := cfg.APIServer
	if server == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, fmt.Errorf("kubernetes.apiServer is not set and not running in a cluster")
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		server = "https://" + host + ":" + port
	}
	tokenFile := cfg.TokenFile
	if tokenFile == "" {
		tokenFile = inClusterTokenFile
	}
	caFile := cfg.CAFile
	if caFile == "" && cfg.APIServer == "" {
		caFile = inClusterCAFile
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // opt-in for local development
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &kubeClient{
		server:    strings.TrimRight(server, "/"),
		tokenFile: tokenFile,
		client:    &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}},
	}, nil
}

// saToken re-reads the projected ServiceAccount token every minute; kubelet
// rotates it.
func (k *kubeClient) saToken() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Since(k.tokenAt) < time.Minute {
		return k.token, nil
	}
	t, err := readSecretFile(k.tokenFile)
	if err != nil {
		return "", err
	}
	k.token, k.tokenAt = t, time.Now()
	return t, nil
}

func (k *kubeClient) do(ctx context.Context, method, path, bearer string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.server+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(data), 200))
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

// ResourceAttributes of a SelfSubjectAccessReview.
type ResourceAttributes struct {
	Namespace string `json:"namespace,omitempty"`
	Verb      string `json:"verb"`
	Group     string `json:"group,omitempty"`
	Resource  string `json:"resource"`
	Name      string `json:"name,omitempty"`
}

func (a ResourceAttributes) key() string {
	return a.Verb + " " + a.Group + "/" + a.Resource + " " + a.Namespace + "/" + a.Name
}

// errUnauthenticated means the API server rejected the viewer's token.
var errUnauthenticated = fmt.Errorf("token rejected by the API server")

func (k *kubeClient) canI(ctx context.Context, userToken string, attrs ResourceAttributes) (bool, error) {
	body := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec":       map[string]any{"resourceAttributes": attrs},
	}
	var out struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	code, err := k.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", userToken, body, &out)
	if code == http.StatusUnauthorized {
		return false, errUnauthenticated
	}
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

// SentryProjectBinding (observability.royalroad.com/v1alpha1, cluster
// scoped). See royal-road/deploy/crds/observability.royalroad.com_sentryprojectbindings.yaml.
type SentryProjectBinding struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Sentry struct {
			Organization string   `json:"organization"`
			Project      string   `json:"project"`
			Environments []string `json:"environments"`
		} `json:"sentry"`
		Targets []BindingTarget `json:"targets"`
	} `json:"spec"`
}

type BindingTarget struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
}

// workloadResources maps the target kinds the CRD allows to API resources.
var workloadResources = map[string][2]string{
	"Deployment":  {"apps", "deployments"},
	"StatefulSet": {"apps", "statefulsets"},
	"DaemonSet":   {"apps", "daemonsets"},
	"CronJob":     {"batch", "cronjobs"},
	"Job":         {"batch", "jobs"},
}

// accessCheck is the review that grants a target: get on the workload, or,
// for a whole namespace, get on pods there.
func (t BindingTarget) accessCheck() (ResourceAttributes, bool) {
	if t.Kind == "" {
		return ResourceAttributes{Namespace: t.Namespace, Verb: "get", Resource: "pods"}, true
	}
	gr, ok := workloadResources[t.Kind]
	if !ok || t.Name == "" {
		return ResourceAttributes{}, false
	}
	return ResourceAttributes{Namespace: t.Namespace, Verb: "get", Group: gr[0], Resource: gr[1], Name: t.Name}, true
}

const bindingsPath = "/apis/observability.royalroad.com/v1alpha1/sentryprojectbindings"

// bindingStore keeps the current SentryProjectBindings, re-listed
// periodically with the ServiceAccount token.
type bindingStore struct {
	kube *kubeClient
	mu   sync.RWMutex
	list []SentryProjectBinding
	ok   bool
}

func (s *bindingStore) refresh(ctx context.Context) error {
	tok, err := s.kube.saToken()
	if err != nil {
		return err
	}
	var out struct {
		Items []SentryProjectBinding `json:"items"`
	}
	code, err := s.kube.do(ctx, http.MethodGet, bindingsPath, tok, nil, &out)
	if code == http.StatusNotFound {
		out.Items, err = nil, nil // CRD not installed: no Sentry access for anyone
	}
	if err != nil {
		return err
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Metadata.Name < out.Items[j].Metadata.Name })
	s.mu.Lock()
	s.list, s.ok = out.Items, true
	s.mu.Unlock()
	return nil
}

func (s *bindingStore) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.refresh(ctx); err != nil {
			slog.Warn("listing SentryProjectBindings", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *bindingStore) snapshot() ([]SentryProjectBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.list, s.ok
}

// authorizer answers "may this viewer read backend X" and "which Sentry
// projects may this viewer read", caching per token.
type authorizer struct {
	kube     *kubeClient
	bindings *bindingStore
	group    string
	resource string
	ttl      time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	allowed bool
	err     error
	at      time.Time
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:16])
}

func (a *authorizer) check(ctx context.Context, token string, attrs ResourceAttributes) (bool, error) {
	key := tokenHash(token) + " " + attrs.key()
	a.mu.Lock()
	if e, ok := a.cache[key]; ok && time.Since(e.at) < a.ttl {
		a.mu.Unlock()
		return e.allowed, e.err
	}
	a.mu.Unlock()
	allowed, err := a.kube.canI(ctx, token, attrs)
	metricAuthzReviews.inc(fmt.Sprint(allowed))
	if err != nil && err != errUnauthenticated {
		return false, err // don't cache transient errors
	}
	a.mu.Lock()
	if len(a.cache) > 50000 {
		a.cache = map[string]cacheEntry{}
	}
	a.cache[key] = cacheEntry{allowed, err, time.Now()}
	a.mu.Unlock()
	return allowed, err
}

// canReadBackend checks get on the virtual resource for an "rbac" backend.
func (a *authorizer) canReadBackend(ctx context.Context, token, backend string) (bool, error) {
	return a.check(ctx, token, ResourceAttributes{Verb: "get", Group: a.group, Resource: a.resource, Name: backend})
}

// sentryScope returns the Sentry projects and environments the viewer may
// read: every bound project for those who can list SentryProjectBindings
// (cluster admins: nobody else gets RBAC on them), otherwise the projects of
// bindings with at least one target the viewer can get.
func (a *authorizer) sentryScope(ctx context.Context, token string) (*Scope, error) {
	list, ok := a.bindings.snapshot()
	if !ok {
		return nil, fmt.Errorf("SentryProjectBindings not loaded yet")
	}
	admin, err := a.check(ctx, token, ResourceAttributes{Verb: "list", Group: "observability.royalroad.com", Resource: "sentryprojectbindings"})
	if err != nil {
		return nil, err
	}
	type result struct {
		ok  bool
		err error
	}
	checks := map[string]ResourceAttributes{}
	if !admin {
		for _, b := range list {
			for _, t := range b.Spec.Targets {
				if attrs, ok := t.accessCheck(); ok {
					checks[attrs.key()] = attrs
				}
			}
		}
	}
	results := map[string]result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for k, attrs := range checks {
		wg.Add(1)
		go func(k string, attrs ResourceAttributes) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, err := a.check(ctx, token, attrs)
			mu.Lock()
			results[k] = result{ok, err}
			mu.Unlock()
		}(k, attrs)
	}
	wg.Wait()
	merged := map[string]*ProjectScope{}
	allEnvs := map[string]bool{}
	var order []string
	for _, b := range list {
		granted := admin
		for _, t := range b.Spec.Targets {
			if granted {
				break
			}
			if attrs, ok := t.accessCheck(); ok {
				r := results[attrs.key()]
				if r.err != nil {
					return nil, r.err
				}
				granted = r.ok
			}
		}
		if !granted {
			continue
		}
		s := b.Spec.Sentry
		key := s.Organization + "/" + s.Project
		p, ok := merged[key]
		if !ok {
			p = &ProjectScope{Organization: s.Organization, Project: s.Project}
			merged[key] = p
			order = append(order, key)
		}
		if len(s.Environments) == 0 {
			allEnvs[key] = true
		}
		p.Environments = append(p.Environments, s.Environments...)
	}
	scope := &Scope{}
	for _, k := range order {
		p := merged[k]
		if allEnvs[k] {
			p.Environments = nil
		} else {
			p.Environments = dedupe(p.Environments)
		}
		scope.Projects = append(scope.Projects, *p)
	}
	return scope, nil
}
