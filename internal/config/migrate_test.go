package config

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

func fakeHostname(t *testing.T, name string) {
	t.Helper()
	original, originalEnv := osHostname, getenv
	osHostname = func() (string, error) { return name, nil }
	getenv = func(string) string { return "" }
	t.Cleanup(func() { osHostname, getenv = original, originalEnv })
}

func fakeEnv(t *testing.T, values map[string]string) {
	t.Helper()
	getenv = func(key string) string { return values[key] }
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(writer); log.SetFlags(flags) })
	return &buffer
}

func TestLoadOldFullConfigKeepsValues(t *testing.T) {
	fakeHostname(t, "ignored.example.net")
	path := writeTestConfig(t, `{
  "listen": "0.0.0.0:9090",
  "node": {"key": "node-a", "hostname": "node-a.example.net", "models": [
    {"key": "model-a", "name": "Model A", "metrics_url": "http://127.0.0.1:8000/metrics", "port": "8000", "unit": "vllm-model-a.service", "user_unit": true}
  ]},
  "peers": [{"key": "node-b", "hostname": "node-b.example.net", "url": "http://192.0.2.11:9090"}]
}`)
	logs := captureLog(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.Node.Models[0]
	if cfg.Listen != "0.0.0.0:9090" || cfg.Node.Key != "node-a" || cfg.Node.Hostname != "node-a.example.net" || cfg.Node.Name != "" {
		t.Errorf("node = %+v", cfg.Node)
	}
	if model.Key != "model-a" || model.Name != "Model A" || model.Port != "8000" || model.Unit != "vllm-model-a.service" || model.UserUnit || model.LegacyUserUnit != nil || model.KeyAuto || model.NameAuto || model.PortAuto {
		t.Errorf("model = %+v", model)
	}
	// The obsolete peer fields are ignored. The fallback comes from the URL
	// until the peer reports its own key and name.
	if peer := cfg.Peers[0]; peer.Key != "192-0-2-11" || peer.Name != "192.0.2.11" || peer.Hostname != "" || peer.URL != "http://192.0.2.11:9090" {
		t.Errorf("peer = %+v", peer)
	}
	for _, want := range []string{"node.models[0].user_unit is obsolete", "peers[0] fields hostname, key are obsolete"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs.String(), want)
		}
	}
	if !cfg.Node.DiscoveryEnabled() {
		t.Error("discovery must be on by default")
	}
}

func TestLoadWithoutFileUsesHostname(t *testing.T) {
	fakeHostname(t, "GPU-Node-01.example.net")
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.json")} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Listen != ":9090" || cfg.Node.Key != "gpu-node-01" || cfg.Node.Hostname != "GPU-Node-01.example.net" || len(cfg.Node.Models) != 0 || len(cfg.Peers) != 0 {
			t.Errorf("Load(%q) = %+v", path, cfg)
		}
	}
}

func TestLoadInvalidHostnameUsesKeyAsName(t *testing.T) {
	fakeHostname(t, "bad_host")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Key != "bad_host" || cfg.Node.Name != "bad_host" || cfg.Node.Hostname != "" {
		t.Errorf("node = %+v", cfg.Node)
	}
}

