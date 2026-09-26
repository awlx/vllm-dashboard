package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dashboard.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigAcceptsMultiplePeers(t *testing.T) {
	path := writeTestConfig(t, `{
  "listen": "127.0.0.1:9090",
	"node": {"key":"node-a","name":"Compute Node A","models":[{"key":"model-a","name":"Model A","metrics_url":"http://127.0.0.1:8000/metrics"}]},
	"peers": [{"key":"node-b","name":"Compute Node B","url":"http://192.0.2.11:9090"}]
}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Key != "node-a" || len(cfg.Peers) != 1 || cfg.Peers[0].Key != "node-b" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigRejectsDuplicateNodeAndPeerKeys(t *testing.T) {
	path := writeTestConfig(t, `{
  "listen": ":9090",
	"node": {"key":"node-a","name":"Compute Node A","models":[]},
	"peers": [{"key":"node-a","name":"Duplicate","url":"http://192.0.2.11:9090"}]
}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected duplicate peer key error")
	}
}

func TestDisplayNameUsesConfiguredHostnameLabel(t *testing.T) {
	tests := map[string]string{
		"compute.example.net":    "compute",
		"compute-01.example.net": "compute-01",
	}
	for hostname, want := range tests {
		if got := displayName("Compute Node", hostname); got != want {
			t.Errorf("displayName(%q) = %q, want %q", hostname, got, want)
		}
	}
}

func TestPeerAPIForwardsModelAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/metrics" || r.URL.Query().Get("model") != "model-a" {
			t.Fatalf("unexpected peer request: %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode([]Sample{{Time: 123, Running: 2}})
	}))
	defer server.Close()

	var samples []Sample
	if err := peerAPI(PeerConfig{Key: "node-b", URL: server.URL}, "/api/metrics", "model-a", &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Running != 2 {
		t.Fatalf("unexpected samples: %+v", samples)
	}
}

func TestCombinedOverviewScopesPeerModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]ModelOverview{{Key: "node-b/model-a", Name: "Model A"}})
	}))
	defer server.Close()

	configureRuntime(DashboardConfig{
		Node:  NodeConfig{Key: "node-a", Name: "Node A"},
		Peers: []PeerConfig{{Key: "node-b", Name: "Node B", Hostname: "node-b.example.net", URL: server.URL}},
	})
	overview := combinedOverview()
	if len(overview) != 1 || overview[0].Key != "node-b/model-a" || overview[0].NodeName != "node-b" {
		t.Fatalf("unexpected overview: %+v", overview)
	}
}

func TestAggregateSampleSetsCombinesNodes(t *testing.T) {
	got := aggregateSampleSets([][]Sample{
		{{Time: 100, Running: 1, GPUUtilPct: 40, GPUPowerW: 20, GPUClockMHz: 1800, CPUClockMHz: 2800}},
		{{Time: 100, Running: 2, GPUUtilPct: 70, GPUPowerW: 30, GPUClockMHz: 2400, CPUClockMHz: 3900}},
	})
	if len(got) != 1 || got[0].Running != 3 || got[0].GPUUtilPct != 70 || got[0].GPUPowerW != 50 || got[0].GPUClockMHz != 2400 || got[0].CPUClockMHz != 3900 {
		t.Fatalf("unexpected aggregate: %+v", got)
	}
}

func TestAverageClockKHz(t *testing.T) {
	got := averageClockKHz([]string{"2808000\n", "3900000", "offline", "0"})
	if got != 3354 {
		t.Fatalf("average clock = %.0f MHz, want 3354", got)
	}
}

func TestTokensInWindowCountsResetAfterNewCounterSurpassesAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	samples := []Sample{
		{Time: now.Add(-2 * time.Hour).UnixMilli(), PromptTokensCum: 32_000_000, GenTokensCum: 100_000},
		{Time: now.Add(-30 * time.Minute).UnixMilli(), PromptTokensCum: 500_000, GenTokensCum: 2_000},
		{Time: now.UnixMilli(), PromptTokensCum: 40_000_000, GenTokensCum: 120_000},
	}

	prompt, generated := tokensInWindow(samples, now, 24*time.Hour)
	if prompt != 40_000_000 {
		t.Fatalf("prompt tokens = %.0f, want 40000000", prompt)
	}
	if generated != 120_000 {
		t.Fatalf("generated tokens = %.0f, want 120000", generated)
	}
}

func TestTokensInWindowUsesTrailingWindowAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	samples := []Sample{
		{Time: now.Add(-2 * time.Hour).UnixMilli(), PromptTokensCum: 100},
		{Time: now.Add(-time.Hour).UnixMilli(), PromptTokensCum: 200},
		{Time: now.Add(-30 * time.Minute).UnixMilli(), PromptTokensCum: 260},
		{Time: now.UnixMilli(), PromptTokensCum: 300},
	}

	prompt, _ := tokensInWindow(samples, now, time.Hour)
	if prompt != 100 {
		t.Fatalf("prompt tokens = %.0f, want 100", prompt)
	}
}

