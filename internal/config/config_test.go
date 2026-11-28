package config

import (
	"os"
	"path/filepath"
	"testing"
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
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Key != "node-a" || len(cfg.Peers) != 1 || cfg.Peers[0].URL != "http://192.0.2.11:9090" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigGivesPeerAKeyThatDiffersFromTheNodeKey(t *testing.T) {
	path := writeTestConfig(t, `{
  "listen": ":9090",
	"node": {"key":"node-a","name":"Compute Node A","models":[]},
	"peers": ["http://node-a.example.net:9090"]
}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Key != "node-a-9090" {
		t.Fatalf("peer key = %q, want node-a-9090", cfg.Peers[0].Key)
	}
}

func TestNodeModelReturnsMetricsModelWithAutomaticName(t *testing.T) {
	model, err := NodeModel("http://qwen-main-gpu0:8000/")
	if err != nil {
		t.Fatal(err)
	}
	want := Model{
		Key:        "qwen-main-gpu0",
		Name:       "qwen-main-gpu0",
		NameAuto:   true,
		MetricsURL: "http://qwen-main-gpu0:8000/metrics",
		Port:       "8000",
		PortAuto:   true,
	}
	if model != want {
		t.Fatalf("NodeModel = %+v, want %+v", model, want)
	}
	if model.KeyAuto {
		t.Error("key is not automatic")
	}
}

func TestNodeModelRejectsInvalidURLs(t *testing.T) {
	for _, raw := range []string{
		"qwen:8000",
		"ftp://qwen:8000",
		"http://",
		"http://qwen:8000/v1",
		"http://qwen:8000?x=1",
		"http://gpu:8000?",
		"http://user:pass@qwen:8000",
	} {
		if _, err := NodeModel(raw); err == nil {
			t.Errorf("NodeModel(%q) accepted the url", raw)
		}
	}
}

func TestNodeModelMarksPortAutomatic(t *testing.T) {
	tests := []struct {
		raw        string
		port       string
		metricsURL string
	}{
		{"http://192.0.2.10:8000", "8000", "http://192.0.2.10:8000/metrics"},
		{"http://192.0.2.10/", "", "http://192.0.2.10/metrics"},
	}
	for _, testcase := range tests {
		model, err := NodeModel(testcase.raw)
		if err != nil {
			t.Fatal(err)
		}
		if model.Port != testcase.port || model.MetricsURL != testcase.metricsURL || !model.PortAuto {
			t.Errorf("NodeModel(%q) = %+v, want port %q and metrics url %q", testcase.raw, model, testcase.port, testcase.metricsURL)
		}
	}
}

func TestNodeModelWithDefaultPortCompletes(t *testing.T) {
	model, err := NodeModel("http://gpu.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if model.Port != "" || model.MetricsURL != "http://gpu.example.net/metrics" {
		t.Fatalf("model = %+v", model)
	}
	if err := CompleteModel(&model); err != nil {
		t.Fatal(err)
	}
	if model.Port != "" || model.MetricsURL != "http://gpu.example.net/metrics" {
		t.Fatalf("completed model = %+v", model)
	}
}

func TestNodeModelKeysDifferByHostAndPort(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"http://192.0.2.10:8000", "192-0-2-10"},
		{"http://gpu.example.net:8000", "gpu"},
		{"https://qwen:443", "qwen"},
	}
	for _, testcase := range tests {
		model, err := NodeModel(testcase.raw)
		if err != nil {
			t.Fatal(err)
		}
		if model.Key != testcase.want {
			t.Errorf("NodeModel(%q).Key = %q, want %q", testcase.raw, model.Key, testcase.want)
		}
	}
}

func TestLoadConfigAddsNodeModelsFromEnv(t *testing.T) {
	t.Setenv(EnvNodes, "http://qwen-main-gpu0:8000, http://qwen-embedding:8000")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Node.Models) != 2 {
		t.Fatalf("models = %+v, want two models", cfg.Node.Models)
	}
	first, second := cfg.Node.Models[0], cfg.Node.Models[1]
	if first.Key != "qwen-main-gpu0" || first.MetricsURL != "http://qwen-main-gpu0:8000/metrics" {
		t.Errorf("first model = %+v", first)
	}
	if second.Key != "qwen-embedding" || second.MetricsURL != "http://qwen-embedding:8000/metrics" {
		t.Errorf("second model = %+v", second)
	}
	if !first.NameAuto || !second.NameAuto {
		t.Error("names are not automatic")
	}
}

func TestLoadConfigGivesDuplicateNodeHostsUniqueKeys(t *testing.T) {
	t.Setenv(EnvNodes, "http://gpu.example.net:8000, http://gpu.example.org:8001")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Models[0].Key != "gpu" || cfg.Node.Models[1].Key != "gpu-8001" {
		t.Fatalf("keys = %q, %q, want gpu, gpu-8001", cfg.Node.Models[0].Key, cfg.Node.Models[1].Key)
	}
}

func TestLoadConfigSkipsNodeModelOfConfiguredModel(t *testing.T) {
	path := writeTestConfig(t, `{
	"node": {"key":"node-a","name":"Node A","models":[{"key":"model-a","name":"Model A","metrics_url":"http://qwen-main-gpu0:8000/metrics"}]}
}`)
	t.Setenv(EnvNodes, "http://qwen-main-gpu0:8000, http://qwen-embedding:8000")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Node.Models) != 2 || cfg.Node.Models[0].Key != "model-a" || cfg.Node.Models[1].Key != "qwen-embedding" {
		t.Fatalf("models = %+v", cfg.Node.Models)
	}
}

func TestLoadConfigRejectsDuplicateNodeURL(t *testing.T) {
	t.Setenv(EnvNodes, "http://qwen-main-gpu0:8000, http://qwen-main-gpu0:8000")
	if _, err := Load(""); err == nil {
		t.Fatal("Load accepted a duplicate node url")
	}
}

func TestLoadConfigIgnoresEmptyNodesEnv(t *testing.T) {
	for _, value := range []string{"", " , ", " ", "\n"} {
		t.Setenv(EnvNodes, value)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load with %q failed: %v", value, err)
		}
		if len(cfg.Node.Models) != 0 {
			t.Fatalf("value %q added models: %+v", value, cfg.Node.Models)
		}
	}
}

func TestLoadConfigKeepsFileModelKeyAgainstNodeURL(t *testing.T) {
	path := writeTestConfig(t, `{
	"node": {"key":"node-a","name":"Node A","models":[{"port":"8000","name":"gpu"}]}
}`)
	t.Setenv(EnvNodes, "http://gpu:9000")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Node.Models) != 2 || cfg.Node.Models[0].Key != "gpu" || cfg.Node.Models[1].Key != "gpu-9000" {
		t.Fatalf("models = %+v", cfg.Node.Models)
	}
}

func TestLoadConfigSkipsNodeURLMatchingPortOnlyFileModel(t *testing.T) {
	path := writeTestConfig(t, `{
	"node": {"key":"node-a","name":"Node A","models":[{"port":"8000"}]}
}`)
	t.Setenv(EnvNodes, "HTTP://127.0.0.1:8000/, http://gpu.example.net:8001")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Node.Models) != 2 || cfg.Node.Models[0].Key != "port-8000" || cfg.Node.Models[1].Key != "gpu" {
		t.Fatalf("models = %+v", cfg.Node.Models)
	}
}

func TestDisplayNameUsesConfiguredHostnameLabel(t *testing.T) {
	tests := map[string]string{
		"compute.example.net":    "compute",
		"compute-01.example.net": "compute-01",
	}
	for hostname, want := range tests {
		if got := DisplayName("Compute Node", hostname); got != want {
			t.Errorf("displayName(%q) = %q, want %q", hostname, got, want)
		}
	}
}
