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
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate peer key error")
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
