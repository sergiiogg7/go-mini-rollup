// Command obscheck verifies a running monitoring stack through its HTTP APIs: Prometheus targets
// are up, the alert rules are loaded, every panel query of the dashboard returns data, and Grafana
// has the dashboard and the datasource provisioned (and answers the same queries).
//
//	obscheck --prom http://127.0.0.1:9090 --grafana http://127.0.0.1:3000 --auth admin:secret
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

type result struct{ failed int }

func (r *result) check(ok bool, format string, args ...any) {
	status := "OK  "
	if !ok {
		status = "FAIL"
		r.failed++
	}
	fmt.Printf("%s %s\n", status, fmt.Sprintf(format, args...))
}

func get(u, auth string, out any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if auth != "" {
		user, pass, _ := strings.Cut(auth, ":")
		req.SetBasicAuth(user, pass)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %.200s", u, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

type dashboard struct {
	Title  string `json:"title"`
	Panels []struct {
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

type promResp struct {
	Status string `json:"status"`
	Data   struct {
		Result []json.RawMessage `json:"result"`
	} `json:"data"`
}

func main() {
	prom := flag.String("prom", "http://127.0.0.1:9090", "Prometheus base URL")
	graf := flag.String("grafana", "", "Grafana base URL (skip the Grafana checks if empty)")
	auth := flag.String("auth", "", "Grafana basic auth user:password")
	dash := flag.String("dashboard", "monitoring/grafana/dashboards/rollup.json", "dashboard JSON to check")
	wantRules := flag.Int("rules", 8, "number of alert rules expected")
	flag.Parse()
	var r result

	var targets struct {
		Data struct {
			ActiveTargets []struct {
				Labels map[string]string `json:"labels"`
				Health string            `json:"health"`
				Scrape string            `json:"scrapeUrl"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := get(*prom+"/api/v1/targets", "", &targets); err != nil {
		r.check(false, "prometheus targets: %v", err)
	}
	for _, t := range targets.Data.ActiveTargets {
		r.check(t.Health == "up", "target %-18s %s health=%s", t.Labels["job"], t.Scrape, t.Health)
	}
	r.check(len(targets.Data.ActiveTargets) > 0, "at least one target is configured")

	var rules struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name   string `json:"name"`
					Health string `json:"health"`
					State  string `json:"state"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := get(*prom+"/api/v1/rules", "", &rules); err != nil {
		r.check(false, "prometheus rules: %v", err)
	}
	n := 0
	for _, g := range rules.Data.Groups {
		for _, ru := range g.Rules {
			n++
			r.check(ru.Health == "ok", "rule  %-26s health=%s state=%s", ru.Name, ru.Health, ru.State)
		}
	}
	r.check(n == *wantRules, "%d alert rules loaded (expected %d)", n, *wantRules)

	raw, err := os.ReadFile(*dash)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, p := range d.Panels {
		for _, t := range p.Targets {
			var pr promResp
			err := get(*prom+"/api/v1/query?query="+url.QueryEscape(t.Expr), "", &pr)
			optional := strings.HasPrefix(p.Title, "Alerts") // empty while nothing fires
			r.check(err == nil && pr.Status == "success" && (len(pr.Data.Result) > 0 || optional),
				"panel %-62.62s series=%d  %.70s", p.Title, len(pr.Data.Result), t.Expr)
		}
	}

	if *graf != "" {
		var health struct {
			Database string `json:"database"`
		}
		r.check(get(*graf+"/api/health", "", &health) == nil && health.Database == "ok", "grafana health database=%s", health.Database)
		var found []struct {
			Title string `json:"title"`
			UID   string `json:"uid"`
		}
		_ = get(*graf+"/api/search?query="+url.QueryEscape(d.Title), *auth, &found)
		r.check(len(found) == 1 && found[0].Title == d.Title, "grafana dashboard %q provisioned: %v", d.Title, found)
		var ds []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			UID  string `json:"uid"`
			URL  string `json:"url"`
		}
		_ = get(*graf+"/api/datasources", *auth, &ds)
		r.check(len(ds) == 1 && ds[0].Type == "prometheus", "grafana datasource provisioned: %v", ds)
		if len(ds) == 1 {
			// the same query through Grafana's datasource proxy: proves Grafana can reach Prometheus
			var pr promResp
			err := get(*graf+"/api/datasources/proxy/uid/"+ds[0].UID+"/api/v1/query?query="+url.QueryEscape("rollup_safe_head"), *auth, &pr)
			r.check(err == nil && len(pr.Data.Result) > 0, "grafana -> prometheus query rollup_safe_head series=%d err=%v", len(pr.Data.Result), err)
		}
	}
	if r.failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", r.failed)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
