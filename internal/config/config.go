// Package config loads and checks the dashboard configuration.
package config

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

type Model struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	MetricsURL string `json:"metrics_url"`
	Port       string `json:"port,omitempty"`
	Unit       string `json:"unit,omitempty"`
	UserUnit   bool   `json:"user_unit,omitempty"`
}

type Node struct {
	Key      string  `json:"key"`
	Name     string  `json:"name,omitempty"`
	Hostname string  `json:"hostname,omitempty"`
	Models   []Model `json:"models"`
}

type Peer struct {
	Key      string `json:"key"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	URL      string `json:"url"`
}

type Config struct {
	Listen string `json:"listen"`
	// PollInterval is a Go duration string, for example "1s" or "500ms".
	PollInterval string `json:"poll_interval,omitempty"`
	Node         Node   `json:"node"`
	Peers        []Peer `json:"peers,omitempty"`
}

const (
	DefaultPollInterval = time.Second
	MinPollInterval     = 500 * time.Millisecond
	// The 30s telemetry freshness window must cover several polls.
	MaxPollInterval = 10 * time.Second
	// PollIntervalEnv overrides poll_interval from the file.
	PollIntervalEnv = "VLLM_DASHBOARD_POLL_INTERVAL"
)

// ParsePollInterval checks a poll_interval value. Empty means the default.
func ParsePollInterval(value string) (time.Duration, error) {
	if value == "" {
		return DefaultPollInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid poll_interval %q: %w", value, err)
	}
	if interval < MinPollInterval || interval > MaxPollInterval {
		return 0, fmt.Errorf("poll_interval %s must be between %s and %s", interval, MinPollInterval, MaxPollInterval)
	}
	return interval, nil
}

// Interval returns the poll interval, or the default if the value is not valid.
func (c Config) Interval() time.Duration {
	if interval, err := ParsePollInterval(c.PollInterval); err == nil {
		return interval
	}
	return DefaultPollInterval
}

var KeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var hostnameRe = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// Default returns the configuration that is used when no file is set.
func Default() Config {
	return Config{
		Listen: ":9090",
		Node: Node{
			Key:  "node-a",
			Name: "Compute Node A",
			Models: []Model{
				{Key: "model-a", Name: "Model A", MetricsURL: "http://127.0.0.1:8000/metrics", Port: "8000", Unit: "vllm-model-a.service"},
				{Key: "model-b", Name: "Model B", MetricsURL: "http://127.0.0.1:8001/metrics", Port: "8001", Unit: "vllm-model-b.service", UserUnit: true},
			},
		},
	}
}

// Load reads and checks the configuration file at path.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Listen == "" {
		return Config{}, fmt.Errorf("listen must not be empty")
	}
	if _, err := ParsePollInterval(cfg.PollInterval); err != nil {
		return Config{}, err
	}
	if !KeyRe.MatchString(cfg.Node.Key) {
		return Config{}, fmt.Errorf("invalid node key %q", cfg.Node.Key)
	}
	if DisplayName(cfg.Node.Name, cfg.Node.Hostname) == "" || (cfg.Node.Hostname != "" && !hostnameRe.MatchString(cfg.Node.Hostname)) {
		return Config{}, fmt.Errorf("node requires a valid name or hostname")
	}
	seen := map[string]bool{}
	for index, model := range cfg.Node.Models {
		if !KeyRe.MatchString(model.Key) || seen[model.Key] {
			return Config{}, fmt.Errorf("invalid or duplicate model key %q", model.Key)
		}
		if model.Name == "" || model.MetricsURL == "" {
			return Config{}, fmt.Errorf("model %d requires name and metrics_url", index)
		}
		parsed, err := url.ParseRequestURI(model.MetricsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Config{}, fmt.Errorf("model %q has invalid metrics_url", model.Key)
		}
		seen[model.Key] = true
	}
	peerKeys := map[string]bool{cfg.Node.Key: true}
	for _, peer := range cfg.Peers {
		parsed, err := url.ParseRequestURI(peer.URL)
		if !KeyRe.MatchString(peer.Key) || peerKeys[peer.Key] || DisplayName(peer.Name, peer.Hostname) == "" || (peer.Hostname != "" && !hostnameRe.MatchString(peer.Hostname)) || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Config{}, fmt.Errorf("invalid or duplicate peer %q", peer.Key)
		}
		peerKeys[peer.Key] = true
	}
	return cfg, nil
}

// ScopedModelKey returns "<node key>/<model key>".
func ScopedModelKey(nodeKey, modelKey string) string {
	return nodeKey + "/" + modelKey
}

// ShortHostname returns the first label of hostname.
func ShortHostname(hostname string) string {
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	if label, _, found := strings.Cut(hostname, "."); found {
		return label
	}
	return hostname
}

// DisplayName returns the short hostname, or name if hostname is empty.
func DisplayName(name, hostname string) string {
	if hostname != "" {
		return ShortHostname(hostname)
	}
	return name
}
