// Package metrics is a small Prometheus-compatible metrics registry: counters,
// gauges and histograms with labels, rendered in the text exposition format.
// It has no dependencies. A nil *Registry, and the metrics made from it, accept
// every call and do nothing, so code can be instrumented without checking
// whether metrics are switched on.
package metrics

import (
	"crypto/subtle"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxSeries bounds the number of label combinations of one metric, so that a
// label fed from outside cannot make the registry grow without limit.
const maxSeries = 500

// Registry holds metrics and renders them.
type Registry struct {
	mu      sync.Mutex
	metrics []renderer
	names   map[string]bool
}

type renderer interface {
	name() string
	render(sb *strings.Builder)
}

// New returns an empty registry.
func New() *Registry { return &Registry{names: map[string]bool{}} }

func (r *Registry) add(m renderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[m.name()] {
		panic("metrics: duplicate metric " + m.name())
	}
	r.names[m.name()] = true
	r.metrics = append(r.metrics, m)
}

// Render returns the metrics in the Prometheus text format.
func (r *Registry) Render() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	ms := append([]renderer(nil), r.metrics...)
	r.mu.Unlock()
	sort.Slice(ms, func(i, j int) bool { return ms[i].name() < ms[j].name() })
	var sb strings.Builder
	for _, m := range ms {
		m.render(&sb)
	}
	return sb.String()
}

// Handler serves the metrics. With a token, requests must carry it as a bearer token.
func (r *Registry) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(r.Render()))
	})
}

// --- label handling shared by the metric kinds ---

type family struct {
	nm, help, kind string
	labels         []string
}

func (f *family) name() string { return f.nm }

func (f *family) header(sb *strings.Builder) {
	fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s %s\n", f.nm, escapeHelp(f.help), f.nm, f.kind)
}

func (f *family) key(values []string) (string, bool) {
	if len(values) != len(f.labels) {
		return "", false // a programming error: ignore rather than crash a chat
	}
	var sb strings.Builder
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(0)
		}
		sb.WriteString(v)
	}
	return sb.String(), true
}

