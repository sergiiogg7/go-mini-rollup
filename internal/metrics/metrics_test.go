package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryRendersPrometheusText(t *testing.T) {
	r := NewRegistry()
	r.Inc("x_total")
	r.Inc("x_total")
	r.Inc("rejected_total", "reason", "decode", "a", "b")
	r.Set("head", 7)
	require.Equal(t, 2.0, r.Get("x_total"))
	require.Equal(t, 1.0, r.Get("rejected_total", "reason", "decode", "a", "b"))
	out := r.Render()
	for _, want := range []string{"# TYPE x_total counter", "x_total 2", `rejected_total{a="b",reason="decode"} 1`, "# TYPE head gauge", "head 7"} {
		require.Contains(t, out, want)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain"))
	var n Sink = Nop{}
	n.Inc("ignored")
	n.Set("ignored", 1)
}
