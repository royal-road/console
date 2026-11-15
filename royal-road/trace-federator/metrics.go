package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// A deliberately small Prometheus text-format registry, so the module needs
// no dependencies.

type counterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

func newCounter(name, help string, labels ...string) *counterVec {
	c := &counterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	registry = append(registry, c)
	return c
}

func (c *counterVec) inc(labelValues ...string) {
	c.mu.Lock()
	c.values[strings.Join(labelValues, "\x00")]++
	c.mu.Unlock()
}

func (c *counterVec) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, k := range sortedMapKeys(c.values) {
		fmt.Fprintf(w, "%s%s %v\n", c.name, labelString(c.labels, strings.Split(k, "\x00")), c.values[k])
	}
}

type histogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histogram
}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(name, help string, buckets []float64, labels ...string) *histogramVec {
	h := &histogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histogram{}}
	registry = append(registry, h)
	return h
}

func (h *histogramVec) observe(v float64, labelValues ...string) {
	k := strings.Join(labelValues, "\x00")
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[k]
	if !ok {
		s = &histogram{counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

func (h *histogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		lv := strings.Split(k, "\x00")
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(append(h.labels, "le"), append(lv, strconv.FormatFloat(b, 'g', -1, 64))), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(append(h.labels, "le"), append(lv, "+Inf")), s.count)
		fmt.Fprintf(w, "%s_sum%s %v\n%s_count%s %d\n", h.name, labelString(h.labels, lv), s.sum, h.name, labelString(h.labels, lv), s.count)
	}
}

type metric interface{ write(io.Writer) }

var registry []metric

func writeMetrics(w io.Writer) {
	for _, m := range registry {
		m.write(w)
	}
}

func labelString(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts[i] = n + "=" + strconv.Quote(v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	metricRequests        = newCounter("tracefed_requests_total", "HTTP requests by endpoint and status code.", "endpoint", "code")
	metricBackendRequests = newCounter("tracefed_backend_requests_total", "Backend calls by store and outcome (ok, unsupported, error, timeout).", "store", "outcome")
	metricBackendDuration = newHistogram("tracefed_backend_duration_seconds", "Backend call latency.", []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}, "store")
	metricPartial         = newCounter("tracefed_partial_responses_total", "Responses missing results from at least one store.", "endpoint")
	metricDedupedSpans    = newCounter("tracefed_deduplicated_spans_total", "Spans dropped from a trace because a higher-priority store had the same span ID.", "store")
	metricAuthzReviews    = newCounter("tracefed_access_reviews_total", "SelfSubjectAccessReviews made (cache misses) by result.", "allowed")
)
