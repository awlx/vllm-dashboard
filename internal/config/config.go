// Package config loads and checks the dashboard configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Model is one local vLLM or TensorFold server. Set metrics_url or port.
// The other fields are optional. Discovery fills them where it can.
type Model struct {
	Key        string `json:"key,omitempty"`
	Name       string `json:"name,omitempty"`
	MetricsURL string `json:"metrics_url,omitempty"`
	Port       string `json:"port,omitempty"`
	Unit       string `json:"unit,omitempty"`
	// UserUnit is true for a unit of a user manager. Discovery sets it from
	// the cgroup of the process. For a configured unit, the process package
	// tries the system manager first and then the user manager.
	UserUnit bool `json:"-"`

	// LegacyUserUnit is the obsolete "user_unit" field. Load ignores it.
	LegacyUserUnit *bool `json:"user_unit,omitempty"`

	// KeyAuto and NameAuto are true when Load made the value. Discovery
	// can then replace the value with a better one.
	KeyAuto  bool `json:"-"`
	NameAuto bool `json:"-"`
	// PortAuto is true when Load read the port from metrics_url.
	PortAuto bool `json:"-"`
	// Discovered is true for a model that discovery added. Identity is the
	// served name and the model path of its process. A new identity on the
	// same URL is a different model.
	Discovered bool   `json:"-"`
	Identity   string `json:"-"`
}

// Node is the local node. All fields are optional.
type Node struct {
	Key      string  `json:"key,omitempty"`
	Name     string  `json:"name,omitempty"`
	Hostname string  `json:"hostname,omitempty"`
	Discover *bool   `json:"discover,omitempty"`
	Models   []Model `json:"models,omitempty"`
}

// DiscoveryEnabled reports whether local model discovery is on. It is on by
// default.
func (n Node) DiscoveryEnabled() bool {
	return n.Discover == nil || *n.Discover
}

// Peer is another dashboard. Only the URL is configured. In JSON, a peer
// is a URL string. The old object form is also accepted.
//
// Key, Name, and Hostname come from the /api/node response of the peer.
// Until the peer answers, Key comes from the URL host and Name or Hostname
// is the URL host.
type Peer struct {
	URL      string `json:"url"`
	Key      string `json:"-"`
	Name     string `json:"-"`
	Hostname string `json:"-"`

	// Legacy holds the names of the obsolete fields of the object form.
	Legacy []string `json:"-"`
}

