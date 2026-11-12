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
	n.Observe("ignored", 1)
}

func TestHistogramInitAndHelp(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r)
	r.Observe(BlockBuild, 0.002)
	r.Observe(BlockBuild, 0.3)
	r.Observe(BlockBuild, 5) // above the last bucket
	require.EqualValues(t, 3, r.Count(BlockBuild))
	out := r.Render()
	for _, want := range []string{
		"# HELP rollup_block_build_seconds Seconds the sequencer spends executing one block on the EVM.",
		"# TYPE rollup_block_build_seconds histogram",
		`rollup_block_build_seconds_bucket{le="0.0025"} 1`,
		`rollup_block_build_seconds_bucket{le="0.5"} 2`,
		`rollup_block_build_seconds_bucket{le="+Inf"} 3`,
		"rollup_block_build_seconds_count 3",
		`rollup_batch_rejected_total{reason="origin_hash"} 0`, // exported before the first event
		"rollup_l1_reorg_total 0",
	} {
		require.Contains(t, out, want)
	}
	r.Inc(L1Reorgs)
	require.EqualValues(t, 1, r.Get(L1Reorgs))
}
