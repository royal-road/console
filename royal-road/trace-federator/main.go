// trace-federator serves the Tempo HTTP API over several trace stores
// (Tempo-compatible stores such as VictoriaTraces, SigNoz and Sentry), and the
// backend API of the distributed-tracing console plugin. See README.md.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "/etc/trace-federator/config.json", "configuration file")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("loading configuration", "err", err)
		os.Exit(1)
	}
	kube, err := newKubeClient(cfg.Kubernetes)
	if err != nil {
		slog.Error("Kubernetes client", "err", err)
		os.Exit(1)
	}
	fed, err := newFederator(cfg, kube)
	if err != nil {
		slog.Error("backends", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go fed.authz.bindings.run(ctx, cfg.Kubernetes.BindingsRefresh.Duration)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           logRequests((&server{fed: fed, cfg: cfg}).routes()),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", cfg.Listen, "views", fed.order)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}

func newFederator(cfg *Config, kube *kubeClient) (*federator, error) {
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}}
	var stores []*store
	for _, bc := range cfg.Backends {
		var b Backend
		var err error
		switch bc.Type {
		case "tempo":
			b, err = newTempoBackend(bc, client)
		case "signoz":
			b, err = newSignozBackend(bc, client)
		case "sentry":
			b, err = newSentryBackend(bc, client)
		}
		if err != nil {
			return nil, err
		}
		stores = append(stores, &store{Backend: b, cfg: bc})
	}
	sort.SliceStable(stores, func(i, j int) bool { return stores[i].cfg.Priority > stores[j].cfg.Priority })
	f := &federator{
		stores: stores,
		authz: &authorizer{
			kube:     kube,
			bindings: &bindingStore{kube: kube},
			group:    cfg.Authz.Group,
			resource: cfg.Authz.Resource,
			ttl:      cfg.Authz.CacheTTL.Duration,
			cache:    map[string]cacheEntry{},
		},
		views: map[string][]*store{cfg.AllView: stores},
		order: []string{cfg.AllView},
	}
	if cfg.Profiles != nil {
		f.profiles = &profilesProxy{cfg: cfg.Profiles, client: client}
	}
	for _, s := range stores {
		f.views[s.cfg.Name] = []*store{s}
		f.order = append(f.order, s.cfg.Name)
	}
	return f, nil
}