func (f *family) labelString(values []string, extra ...string) string {
	if len(f.labels) == 0 && len(extra) == 0 {
		return ""
	}
	var parts []string
	for i, l := range f.labels {
		parts = append(parts, l+`="`+escapeLabel(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escapeLabel(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func escapeHelp(s string) string { return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// --- counters ---

// Counter only goes up.
type Counter struct {
	family
	mu     sync.Mutex
	values map[string]*cell
	over   float64 // increments dropped because the metric had too many series
}

type cell struct {
	labels []string
	v      float64
}

// Counter registers a counter with the given label names.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	if r == nil {
		return nil
	}
	c := &Counter{family: family{nm: name, help: help, kind: "counter", labels: labels}, values: map[string]*cell{}}
	r.add(c)
	return c
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v (which must not be negative).
func (c *Counter) Add(v float64, labelValues ...string) {
	if c == nil || v < 0 {
		return
	}
	key, ok := c.key(labelValues)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.values[key]
	if cl == nil {
		if len(c.values) >= maxSeries {
			c.over += v
			return
		}
		cl = &cell{labels: append([]string(nil), labelValues...)}
		c.values[key] = cl
	}
	cl.v += v
}

func (c *Counter) render(sb *strings.Builder) {
	c.header(sb)
	c.mu.Lock()
	defer c.mu.Unlock()
	cells := sortedCells(c.values)
	if len(cells) == 0 && len(c.labels) == 0 {
		fmt.Fprintf(sb, "%s 0\n", c.nm)
	}
	for _, cl := range cells {
		fmt.Fprintf(sb, "%s%s %s\n", c.nm, c.labelString(cl.labels), formatFloat(cl.v))
	}
	if c.over > 0 {
		fmt.Fprintf(sb, "%s_dropped_total %s\n", c.nm, formatFloat(c.over))
	}
}

func sortedCells(m map[string]*cell) []*cell {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*cell, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// --- gauges computed when scraped ---

// Sample is one value of a gauge function, with its label values.
type Sample struct {
	Labels []string
	Value  float64
}

type gaugeFunc struct {
	family
	f func() []Sample
}

// GaugeFunc registers a gauge whose values are computed at every scrape.
func (r *Registry) GaugeFunc(name, help string, labels []string, f func() []Sample) {
	if r == nil {
		return
	}
	r.add(&gaugeFunc{family: family{nm: name, help: help, kind: "gauge", labels: labels}, f: f})
}

func (g *gaugeFunc) render(sb *strings.Builder) {
	g.header(sb)
	samples := g.f()
	sort.Slice(samples, func(i, j int) bool {
		return strings.Join(samples[i].Labels, "\x00") < strings.Join(samples[j].Labels, "\x00")
	})
	for _, s := range samples {
		if len(s.Labels) != len(g.labels) {
			continue
		}
		fmt.Fprintf(sb, "%s%s %s\n", g.nm, g.labelString(s.Labels), formatFloat(s.Value))
	}
}

// Gauge1 registers a gauge without labels computed at every scrape.
func (r *Registry) Gauge1(name, help string, f func() float64) {
	r.GaugeFunc(name, help, nil, func() []Sample { return []Sample{{Value: f()}} })
}

// --- histograms ---

// Histogram counts observations in buckets.
type Histogram struct {
	family
	buckets []float64
	mu      sync.Mutex
	series  map[string]*hseries
}

type hseries struct {
	labels []string
	counts []uint64 // per bucket, not cumulative
	sum    float64
	n      uint64
}

// DefBuckets suit request durations from milliseconds to minutes.
var DefBuckets = []float64{0.05, 0.25, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Histogram registers a histogram; buckets must be ascending (nil uses DefBuckets).
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if r == nil {
		return nil
	}
	if buckets == nil {
		buckets = DefBuckets
	}
	h := &Histogram{family: family{nm: name, help: help, kind: "histogram", labels: labels}, buckets: buckets, series: map[string]*hseries{}}
	r.add(h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	if h == nil {
		return
	}
	key, ok := h.key(labelValues)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[key]
	if s == nil {
		if len(h.series) >= maxSeries {
			return
		}
		s = &hseries{labels: append([]string(nil), labelValues...), counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	s.sum += v
	s.n++
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
}

// Since observes the seconds elapsed since start.
func (h *Histogram) Since(start time.Time, labelValues ...string) {
	h.Observe(time.Since(start).Seconds(), labelValues...)
}

func (h *Histogram) render(sb *strings.Builder) {
	h.header(sb)
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(sb, "%s_bucket%s %d\n", h.nm, h.labelString(s.labels, "le", formatFloat(b)), cum)
		}
		fmt.Fprintf(sb, "%s_bucket%s %d\n", h.nm, h.labelString(s.labels, "le", "+Inf"), s.n)
		fmt.Fprintf(sb, "%s_sum%s %s\n", h.nm, h.labelString(s.labels), formatFloat(s.sum))
		fmt.Fprintf(sb, "%s_count%s %d\n", h.nm, h.labelString(s.labels), s.n)
	}
}

// RegisterRuntime adds process metrics: Go runtime figures, start time and build version.
func (r *Registry) RegisterRuntime(version string) {
	if r == nil {
		return
	}
	start := time.Now()
	r.Gauge1("process_start_time_seconds", "Start time of the process, in Unix seconds.", func() float64 { return float64(start.Unix()) })
	r.Gauge1("go_goroutines", "Number of goroutines.", func() float64 { return float64(runtime.NumGoroutine()) })
	mem := func(f func(*runtime.MemStats) float64) func() float64 {
		return func() float64 { var m runtime.MemStats; runtime.ReadMemStats(&m); return f(&m) }
	}
	r.Gauge1("go_memstats_alloc_bytes", "Bytes of heap in use.", mem(func(m *runtime.MemStats) float64 { return float64(m.Alloc) }))
	r.Gauge1("go_memstats_sys_bytes", "Bytes obtained from the system.", mem(func(m *runtime.MemStats) float64 { return float64(m.Sys) }))
	r.Gauge1("go_gc_cycles_completed", "Completed garbage collection cycles.", mem(func(m *runtime.MemStats) float64 { return float64(m.NumGC) }))
	r.GaugeFunc("jannyq_build_info", "Build information; the value is always 1.", []string{"version", "go"},
		func() []Sample { return []Sample{{Labels: []string{version, runtime.Version()}, Value: 1}} })
}
