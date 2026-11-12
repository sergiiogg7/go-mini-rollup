// Package metrics is a tiny Prometheus-compatible registry (counters, gauges and histograms).
// Components depend on the Sink interface, not on this implementation.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Sink is what the components report to.
type Sink interface {
	// Inc increments a counter. kv are label key/value pairs.
	Inc(name string, kv ...string)
	// Set sets a gauge.
	Set(name string, v float64)
	// Observe records one value (seconds) in a histogram with DefaultBuckets.
	Observe(name string, v float64)
}

// Nop discards everything.
type Nop struct{}

func (Nop) Inc(string, ...string)   {}
func (Nop) Set(string, float64)     {}
func (Nop) Observe(string, float64) {}

// DefaultBuckets are the upper bounds (seconds) of every histogram: executing a block is expected
// to take from well under a millisecond (empty epoch block) to a few hundred milliseconds.
var DefaultBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

type series struct {
	name, labels string
	gauge        bool
	value        float64
}

type histogram struct {
	counts []uint64 // counts[i] is the number of values in (bounds[i-1], bounds[i]]
	inf    uint64   // values above the last bound
	sum    float64
	count  uint64
}

// Registry stores the series and serves them in the Prometheus text format.
type Registry struct {
	mu   sync.Mutex
	s    map[string]*series
	h    map[string]*histogram
	help map[string]string
}

func NewRegistry() *Registry {
	return &Registry{s: map[string]*series{}, h: map[string]*histogram{}, help: map[string]string{}}
}

func labelString(kv []string) string {
	if len(kv) < 2 {
		return ""
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", kv[i], kv[i+1]))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

// Describe attaches a HELP text to a metric name.
func (r *Registry) Describe(name, help string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.help[name] = help
}

// Init creates a counter series at 0 so that it is exported before its first increment. Without
// it Prometheus' rate() and increase() miss the first event of a series that appears at 1.
func (r *Registry) Init(name string, kv ...string) {
	l := labelString(kv)
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := name + l; r.s[k] == nil {
		r.s[k] = &series{name: name, labels: l}
	}
}

func (r *Registry) Inc(name string, kv ...string) {
	l := labelString(kv)
	r.mu.Lock()
	defer r.mu.Unlock()
	k := name + l
	if r.s[k] == nil {
		r.s[k] = &series{name: name, labels: l}
	}
	r.s[k].value++
}

func (r *Registry) Set(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s[name] = &series{name: name, gauge: true, value: v}
}

func (r *Registry) Observe(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.h[name]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(DefaultBuckets))}
		r.h[name] = h
	}
	h.sum += v
	h.count++
	for i, ub := range DefaultBuckets {
		if v <= ub {
			h.counts[i]++
			return
		}
	}
	h.inf++
}

// Get returns a series value (0 if absent). Used by tests.
func (r *Registry) Get(name string, kv ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.s[name+labelString(kv)]; s != nil {
		return s.value
	}
	return 0
}

// Count returns the number of observations of a histogram (0 if absent). Used by tests.
func (r *Registry) Count(name string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.h[name]; h != nil {
		return h.count
	}
	return 0
}

// Render writes the exposition text.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.s))
	for k := range r.s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	header := func(name, typ string) {
		if h, ok := r.help[name]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, h)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typ)
	}
	typed := map[string]bool{}
	for _, k := range keys {
		s := r.s[k]
		if !typed[s.name] {
			t := "counter"
			if s.gauge {
				t = "gauge"
			}
			header(s.name, t)
			typed[s.name] = true
		}
		fmt.Fprintf(&b, "%s%s %v\n", s.name, s.labels, s.value)
	}
	names := make([]string, 0, len(r.h))
	for n := range r.h {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := r.h[n]
		header(n, "histogram")
		var cum uint64
		for i, ub := range DefaultBuckets {
			cum += h.counts[i]
			fmt.Fprintf(&b, "%s_bucket{le=\"%v\"} %d\n", n, ub, cum)
		}
		fmt.Fprintf(&b, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %v\n%s_count %d\n", n, cum+h.inf, n, h.sum, n, h.count)
	}
	return b.String()
}

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(r.Render()))
	})
}
