package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestTelemetryCollectorFreshnessAndUnits(t *testing.T) {
	now := time.Now()
	latest := Sample{Time: now.UnixMilli(), Running: 3, KVCachePct: 50, TTFTMs: 250, MemUsedGB: 2, NetRxMbps: 8, GPUClockMHz: 1000}
	collector := &telemetryCollector{now: now, nodes: []telemetryNode{{key: "node-a", up: true, snapshot: TelemetrySnapshot{
		Host: latest,
		Models: []ModelTelemetry{
			{Key: "fresh", Latest: latest, LastScrape: latest.Time, LastPollSuccess: true, Counters: &TelemetryCounters{PromptTokens: 123}},
			{Key: "failed", Latest: latest, LastScrape: latest.Time, Counters: &TelemetryCounters{}},
			{Key: "stale", Latest: latest, LastScrape: now.Add(-time.Minute).UnixMilli(), LastPollSuccess: true, Counters: &TelemetryCounters{}},
		},
	}}}}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"model_kv_cache_usage_ratio": 0.5, "model_ttft_average_seconds": 0.25,
		"node_memory_used_bytes": 2 << 30, "node_network_receive_bytes_per_second": 1e6,
		"node_gpu_clock_hertz": 1e9, "model_prompt_tokens_total": 123,
	}
	for _, family := range families {
		name := strings.TrimPrefix(family.GetName(), "vllm_dashboard_")
		if name == "model_up" {
			if len(family.Metric) != 3 {
				t.Fatalf("expected availability for all models: %v", family)
			}
			for _, metric := range family.Metric {
				value := 0.0
				for _, label := range metric.Label {
					if label.GetName() == "model" && label.GetValue() == "fresh" {
						value = 1
					}
				}
				if metric.GetGauge().GetValue() != value {
					t.Fatalf("wrong availability: %v", metric)
				}
			}
		}
		if expected, ok := want[name]; ok {
			if len(family.Metric) != 1 {
				t.Fatalf("duplicate or stale series: %v", family)
			}
			metric := family.Metric[0]
			value := metric.GetGauge().GetValue()
			if metric.Counter != nil {
				value = metric.GetCounter().GetValue()
			}
			if value != expected {
				t.Errorf("%s = %g, want %g", name, value, expected)
			}
			delete(want, name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing metrics: %v", want)
	}
}

func TestMetricsRoutesAndPeers(t *testing.T) {
	stamp := time.Now().UnixMilli()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry" {
			t.Errorf("unexpected peer path %s", r.URL.Path)
		}
		writeJSON(w, TelemetrySnapshot{Version: 1, Host: Sample{Time: stamp}, KernelVersion: "6.11.0-1016-nvidia", Models: []ModelTelemetry{
			{Key: "model-a", Name: "discovered-llm", Latest: Sample{Time: stamp, Running: 7}, LastScrape: stamp, LastPollSuccess: true, Counters: &TelemetryCounters{GenerationTokens: 42}},
		}})
	}))
	defer peer.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer bad.Close()
	configureRuntime(DashboardConfig{Node: NodeConfig{Key: "collector", Name: "Fleet Collector"}, Peers: []PeerConfig{{Key: "remote", Hostname: "spark-one.example.test", URL: peer.URL}, {Key: "failed", Name: "Offline node", URL: bad.URL}}})
	mux := http.NewServeMux()
	registerMetricsHandlers(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	for _, line := range []string{
		`vllm_dashboard_node_up{node="Fleet Collector",node_key="collector"} 1`,
		`vllm_dashboard_node_up{node="Offline node",node_key="failed"} 0`,
		`vllm_dashboard_node_up{node="spark-one",node_key="remote"} 1`,
		`vllm_dashboard_node_info{kernel_version="6.11.0-1016-nvidia",node="spark-one",node_key="remote"} 1`,
		`vllm_dashboard_model_generation_tokens_total{model="discovered-llm",model_key="model-a",node="spark-one",node_key="remote"} 42`,
	} {
		if !strings.Contains(response.Body.String(), line) {
			t.Errorf("missing %s", line)
		}
	}
	if strings.Contains(response.Body.String(), `vllm_dashboard_node_info{kernel_version=""`) || strings.Count(response.Body.String(), "vllm_dashboard_node_info{") != 1 {
		t.Errorf("node_info must only be emitted for nodes reporting a kernel version")
	}
	for _, path := range []string{"/metrics", "/api/telemetry"} {
		for _, method := range []string{"HEAD", "POST"} {
			result := httptest.NewRecorder()
			mux.ServeHTTP(result, httptest.NewRequest(method, path, nil))
			if method == "POST" && (result.Code != 405 || result.Header().Get("Allow") != "GET, HEAD") {
				t.Fatalf("method not rejected: %v", result)
			}
			if method == "HEAD" && (result.Code != 200 || result.Body.Len() != 0) {
				t.Fatalf("invalid HEAD: %v", result)
			}
		}
	}
	local := httptest.NewRecorder()
	mux.ServeHTTP(local, httptest.NewRequest("GET", "/api/telemetry", nil))
	var snapshot TelemetrySnapshot
	if err := json.Unmarshal(local.Body.Bytes(), &snapshot); err != nil || len(snapshot.Models) != 0 {
		t.Fatalf("local API included peers: %s", local.Body)
	}
}

func TestValidateTelemetryRejectsInvalidKernelVersion(t *testing.T) {
	snapshot := TelemetrySnapshot{Version: telemetryVersion, Models: []ModelTelemetry{}, KernelVersion: "6.8\"} evil"}
	if err := validateTelemetry(snapshot, time.Now()); err == nil {
		t.Fatal("accepted invalid kernel version")
	}
}

func TestLocalTelemetryUsesCachedRuntimeName(t *testing.T) {
	configureRuntime(DashboardConfig{Node: NodeConfig{Key: "local", Models: []ModelConfig{
		{Key: "secondary", Name: "Configured fallback"},
	}}})
	mu.Lock()
	modelStates["secondary"].ModelName = "discovered-served-name"
	snapshot := localTelemetryLocked()
	modelStates["secondary"].ModelName = ""
	fallback := localTelemetryLocked()
	mu.Unlock()
	if len(snapshot.Models) != 1 || snapshot.Models[0].Name != "discovered-served-name" || snapshot.Models[0].Key != "secondary" {
		t.Fatalf("runtime identity not propagated: %+v", snapshot.Models)
	}
	if fallback.Models[0].Name != "Configured fallback" {
		t.Fatalf("configured fallback lost: %+v", fallback.Models)
	}
}
