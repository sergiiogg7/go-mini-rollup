// Package metrics is a tiny Prometheus-compatible registry (counters and gauges with labels).
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
}

// Nop discards everything.
type Nop struct{}

func (Nop) Inc(string, ...string) {}
func (Nop) Set(string, float64)   {}

type series struct {
	name, labels string
	gauge        bool
	value        float64
}

// Registry stores the series and serves them in the Prometheus text format.
type Registry struct {
	mu sync.Mutex
	s  map[string]*series
}

func NewRegistry() *Registry { return &Registry{s: map[string]*series{}} }

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

// Get returns a series value (0 if absent). Used by tests.
func (r *Registry) Get(name string, kv ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.s[name+labelString(kv)]; s != nil {
		return s.value
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
	typed := map[string]bool{}
	for _, k := range keys {
		s := r.s[k]
		if !typed[s.name] {
			t := "counter"
			if s.gauge {
				t = "gauge"
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n", s.name, t)
			typed[s.name] = true
		}
		fmt.Fprintf(&b, "%s%s %v\n", s.name, s.labels, s.value)
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
