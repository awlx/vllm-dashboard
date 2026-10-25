package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type ModelConfig struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	MetricsURL string `json:"metrics_url"`
	Port       string `json:"port,omitempty"`
	Unit       string `json:"unit,omitempty"`
	UserUnit   bool   `json:"user_unit,omitempty"`
}

type NodeConfig struct {
	Key      string        `json:"key"`
	Name     string        `json:"name,omitempty"`
	Hostname string        `json:"hostname,omitempty"`
	Models   []ModelConfig `json:"models"`
}

type PeerConfig struct {
	Key      string `json:"key"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	URL      string `json:"url"`
}

type DashboardConfig struct {
	Listen string `json:"listen"`
	// PollInterval is a Go duration string, for example "1s" or "500ms".
	PollInterval string       `json:"poll_interval,omitempty"`
	Node         NodeConfig   `json:"node"`
	Peers        []PeerConfig `json:"peers,omitempty"`
}

func parsePollInterval(value string) (time.Duration, error) {
	if value == "" {
		return defaultPollInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid poll_interval %q: %w", value, err)
	}
	if interval < minPollInterval || interval > maxPollInterval {
		return 0, fmt.Errorf("poll_interval %s must be between %s and %s", interval, minPollInterval, maxPollInterval)
	}
	return interval, nil
}

var configKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var hostnameRe = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

func defaultConfig() DashboardConfig {
	return DashboardConfig{
		Listen: ":9090",
		Node: NodeConfig{
			Key:  "node-a",
			Name: "Compute Node A",
			Models: []ModelConfig{
				{Key: "model-a", Name: "Model A", MetricsURL: "http://127.0.0.1:8000/metrics", Port: "8000", Unit: "vllm-model-a.service"},
				{Key: "model-b", Name: "Model B", MetricsURL: "http://127.0.0.1:8001/metrics", Port: "8001", Unit: "vllm-model-b.service", UserUnit: true},
			},
		},
	}
}

func loadConfig(path string) (DashboardConfig, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DashboardConfig{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return DashboardConfig{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Listen == "" {
		return DashboardConfig{}, fmt.Errorf("listen must not be empty")
	}
	if _, err := parsePollInterval(cfg.PollInterval); err != nil {
		return DashboardConfig{}, err
	}
	if !configKeyRe.MatchString(cfg.Node.Key) {
		return DashboardConfig{}, fmt.Errorf("invalid node key %q", cfg.Node.Key)
	}
	if displayName(cfg.Node.Name, cfg.Node.Hostname) == "" || (cfg.Node.Hostname != "" && !hostnameRe.MatchString(cfg.Node.Hostname)) {
		return DashboardConfig{}, fmt.Errorf("node requires a valid name or hostname")
	}
	seen := map[string]bool{}
	for index, model := range cfg.Node.Models {
		if !configKeyRe.MatchString(model.Key) || seen[model.Key] {
			return DashboardConfig{}, fmt.Errorf("invalid or duplicate model key %q", model.Key)
		}
		if model.Name == "" || model.MetricsURL == "" {
			return DashboardConfig{}, fmt.Errorf("model %d requires name and metrics_url", index)
		}
		parsed, err := url.ParseRequestURI(model.MetricsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return DashboardConfig{}, fmt.Errorf("model %q has invalid metrics_url", model.Key)
		}
		seen[model.Key] = true
	}
	peerKeys := map[string]bool{cfg.Node.Key: true}
	for _, peer := range cfg.Peers {
		parsed, err := url.ParseRequestURI(peer.URL)
		if !configKeyRe.MatchString(peer.Key) || peerKeys[peer.Key] || displayName(peer.Name, peer.Hostname) == "" || (peer.Hostname != "" && !hostnameRe.MatchString(peer.Hostname)) || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return DashboardConfig{}, fmt.Errorf("invalid or duplicate peer %q", peer.Key)
		}
		peerKeys[peer.Key] = true
	}
	return cfg, nil
}

func scopedModelKey(nodeKey, modelKey string) string {
	return nodeKey + "/" + modelKey
}

func shortHostname(hostname string) string {
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	if label, _, found := strings.Cut(hostname, "."); found {
		return label
	}
	return hostname
}

func displayName(name, hostname string) string {
	if hostname != "" {
		return shortHostname(hostname)
	}
	return name
}