func TestAddUsageSumsModelsIndependently(t *testing.T) {
	qwen := TokenUsage{Last24h: TokenWindow{PromptTokens: 40, GenTokens: 4, TotalTokens: 44}}
	gemma := TokenUsage{Last24h: TokenWindow{PromptTokens: 30, GenTokens: 3, TotalTokens: 33}}

	total := addUsage(qwen, gemma)
	if total.Last24h.PromptTokens != 70 || total.Last24h.GenTokens != 7 || total.Last24h.TotalTokens != 77 {
		t.Fatalf("combined usage = %+v, want prompt=70 gen=7 total=77", total.Last24h)
	}
}

func TestUsageSeedsWindowWhenModelStartedInsideIt(t *testing.T) {
	now := time.Now()
	samples := []Sample{{
		Time:            now.UnixMilli(),
		PromptTokensCum: 40_000_000,
		GenTokensCum:    500_000,
	}}

	usage := usageForStartedAt(samples, now.Add(-11*time.Hour))
	if usage.Last24h.TotalTokens != 40_500_000 {
		t.Fatalf("24h tokens = %.0f, want 40500000", usage.Last24h.TotalTokens)
	}
	if usage.LastHour.TotalTokens != 0 {
		t.Fatalf("1h tokens = %.0f, want 0", usage.LastHour.TotalTokens)
	}
}

func TestSpeculativeConfigFromDottedArgs(t *testing.T) {
	args := []string{
		"--speculative-config.model", "example/Model-DFlash",
		"--speculative-config.num_speculative_tokens", "3",
	}
	got := speculativeConfig(args)
	want := `{"method":"dflash","model":"example/Model-DFlash","num_speculative_tokens":3}`
	if got != want {
		t.Fatalf("speculative config = %s, want %s", got, want)
	}
}

func TestModelDisplayNameUsesRuntimeIdentity(t *testing.T) {
	info := ServerInfo{ServedModelName: "nemotron-3.5-lightning-30b-a3b"}
	if got := modelDisplayName(info, "Secondary model"); got != info.ServedModelName {
		t.Fatalf("display name = %q, want %q", got, info.ServedModelName)
	}
}

func TestDiscoverServedModelName(t *testing.T) {
	for _, testcase := range []struct{ body, want string }{
		{`{"data":[{"id":"actual-served-model"}]}`, "actual-served-model"},
		{`{"data":[{"id":"first"},{"id":"second"}]}`, ""},
		{`<html>not an API</html>`, ""},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/prefix/v1/models" {
				t.Errorf("wrong discovery path: %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(testcase.body))
		}))
		got := discoverServedModelName(server.URL + "/prefix/metrics")
		server.Close()
		if got != testcase.want {
			t.Fatalf("discovered %q, want %q", got, testcase.want)
		}
	}
}

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

func TestServesVLLMOnPort(t *testing.T) {
	for _, testcase := range []struct {
		args []string
		want bool
	}{
		{[]string{"/usr/bin/vllm", "serve", "org/model", "--port", "8888"}, true},
		{[]string{"/opt/venv/bin/python", "-m", "vllm.entrypoints.cli.main", "serve", "/models/tp1", "--port=8888"}, true},
		{[]string{"/usr/bin/vllm", "serve", "org/model", "--port", "88880"}, false},
		{[]string{"bash", "-lc", "vllm serve org/model --port 8888"}, false},
		{[]string{"/home/user/.local/bin/vllm-dashboard"}, false},
	} {
		if got := servesVLLMOnPort(testcase.args, "8888"); got != testcase.want {
			t.Errorf("servesVLLMOnPort(%q) = %v, want %v", testcase.args, got, testcase.want)
		}
	}
}

func TestParseKernelVersion(t *testing.T) {
	for input, want := range map[string]string{
		"6.11.0-1016-nvidia\n": "6.11.0-1016-nvidia",
		"6.8.0-85-generic":     "6.8.0-85-generic",
		"":                     "",
		"6.8 bad\"label":       "",
		"-leading-dash":        "",
	} {
		if got := parseKernelVersion(input); got != want {
			t.Errorf("parseKernelVersion(%q) = %q, want %q", input, got, want)
		}
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

func TestScrapeVLLMRejectsErrorAndNonMetrics(t *testing.T) {
	for _, body := range []string{"server error", "<html>not metrics</html>", "other_metric 1\n"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body == "server error" {
				w.WriteHeader(503)
			}
			_, _ = w.Write([]byte(body))
		}))
		_, err := scrapeVLLM(server.URL)
		server.Close()
		if err == nil {
			t.Fatalf("accepted invalid engine response %q", body)
		}
	}
}
