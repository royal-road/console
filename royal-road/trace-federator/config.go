package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is read from a JSON file (a ConfigMap key). See README.md.
type Config struct {
	Listen string `json:"listen"`
	// Namespace shown for the plugin's "Tempo instances"; any label works.
	InstanceNamespace string `json:"instanceNamespace"`
	// AllView is the name of the instance that queries every backend.
	AllView    string           `json:"allView"`
	Kubernetes KubernetesConfig `json:"kubernetes"`
	Authz      AuthzConfig      `json:"authz"`
	Backends   []BackendConfig  `json:"backends"`
	// Profiles, when set, serves /profiles/* from Pyroscope.
	Profiles *ProfilesConfig `json:"profiles"`
}

// ProfilesConfig is the Pyroscope passthrough (profiles.go).
type ProfilesConfig struct {
	URL     string   `json:"url"`
	Timeout Duration `json:"timeout"`
	// MaxWindow bounds every call and each side of a Diff. Keep it at or
	// under Pyroscope's max_query_length, which an overflowing window can
	// slip past.
	MaxWindow Duration `json:"maxWindow"`
	// MaxNodes is the ceiling for maxNodes; larger requests are clamped.
	MaxNodes int64 `json:"maxNodes"`
	// LegacyServices lists, per namespace, the service_name values whose
	// profiles carry no namespace label yet but are known to come from that
	// namespace. Remove an entry once the service's profiles have carried
	// the namespace label for the store's retention.
	LegacyServices map[string][]string `json:"legacyServices"`
}

type KubernetesConfig struct {
	// APIServer, TokenFile and CAFile default to the in-cluster values.
	APIServer          string `json:"apiServer"`
	TokenFile          string `json:"tokenFile"`
	CAFile             string `json:"caFile"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	// BindingsRefresh is how often SentryProjectBindings are re-listed.
	BindingsRefresh Duration `json:"bindingsRefresh"`
}

type AuthzConfig struct {
	// Group and Resource of the virtual resource checked for "rbac"
	// backends: verb get, name = backend name, cluster scope.
	Group    string   `json:"group"`
	Resource string   `json:"resource"`
	CacheTTL Duration `json:"cacheTTL"`
}

type BackendConfig struct {
	Name string `json:"name"`
	Type string `json:"type"` // tempo | signoz | sentry
	URL  string `json:"url"`
	// Access is "rbac" (virtual-resource check, the default for tempo and
	// signoz) or "sentry-bindings" (required for sentry).
	Access        string   `json:"access"`
	Priority      int      `json:"priority"`
	SearchTimeout Duration `json:"searchTimeout"`
	TraceTimeout  Duration `json:"traceTimeout"`
	// MaxLookback is the store's retention; windows entirely older are
	// skipped and trace-by-ID looks back this far.
	MaxLookback Duration `json:"maxLookback"`

	BearerTokenFile string `json:"bearerTokenFile"` // tempo
	APIKeyFile      string `json:"apiKeyFile"`      // signoz

	TokenFile             string   `json:"tokenFile"`             // sentry
	Organization          string   `json:"organization"`          // sentry
	PublicURL             string   `json:"publicURL"`             // sentry links
	SpanNameField         string   `json:"spanNameField"`         // sentry, default span.name
	AttributeAllowlist    []string `json:"attributeAllowlist"`    // sentry
	UnsupportedAttributes []string `json:"unsupportedAttributes"` // sentry
}

// Duration is a time.Duration that reads "8s"-style strings, plus "Nd" days.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		d.Duration = time.Duration(n) * 24 * time.Hour
		return nil
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.InstanceNamespace == "" {
		c.InstanceNamespace = "tracing"
	}
	if c.AllView == "" {
		c.AllView = "all-stores"
	}
	if c.Authz.Group == "" {
		c.Authz.Group = "tracing.royalroad.com"
	}
	if c.Authz.Resource == "" {
		c.Authz.Resource = "traces"
	}
	if c.Authz.CacheTTL.Duration == 0 {
		c.Authz.CacheTTL.Duration = time.Minute
	}
	if c.Kubernetes.BindingsRefresh.Duration == 0 {
		c.Kubernetes.BindingsRefresh.Duration = 30 * time.Second
	}
	names := map[string]bool{c.AllView: true}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Name == "" || names[b.Name] {
			return nil, fmt.Errorf("backend %d: missing or duplicate name %q", i, b.Name)
		}
		names[b.Name] = true
		switch b.Type {
		case "tempo", "signoz":
			if b.Access == "" {
				b.Access = "rbac"
			}
		case "sentry":
			if b.Access == "" {
				b.Access = "sentry-bindings"
			}
			if b.Access != "sentry-bindings" {
				return nil, fmt.Errorf("backend %s: sentry needs access sentry-bindings", b.Name)
			}
		default:
			return nil, fmt.Errorf("backend %s: unknown type %q", b.Name, b.Type)
		}
		if b.Access != "rbac" && b.Access != "sentry-bindings" {
			return nil, fmt.Errorf("backend %s: unknown access %q", b.Name, b.Access)
		}
		if b.SearchTimeout.Duration == 0 {
			b.SearchTimeout.Duration = 8 * time.Second
		}
		if b.TraceTimeout.Duration == 0 {
			b.TraceTimeout.Duration = 5 * time.Second
		}
		if b.MaxLookback.Duration == 0 {
			b.MaxLookback.Duration = 30 * 24 * time.Hour
		}
		if b.URL == "" {
			return nil, fmt.Errorf("backend %s: url is required", b.Name)
		}
	}
	if len(c.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	if p := c.Profiles; p != nil {
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("profiles: %w", err)
		}
	}
	return &c, nil
}

func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("a credential file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func (p *ProfilesConfig) validate() error {
	if p.URL == "" {
		return fmt.Errorf("url is required")
	}
	p.URL = strings.TrimRight(p.URL, "/")
	if p.Timeout.Duration == 0 {
		p.Timeout.Duration = 20 * time.Second
	}
	if p.MaxWindow.Duration == 0 {
		p.MaxWindow.Duration = 24 * time.Hour
	}
	if p.MaxNodes == 0 {
		p.MaxNodes = 16384
	}
	if p.MaxNodes < 1 || p.Timeout.Duration < 0 || p.MaxWindow.Duration < 0 {
		return fmt.Errorf("timeout, maxWindow and maxNodes must be positive")
	}
	// A service listed under two namespaces would show its unlabelled
	// profiles in both.
	listedIn := map[string]string{}
	for ns, services := range p.LegacyServices {
		if !isNamespace(ns) {
			return fmt.Errorf("legacyServices: %q is not a namespace name", ns)
		}
		for _, s := range services {
			if s == "" {
				return fmt.Errorf("legacyServices[%s]: empty service name", ns)
			}
			if other, ok := listedIn[s]; ok && other != ns {
				return fmt.Errorf("legacyServices: %q is listed under both %s and %s", s, other, ns)
			}
			listedIn[s] = ns
		}
	}
	return nil
}
