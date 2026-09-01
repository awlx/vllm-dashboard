package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
