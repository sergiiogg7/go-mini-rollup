package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The dashboard and the alert rules are written by hand: this test fails when one of them
// references a metric the code does not export (a typo, or a rename that forgot them).
func TestMonitoringFilesOnlyUseExportedMetrics(t *testing.T) {
	known := map[string]bool{}
	for _, n := range Names() {
		known[n] = true
	}
	histograms := map[string]bool{BlockBuild: true}
	tok := regexp.MustCompile(`rollup_[a-z0-9_]+`)
	files := []string{"../../monitoring/alerts.yml", "../../monitoring/grafana/dashboards/rollup.json"}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		used := map[string]bool{}
		for _, m := range tok.FindAllString(string(raw), -1) {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if base := strings.TrimSuffix(m, suffix); base != m && histograms[base] {
					m = base
				}
			}
			used[m] = true
			require.True(t, known[m], "%s uses %s, which no component exports", filepath.Base(f), m)
		}
		require.NotEmpty(t, used, f)
	}
}

func TestDashboardIsValidAndCoversTheRequiredSignals(t *testing.T) {
	raw, err := os.ReadFile("../../monitoring/grafana/dashboards/rollup.json")
	require.NoError(t, err)
	var d struct {
		UID    string `json:"uid"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	require.NoError(t, json.Unmarshal(raw, &d))
	require.Equal(t, "go-mini-rollup", d.UID)
	all := string(raw)
	for _, n := range []string{SafeHead, FinalizedHead, UnsafeHead, L1Head, DerivationLag, BatcherInflight, BatcherResubmit,
		BatchAccepted, BatchRejected, FallbackEpochs, TxPoolSize, L1Reorgs, UnsafeReorgs, BlockBuild} {
		require.Contains(t, all, n, "no panel shows %s", n)
	}
	for _, p := range d.Panels {
		require.NotEmpty(t, p.Title)
	}
}