// UnmarshalJSON accepts a URL string or an object.
func (p *Peer) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	*p = Peer{}
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &p.URL)
	}
	var object struct {
		URL      string  `json:"url"`
		Key      *string `json:"key"`
		Name     *string `json:"name"`
		Hostname *string `json:"hostname"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		return err
	}
	p.URL = object.URL
	for name, value := range map[string]*string{"key": object.Key, "name": object.Name, "hostname": object.Hostname} {
		if value != nil {
			p.Legacy = append(p.Legacy, name)
		}
	}
	sort.Strings(p.Legacy)
	return nil
}

type Config struct {
	Listen string `json:"listen,omitempty"`
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

// DefaultListen is the listen address when the configuration does not set one.
const DefaultListen = ":9090"

// Environment variables. They override the values of the file.
const (
	EnvConfig   = "VLLM_DASHBOARD_CONFIG"
	EnvListen   = "VLLM_DASHBOARD_LISTEN"
	EnvHostname = "VLLM_DASHBOARD_HOSTNAME"
	EnvPeers    = "VLLM_DASHBOARD_PEERS"
	EnvDiscover = "VLLM_DASHBOARD_DISCOVER"
)

// osHostname and getenv are variables so that tests can replace them.
var (
	osHostname = os.Hostname
	getenv     = os.Getenv
)

// Default returns the configuration that is used when no file is set.
func Default() Config {
	return Config{Listen: DefaultListen}
}

// Load reads and checks the configuration file at path. An empty path or a
// missing file gives the default configuration. Load then applies the
// environment variables and fills the empty fields with values from the
// host name, the URLs, and the ports.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			log.Printf("config file %s not found, using defaults", path)
		case err != nil:
			return Config{}, err
		default:
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&cfg); err != nil {
				return Config{}, fmt.Errorf("decode config: %w", err)
			}
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return Config{}, err
	}
	cfg.dropLegacyFields()
	if err := cfg.complete(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg *Config) applyEnv() error {
	if value := getenv(EnvListen); value != "" {
		cfg.Listen = value
	}
	if value := getenv(PollIntervalEnv); value != "" {
		if _, err := ParsePollInterval(value); err != nil {
			return fmt.Errorf("%s: %w", PollIntervalEnv, err)
		}
		cfg.PollInterval = value
	}
	if value := getenv(EnvHostname); value != "" {
		cfg.Node.Hostname = value
		if cfg.Node.Key == "" {
			cfg.Node.Key = SanitizeKey(ShortHostname(value))
		}
	}
	if value := getenv(EnvDiscover); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvDiscover, err)
		}
		cfg.Node.Discover = &enabled
	}
	for _, peerURL := range strings.FieldsFunc(getenv(EnvPeers), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		cfg.Peers = append(cfg.Peers, Peer{URL: peerURL})
	}
	return nil
}

// dropLegacyFields logs and clears the obsolete fields of an old file.
func (cfg *Config) dropLegacyFields() {
	for index := range cfg.Node.Models {
		model := &cfg.Node.Models[index]
		if model.LegacyUserUnit != nil {
			log.Printf("config: node.models[%d].user_unit is obsolete and ignored; the unit scope is detected", index)
			model.LegacyUserUnit = nil
		}
	}
	for index := range cfg.Peers {
		peer := &cfg.Peers[index]
		if len(peer.Legacy) > 0 {
			log.Printf("config: peers[%d] fields %s are obsolete and ignored; the peer reports its own key and name", index, strings.Join(peer.Legacy, ", "))
			peer.Legacy = nil
		}
	}
}

func (cfg *Config) complete() error {
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if _, err := ParsePollInterval(cfg.PollInterval); err != nil {
		return err
	}
	if err := completeNode(&cfg.Node); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index := range cfg.Node.Models {
		model := &cfg.Node.Models[index]
		if err := CompleteModel(model); err != nil {
			return fmt.Errorf("model %d: %w", index, err)
		}
		if seen[model.Key] {
			return fmt.Errorf("invalid or duplicate model key %q", model.Key)
		}
		seen[model.Key] = true
	}
	peerKeys := map[string]bool{cfg.Node.Key: true}
	peerURLs := map[string]bool{}
	for index := range cfg.Peers {
		peer := &cfg.Peers[index]
		if err := completePeer(peer, peerKeys); err != nil {
			return err
		}
		normalized := strings.TrimRight(peer.URL, "/")
		if peerURLs[normalized] {
			return fmt.Errorf("duplicate peer url %q", peer.URL)
		}
		peerKeys[peer.Key] = true
		peerURLs[normalized] = true
	}
	return nil
}

func completeNode(node *Node) error {
	hostname := ""
	if node.Key == "" || (node.Name == "" && node.Hostname == "") {
		hostname, _ = osHostname()
		hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	}
	if node.Key == "" {
		node.Key = SanitizeKey(ShortHostname(hostname))
		if node.Key == "" {
			node.Key = "node"
		}
	}
	if node.Name == "" && node.Hostname == "" {
		if hostname != "" && hostnameRe.MatchString(hostname) {
			node.Hostname = hostname
		} else {
			node.Name = node.Key
		}
	}
	if !KeyRe.MatchString(node.Key) {
		return fmt.Errorf("invalid node key %q", node.Key)
	}
	if DisplayName(node.Name, node.Hostname) == "" || (node.Hostname != "" && !hostnameRe.MatchString(node.Hostname)) {
		return fmt.Errorf("node requires a valid name or hostname")
	}
	return nil
}

// CompleteModel derives metrics_url from port, port from metrics_url, the
// key from the name or the port, and the name from the key. It then checks
// the model.
func CompleteModel(model *Model) error {
	if model.MetricsURL == "" && model.Port == "" {
		return fmt.Errorf("model requires metrics_url or port")
	}
	if model.Port != "" {
		if port, err := strconv.Atoi(model.Port); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %q", model.Port)
		}
	}
	if model.MetricsURL == "" {
		model.MetricsURL = "http://127.0.0.1:" + model.Port + "/metrics"
	}
	parsed, err := url.ParseRequestURI(model.MetricsURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("model %q has invalid metrics_url", model.Key)
	}
	if model.Port == "" {
		model.Port = parsed.Port()
		model.PortAuto = true
	}
	if model.Key == "" {
		model.KeyAuto = true
		model.Key = SanitizeKey(model.Name)
		if model.Key == "" && model.Port != "" {
			model.Key = "port-" + model.Port
		}
		if model.Key == "" {
			model.Key = SanitizeKey(parsed.Hostname())
		}
	}
	if !KeyRe.MatchString(model.Key) {
		return fmt.Errorf("invalid model key %q", model.Key)
	}
	if model.Name == "" {
		model.NameAuto = true
		model.Name = model.Key
	}
	return nil
}

// completePeer checks the URL and sets the fallback key and name from the
// URL host. used holds the keys that are already taken.
func completePeer(peer *Peer, used map[string]bool) error {
	parsed, err := url.ParseRequestURI(peer.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid peer url %q", peer.URL)
	}
	host := parsed.Hostname()
	isIP := net.ParseIP(host) != nil
	if isIP {
		peer.Key, peer.Name = SanitizeKey(host), host
	} else {
		peer.Key = SanitizeKey(ShortHostname(host))
		if hostnameRe.MatchString(host) {
			peer.Hostname = host
		} else {
			peer.Name = host
		}
	}
	peer.Key = UniqueKey(peer.Key, parsed.Port(), used)
	if !KeyRe.MatchString(peer.Key) {
		return fmt.Errorf("invalid peer url %q", peer.URL)
	}
	return nil
}

// UniqueKey returns key, or key with the suffix and then a number if key
// is in used.
func UniqueKey(key, suffix string, used map[string]bool) string {
	if key == "" {
		key = "peer"
	}
	if !used[key] {
		return key
	}
	base := key
	if suffix != "" {
		base = SanitizeKey(key + "-" + suffix)
		if !used[base] {
			return base
		}
	}
	for number := 2; ; number++ {
		if candidate := SanitizeKey(base + "-" + strconv.Itoa(number)); !used[candidate] {
			return candidate
		}
	}
}

// ValidHostname reports whether hostname is a valid DNS name.
func ValidHostname(hostname string) bool {
	return hostnameRe.MatchString(hostname)
}

// SanitizeKey makes a valid key from s. It returns "" if s has no letters
// or digits.
func SanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	key := strings.Trim(b.String(), "-_")
	if len(key) > 63 {
		key = strings.TrimRight(key[:63], "-_")
	}
	return key
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
