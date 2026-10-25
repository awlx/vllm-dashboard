package metrics

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	dashboardMetric  = regexp.MustCompile(`(vllm_dashboard_[a-z0-9_]+)(?:\{([^}]*)\})?`)
	dashboardMatcher = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)`)
	dashboardBy      = regexp.MustCompile(`\bby\s*\(([^)]*)\)`)
	dashboardLegend  = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	dashboardValues  = regexp.MustCompile(`label_values\((vllm_dashboard_[a-z0-9_]+)(?:\{[^}]*\})?,\s*([a-zA-Z_][a-zA-Z0-9_]*)\)`)
	dashboardWord    = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
)

// exportedLabels gathers one fully populated node and returns the label
// names of every exported metric family.
func exportedLabels(t *testing.T) map[string]map[string]bool {
	t.Helper()
	now := time.Now()
	full := sample.Sample{
		Time: now.UnixMilli(), Running: 1, Waiting: 1, KVCachePct: 1, PromptTokPerSec: 1, GenTokPerSec: 1,
		ReqPerSec: 1, TTFTMs: 1, InterTokenMs: 1, E2ELatencyMs: 1, PrefixCacheHitPct: 1, SpecAcceptPct: 1,
		CPUUtilPct: 1, CPUClockMHz: 1, GPUUtilPct: 1, GPUTempC: 1, GPUPowerW: 1, GPUClockMHz: 1,
		MemUsedPct: 1, MemUsedGB: 1, MemTotalGB: 1, NetRxMbps: 1, NetTxMbps: 1,
	}
	collector := &telemetryCollector{now: now, nodes: []telemetryNode{{key: "node-a", name: "Node A", up: true, snapshot: TelemetrySnapshot{
		KernelVersion: "6.8.0",
		Host:          full,
		Models: []ModelTelemetry{{
			Key: "model-a", Name: "Model A", Latest: full, LastScrape: full.Time, LastPollSuccess: true,
			Counters: &TelemetryCounters{PromptTokens: 1, GenerationTokens: 1, CompletedRequests: 1, Preemptions: 1},
		}},
	}}}}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]map[string]bool{}
	for _, family := range families {
		// Prometheus adds job and instance at scrape time.
		set := map[string]bool{"job": true, "instance": true}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				set[label.GetName()] = true
			}
		}
		labels[family.GetName()] = set
	}
	return labels
}

type dashboardPanel struct {
	Title   string           `json:"title"`
	Targets []dashboardQuery `json:"targets"`
	Panels  []dashboardPanel `json:"panels"`
}

type dashboardQuery struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
}

func TestGrafanaDashboardUsesExportedMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../docs/grafana-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels     []dashboardPanel `json:"panels"`
		Templating struct {
			List []struct {
				Name  string          `json:"name"`
				Query json.RawMessage `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	exported := exportedLabels(t)
	used := map[string]bool{}

	checkQuery := func(where, expr, legend string) {
		available := map[string]bool{}
		matches := dashboardMetric.FindAllStringSubmatch(expr, -1)
		if len(matches) == 0 {
			t.Errorf("%s: query uses no vllm_dashboard metric: %s", where, expr)
		}
		for _, match := range matches {
			labels, ok := exported[match[1]]
			if !ok {
				t.Errorf("%s: metric %s is not exported", where, match[1])
				continue
			}
			used[match[1]] = true
			for name := range labels {
				available[name] = true
			}
			for _, matcher := range dashboardMatcher.FindAllStringSubmatch(match[2], -1) {
				if !labels[matcher[1]] {
					t.Errorf("%s: metric %s has no label %q", where, match[1], matcher[1])
				}
			}
		}
		for _, by := range dashboardBy.FindAllStringSubmatch(expr, -1) {
			for _, name := range dashboardWord.FindAllString(by[1], -1) {
				if !available[name] {
					t.Errorf("%s: grouping label %q is not exported", where, name)
				}
			}
		}
		for _, name := range dashboardLegend.FindAllStringSubmatch(legend, -1) {
			if !available[name[1]] {
				t.Errorf("%s: legend label %q is not exported", where, name[1])
			}
		}
	}

	var walk func([]dashboardPanel)
	walk = func(panels []dashboardPanel) {
		for _, panel := range panels {
			for _, target := range panel.Targets {
				checkQuery(panel.Title, target.Expr, target.LegendFormat)
			}
			walk(panel.Panels)
		}
	}
	walk(dashboard.Panels)

	for _, variable := range dashboard.Templating.List {
		var query string
		if json.Unmarshal(variable.Query, &query) != nil {
			var object struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(variable.Query, &object); err != nil {
				t.Fatalf("variable %s: %v", variable.Name, err)
			}
			query = object.Query
		}
		for _, match := range dashboardValues.FindAllStringSubmatch(query, -1) {
			labels, ok := exported[match[1]]
			if !ok || !labels[match[2]] {
				t.Errorf("variable %s: %s has no label %q", variable.Name, match[1], match[2])
			}
		}
	}

	for _, name := range []string{
		"vllm_dashboard_node_gpu_clock_hertz",
		"vllm_dashboard_node_cpu_clock_hertz",
		"vllm_dashboard_node_memory_used_ratio",
		"vllm_dashboard_model_last_success_timestamp_seconds",
	} {
		if !used[name] {
			t.Errorf("dashboard has no panel for %s", name)
		}
	}
}