func TestLoadMinimalModelsAndPeers(t *testing.T) {
	fakeHostname(t, "collector")
	path := writeTestConfig(t, `{
  "node": {"discover": false, "models": [
    {"port": "8001"},
    {"metrics_url": "http://127.0.0.1:8002/metrics"},
    {"name": "Big Model", "metrics_url": "https://proxy.example.net/big/metrics"}
  ]},
  "peers": [
    "http://node-a.example.net:9090",
    "http://192.0.2.10:9090",
    "http://node-a.example.org:9091",
    {"url": "http://192.0.2.12:9090", "key": "custom"}
  ]
}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.DiscoveryEnabled() {
		t.Error("discover false must turn discovery off")
	}
	models := cfg.Node.Models
	if m := models[0]; m.Key != "port-8001" || m.Name != "port-8001" || m.MetricsURL != "http://127.0.0.1:8001/metrics" || !m.KeyAuto || !m.NameAuto || m.PortAuto {
		t.Errorf("port-only model = %+v", m)
	}
	if m := models[1]; m.Key != "port-8002" || m.Port != "8002" || !m.PortAuto {
		t.Errorf("url-only model = %+v", m)
	}
	if m := models[2]; m.Key != "big-model" || m.Name != "Big Model" || m.Port != "" {
		t.Errorf("named model = %+v", m)
	}
	want := []struct{ key, name, hostname string }{
		{"node-a", "", "node-a.example.net"},
		{"192-0-2-10", "192.0.2.10", ""},
		{"node-a-9091", "", "node-a.example.org"},
		{"192-0-2-12", "192.0.2.12", ""},
	}
	for index, w := range want {
		peer := cfg.Peers[index]
		if peer.Key != w.key || peer.Name != w.name || peer.Hostname != w.hostname {
			t.Errorf("peer %d = %+v, want %+v", index, peer, w)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	fakeHostname(t, "node")
	for name, content := range map[string]string{
		"model without url or port": `{"node": {"models": [{"key": "a"}]}}`,
		"bad port":                  `{"node": {"models": [{"port": "99999"}]}}`,
		"duplicate auto model keys": `{"node": {"models": [{"port": "8000"}, {"port": "8000"}]}}`,
		"unknown field":             `{"node": {"models": [{"port": "8000", "typo": 1}]}}`,
		"unknown peer field":        `{"peers": [{"url": "http://192.0.2.1:9090", "typo": 1}]}`,
		"bad peer url":              `{"peers": ["ftp://192.0.2.1"]}`,
		"duplicate peers":           `{"peers": ["http://192.0.2.1:9090", "http://192.0.2.1:9090"]}`,
		"bad listen env":            `{}`,
	} {
		if name == "bad listen env" {
			fakeEnv(t, map[string]string{EnvDiscover: "maybe"})
		}
		if _, err := Load(writeTestConfig(t, content)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSanitizeKey(t *testing.T) {
	for in, want := range map[string]string{
		"Qwen3-32B":   "qwen3-32b",
		"Big Model":   "big-model",
		"--x--":       "x",
		"192.0.2.1":   "192-0-2-1",
		"":            "",
		"!!!":         "",
		"2001:db8::1": "2001-db8--1",
	} {
		if got := SanitizeKey(in); got != want {
			t.Errorf("SanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadEnvironmentOverridesFile(t *testing.T) {
	fakeHostname(t, "ignored")
	fakeEnv(t, map[string]string{
		EnvListen:   "127.0.0.1:9191",
		EnvHostname: "gpu-07.example.net",
		EnvDiscover: "false",
		EnvPeers:    "http://192.0.2.20:9090, http://192.0.2.21:9090",
	})
	cfg, err := Load(writeTestConfig(t, `{"listen": ":9090", "peers": ["http://192.0.2.22:9090"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9191" || cfg.Node.Key != "gpu-07" || cfg.Node.Hostname != "gpu-07.example.net" || cfg.Node.DiscoveryEnabled() {
		t.Errorf("config = %+v", cfg)
	}
	if len(cfg.Peers) != 3 || cfg.Peers[2].URL != "http://192.0.2.21:9090" {
		t.Errorf("peers = %+v", cfg.Peers)
	}
}

func TestLoadDuplicatePeerHostsGetUniqueKeys(t *testing.T) {
	fakeHostname(t, "node-a")
	cfg, err := Load(writeTestConfig(t, `{"peers": ["http://node-a.example.net:9091", "http://node-a.example.org:9091", "http://node-a.example.com:9091"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{cfg.Peers[0].Key, cfg.Peers[1].Key, cfg.Peers[2].Key}; got[0] != "node-a-9091" || got[1] != "node-a-9091-2" || got[2] != "node-a-9091-3" {
		t.Errorf("peer keys = %v", got)
	}
}
