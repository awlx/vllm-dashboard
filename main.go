package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/index.html
var staticFiles embed.FS

type Sample struct {
	Time              int64   `json:"t"`
	Running           float64 `json:"running"`
	Waiting           float64 `json:"waiting"`
	KVCachePct        float64 `json:"kv_cache_pct"`
	PromptTokPerSec   float64 `json:"prompt_tps"`
	GenTokPerSec      float64 `json:"gen_tps"`
	GPUUtilPct        float64 `json:"gpu_util_pct"`
	GPUTempC          float64 `json:"gpu_temp_c"`
	GPUPowerW         float64 `json:"gpu_power_w"`
	GPUClockMHz       float64 `json:"gpu_clock_mhz"`
	TTFTMs            float64 `json:"ttft_ms"`
	InterTokenMs      float64 `json:"inter_token_ms"`
	E2ELatencyMs      float64 `json:"e2e_latency_ms"`
	PrefixCacheHitPct float64 `json:"prefix_cache_hit_pct"`
	PreemptionsTotal  float64 `json:"preemptions_total"`
	CompletedRequests float64 `json:"completed_requests"`
	ReqPerSec         float64 `json:"req_per_sec"`
	SpecAcceptPct     float64 `json:"spec_accept_pct"`
	SpecMeanAccepted  float64 `json:"spec_mean_accepted"`
	CPUUtilPct        float64 `json:"cpu_util_pct"`
	CPUClockMHz       float64 `json:"cpu_clock_mhz"`
	MemUsedPct        float64 `json:"mem_used_pct"`
	MemUsedGB         float64 `json:"mem_used_gb"`
	MemTotalGB        float64 `json:"mem_total_gb"`
	NetRxMbps         float64 `json:"net_rx_mbps"`
	NetTxMbps         float64 `json:"net_tx_mbps"`
	PromptTokensCum   float64 `json:"-"`
	GenTokensCum      float64 `json:"-"`
}

type TokenWindow struct {
	PromptTokens float64 `json:"prompt_tokens"`
	GenTokens    float64 `json:"gen_tokens"`
	TotalTokens  float64 `json:"total_tokens"`
}

type TokenUsage struct {
	LastHour TokenWindow `json:"last_hour"`
	Last24h  TokenWindow `json:"last_24h"`
}

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
	Listen string       `json:"listen"`
	Node   NodeConfig   `json:"node"`
	Peers  []PeerConfig `json:"peers,omitempty"`
}

type ModelState struct {
	History               []Sample
	LongHistory           []Sample
	LastLongSample        time.Time
	CounterStartedAt      time.Time
	LastPromptTokens      float64
	LastGenTokens         float64
	LastTTFTSum           float64
	LastTTFTCount         float64
	LastITLSum            float64
	LastITLCount          float64
	LastE2ESum            float64
	LastE2ECount          float64
	LastCompletedRequests float64
	LastSpecAccepted      float64
	LastSpecDraft         float64
	LastScrape            time.Time
}

type ModelOverview struct {
	Key      string     `json:"key"`
	Name     string     `json:"name"`
	NodeKey  string     `json:"node_key"`
	NodeName string     `json:"node_name"`
	Online   bool       `json:"online"`
	Latest   Sample     `json:"latest"`
	Info     ServerInfo `json:"info"`
	Tokens   TokenUsage `json:"tokens"`
}

// ServerInfo is static-ish serving config, polled at a lower rate than metrics.
type ServerInfo struct {
	Model                string `json:"model"`
	ServedModelName      string `json:"served_model_name"`
	MaxModelLen          string `json:"max_model_len"`
	GPUMemUtilization    string `json:"gpu_memory_utilization"`
	MaxNumBatchedTokens  string `json:"max_num_batched_tokens"`
	SpeculativeConfig    string `json:"speculative_config"`
	VLLMVersion          string `json:"vllm_version"`
	ServiceActiveSince   string `json:"service_active_since"`
	ServiceUptimeSeconds int64  `json:"service_uptime_seconds"`
	ServiceRestarts      string `json:"service_restarts"`
}

const (
	historySize  = 150 // 5 min at 2s resolution
	scrapeEvery  = 2 * time.Second
	longInterval = 60 * time.Second // 24h at 1 min resolution
	longSize     = 24 * 60
)

var (
	mu                        sync.RWMutex
	lastCPUTotal, lastCPUIdle float64
	lastNetRx, lastNetTx      float64
	lastSystemScrape          time.Time
	vllmVersion               string
	config                    DashboardConfig
	models                    []ModelConfig
	modelStates               map[string]*ModelState
	peerClient                = &http.Client{Timeout: 4 * time.Second}
)

var metricLineRe = regexp.MustCompile(`^([a-zA-Z0-9_:]+)(\{[^}]*\})?\s+([0-9eE\.\+\-]+)\s*$`)
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

func configureRuntime(cfg DashboardConfig) {
	config = cfg
	models = cfg.Node.Models
	modelStates = make(map[string]*ModelState, len(models))
	for _, model := range models {
		modelStates[model.Key] = &ModelState{}
	}
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

func localModelKey(key string) (string, bool) {
	prefix := config.Node.Key + "/"
	key = strings.TrimPrefix(key, prefix)
	for _, model := range models {
		if model.Key == key {
			return key, true
		}
	}
	return "", false
}

func peerForModel(key string) (PeerConfig, string, bool) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return PeerConfig{}, "", false
	}
	for _, peer := range config.Peers {
		if peer.Key == parts[0] {
			return peer, parts[1], true
		}
	}
	return PeerConfig{}, "", false
}

func peerAPI(peer PeerConfig, path, model string, target any) error {
	endpoint, err := url.Parse(strings.TrimRight(peer.URL, "/") + path)
	if err != nil {
		return err
	}
	if model != "" {
		query := endpoint.Query()
		query.Set("model", model)
		endpoint.RawQuery = query.Encode()
	}
	response, err := peerClient.Get(endpoint.String())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s returned %s", peer.Key, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func scrapeVLLM(metricsURL string) (map[string]float64, error) {
	client := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(metricsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	metrics := map[string]float64{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := metricLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		val, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		// sum across label variants (e.g. request_success_total has one line per finished_reason)
		metrics[name] += val
	}
	return metrics, scanner.Err()
}

func scrapeGPU() (util, temp, power, clock float64) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=utilization.gpu,temperature.gpu,power.draw,clocks.current.graphics",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) >= 4 {
		util, _ = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		temp, _ = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		power, _ = strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		clock, _ = strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	}
	return
}

func averageClockKHz(values []string) float64 {
	var total float64
	var count int
	for _, value := range values {
		clock, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || clock <= 0 {
			continue
		}
		total += clock
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float64(count) / 1000
}

func scrapeCPUClockMHz() float64 {
	paths, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	if err != nil {
		return 0
	}
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		value, err := os.ReadFile(path)
		if err == nil {
			values = append(values, string(value))
		}
	}
	return averageClockKHz(values)
}

// scrapeCPUTimes reads aggregate CPU jiffies from /proc/stat. Deltas between
// polls give a standard busy/total CPU utilization ratio.
func scrapeCPUTimes() (total, idle float64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0
	}
	fields := strings.Fields(scanner.Text()) // "cpu" user nice system idle iowait irq softirq steal ...
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var vals []float64
	for _, f := range fields[1:] {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue
		}
		vals = append(vals, v)
		total += v
	}
	if len(vals) >= 4 {
		idle = vals[3] // idle
	}
	return
}

// scrapeMem reads /proc/meminfo. On this unified-memory system (GB10), host
// RAM usage is the best available proxy for overall memory pressure since
// nvidia-smi reports GPU memory as N/A.
func scrapeMem() (usedGB, totalGB, usedPct float64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()
	var totalKB, availKB float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseFloat(fields[1], 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	if totalKB == 0 {
		return 0, 0, 0
	}
	usedKB := totalKB - availKB
	totalGB = totalKB / 1024 / 1024
	usedGB = usedKB / 1024 / 1024
	usedPct = usedKB / totalKB * 100
	return
}

// scrapeNet sums rx/tx bytes across all non-loopback interfaces from /proc/net/dev.
func scrapeNet() (rxBytes, txBytes float64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // header lines
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseFloat(fields[0], 64)
		tx, _ := strconv.ParseFloat(fields[8], 64)
		rxBytes += rx
		txBytes += tx
	}
	return
}

func poll() {
	gpuUtil, gpuTemp, gpuPower, gpuClock := scrapeGPU()
	cpuTotal, cpuIdle := scrapeCPUTimes()
	cpuClock := scrapeCPUClockMHz()
	memUsedGB, memTotalGB, memUsedPct := scrapeMem()
	netRx, netTx := scrapeNet()
	now := time.Now()

	shared := Sample{
		Time:       now.UnixMilli(),
		GPUUtilPct: gpuUtil, GPUTempC: gpuTemp, GPUPowerW: gpuPower, GPUClockMHz: gpuClock,
		CPUClockMHz: cpuClock,
		MemUsedGB:   memUsedGB, MemTotalGB: memTotalGB, MemUsedPct: memUsedPct,
	}

	if !lastSystemScrape.IsZero() {
		dt := now.Sub(lastSystemScrape).Seconds()
		if dt > 0 {
			dTotal := cpuTotal - lastCPUTotal
			dIdle := cpuIdle - lastCPUIdle
			if dTotal > 0 {
				shared.CPUUtilPct = (1 - dIdle/dTotal) * 100
			}
			shared.NetRxMbps = (netRx - lastNetRx) * 8 / 1e6 / dt
			shared.NetTxMbps = (netTx - lastNetTx) * 8 / 1e6 / dt
		}
	}
	lastCPUTotal, lastCPUIdle = cpuTotal, cpuIdle
	lastNetRx, lastNetTx = netRx, netTx
	lastSystemScrape = now

	for _, model := range models {
		pollModel(model, shared, now)
	}
}

func pollModel(model ModelConfig, shared Sample, now time.Time) {
	metrics, err := scrapeVLLM(model.MetricsURL)
	state := modelStates[model.Key]
	s := shared

	if err != nil {
		log.Printf("%s scrape error: %v", model.Key, err)
		mu.Lock()
		s.PromptTokensCum = state.LastPromptTokens
		s.GenTokensCum = state.LastGenTokens
		mu.Unlock()
	} else {
		s.Running = metrics["vllm:num_requests_running"]
		s.Waiting = metrics["vllm:num_requests_waiting"]
		s.KVCachePct = metrics["vllm:kv_cache_usage_perc"] * 100
		s.PreemptionsTotal = metrics["vllm:num_preemptions_total"]
		s.CompletedRequests = metrics["vllm:request_success_total"]

		queries := metrics["vllm:prefix_cache_queries_total"]
		hits := metrics["vllm:prefix_cache_hits_total"]
		if queries > 0 {
			s.PrefixCacheHitPct = hits / queries * 100
		}

		promptTok := metrics["vllm:prompt_tokens_total"]
		genTok := metrics["vllm:generation_tokens_total"]
		s.PromptTokensCum = promptTok
		s.GenTokensCum = genTok
		ttftSum := metrics["vllm:time_to_first_token_seconds_sum"]
		ttftCount := metrics["vllm:time_to_first_token_seconds_count"]
		itlSum := metrics["vllm:inter_token_latency_seconds_sum"]
		itlCount := metrics["vllm:inter_token_latency_seconds_count"]
		e2eSum := metrics["vllm:e2e_request_latency_seconds_sum"]
		e2eCount := metrics["vllm:e2e_request_latency_seconds_count"]
		specAccepted := metrics["vllm:spec_decode_num_accepted_tokens_total"]
		specDraft := metrics["vllm:spec_decode_num_draft_tokens_total"]

		mu.Lock()
		if !state.LastScrape.IsZero() {
			dt := now.Sub(state.LastScrape).Seconds()
			if dt > 0 {
				pps := (promptTok - state.LastPromptTokens) / dt
				gps := (genTok - state.LastGenTokens) / dt
				if pps > 0 {
					s.PromptTokPerSec = pps
				}
				if gps > 0 {
					s.GenTokPerSec = gps
				}
				dReq := s.CompletedRequests - state.LastCompletedRequests
				if dReq > 0 {
					s.ReqPerSec = dReq / dt
				}
			}
			dCount := ttftCount - state.LastTTFTCount
			if dCount > 0 {
				s.TTFTMs = (ttftSum - state.LastTTFTSum) / dCount * 1000
			}
			dITLCount := itlCount - state.LastITLCount
			if dITLCount > 0 {
				s.InterTokenMs = (itlSum - state.LastITLSum) / dITLCount * 1000
			}
			dE2ECount := e2eCount - state.LastE2ECount
			if dE2ECount > 0 {
				s.E2ELatencyMs = (e2eSum - state.LastE2ESum) / dE2ECount * 1000
			}
			dSpecDraft := specDraft - state.LastSpecDraft
			dSpecAccepted := specAccepted - state.LastSpecAccepted
			if dSpecDraft > 0 {
				s.SpecAcceptPct = dSpecAccepted / dSpecDraft * 100
			}
			dReqTotal := s.CompletedRequests - state.LastCompletedRequests
			if dReqTotal > 0 {
				s.SpecMeanAccepted = dSpecAccepted / dReqTotal
			}
		}
		state.LastPromptTokens = promptTok
		state.LastGenTokens = genTok
		state.LastTTFTSum, state.LastTTFTCount = ttftSum, ttftCount
		state.LastITLSum, state.LastITLCount = itlSum, itlCount
		state.LastE2ESum, state.LastE2ECount = e2eSum, e2eCount
		state.LastSpecAccepted, state.LastSpecDraft = specAccepted, specDraft
		state.LastCompletedRequests = s.CompletedRequests
		state.LastScrape = now
		mu.Unlock()
	}

	mu.Lock()
	state.History = append(state.History, s)
	if len(state.History) > historySize {
		state.History = state.History[len(state.History)-historySize:]
	}
	if state.LastLongSample.IsZero() || now.Sub(state.LastLongSample) >= longInterval {
		state.LongHistory = append(state.LongHistory, s)
		if len(state.LongHistory) > longSize {
			state.LongHistory = state.LongHistory[len(state.LongHistory)-longSize:]
		}
		state.LastLongSample = now
	}
	mu.Unlock()
}

// tokensInWindow returns cumulative prompt/gen token deltas over the trailing
// window, using the closest sample at or before (now - window) as the anchor.
// It sums adjacent deltas so counter resets inside the window remain visible
// even after the new process counters surpass the pre-reset values.
func tokensInWindow(samples []Sample, now time.Time, window time.Duration) (promptTokens, genTokens float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	cutoffMs := now.Add(-window).UnixMilli()
	anchorIndex := 0
	for index, sample := range samples {
		if sample.Time > cutoffMs {
			break
		}
		anchorIndex = index
	}
	for index := anchorIndex + 1; index < len(samples); index++ {
		previous := samples[index-1]
		current := samples[index]
		promptTokens += counterDelta(previous.PromptTokensCum, current.PromptTokensCum)
		genTokens += counterDelta(previous.GenTokensCum, current.GenTokensCum)
	}
	return promptTokens, genTokens
}

func counterDelta(previous, current float64) float64 {
	if current < previous {
		return current
	}
	return current - previous
}

// findVLLMPid locates the running "vllm serve" API server process.
func findVLLMPid(port string) (string, error) {
	out, err := exec.Command("pgrep", "-af", "vllm serve").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.Contains(line, "--port "+port) {
			return fields[0], nil
		}
	}
	return "", os.ErrNotExist
}

func modelProcessStartedAt(model ModelConfig) time.Time {
	pid, err := findVLLMPid(model.Port)
	if err != nil {
		return time.Time{}
	}
	out, err := exec.Command("ps", "-o", "etimes=", "-p", pid).Output()
	if err != nil {
		return time.Time{}
	}
	elapsed, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(elapsed) * time.Second)
}

// readCmdlineArgs reads a process's argv from /proc/<pid>/cmdline, which is
// NUL-separated and therefore avoids shell-quoting ambiguity entirely.
func readCmdlineArgs(pid string) ([]string, error) {
	data, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil {
		return nil, err
	}
	var args []string
	for _, p := range strings.Split(string(data), "\x00") {
		if p != "" {
			args = append(args, p)
		}
	}
	return args, nil
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func speculativeConfig(args []string) string {
	if config := argValue(args, "--speculative-config"); config != "" {
		return config
	}
	model := argValue(args, "--speculative-config.model")
	tokens := argValue(args, "--speculative-config.num_speculative_tokens")
	if model == "" && tokens == "" {
		return ""
	}
	method := "speculative"
	lowerModel := strings.ToLower(model)
	for _, candidate := range []string{"dspark", "dflash", "mtp"} {
		if strings.Contains(lowerModel, candidate) {
			method = candidate
			break
		}
	}
	config := map[string]any{"method": method, "model": model}
	if parsed, err := strconv.Atoi(tokens); err == nil {
		config["num_speculative_tokens"] = parsed
	}
	encoded, _ := json.Marshal(config)
	return string(encoded)
}

func modelDisplayName(info ServerInfo, fallback string) string {
	if info.ServedModelName != "" {
		return info.ServedModelName
	}
	if info.Model != "" {
		parts := strings.Split(info.Model, "/")
		return parts[len(parts)-1]
	}
	return fallback
}

func systemdProperty(model ModelConfig, prop string) string {
	args := []string{"show", model.Unit, "--property=" + prop, "--value"}
	if model.UserUnit {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func buildServerInfo(model ModelConfig) ServerInfo {
	info := ServerInfo{}
	processPID := ""

	if pid, err := findVLLMPid(model.Port); err == nil {
		processPID = pid
		if args, aerr := readCmdlineArgs(pid); aerr == nil {
			info.Model = argValue(args, "--model")
			if info.Model == "" {
				for i, a := range args {
					if a == "serve" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						info.Model = args[i+1]
						break
					}
				}
			}
			info.ServedModelName = argValue(args, "--served-model-name")
			info.MaxModelLen = argValue(args, "--max-model-len")
			info.GPUMemUtilization = argValue(args, "--gpu-memory-utilization")
			info.MaxNumBatchedTokens = argValue(args, "--max-num-batched-tokens")
			info.SpeculativeConfig = speculativeConfig(args)
		}
	}

	activeSince := systemdProperty(model, "ActiveEnterTimestamp")
	info.ServiceActiveSince = activeSince
	if activeSince != "" {
		if t, terr := time.Parse("Mon 2006-01-02 15:04:05 MST", activeSince); terr == nil {
			info.ServiceUptimeSeconds = int64(time.Since(t).Seconds())
		}
	}
	if info.ServiceUptimeSeconds == 0 && processPID != "" {
		if out, err := exec.Command("ps", "-o", "etimes=", "-p", processPID).Output(); err == nil {
			if elapsed, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); parseErr == nil {
				info.ServiceUptimeSeconds = elapsed
			}
		}
	}
	info.ServiceRestarts = systemdProperty(model, "NRestarts")
	info.VLLMVersion = vllmVersion

	return info
}

func modelConfig(key string) ModelConfig {
	if localKey, ok := localModelKey(key); ok {
		key = localKey
	}
	for _, model := range models {
		if model.Key == key {
			return model
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ModelConfig{}
}

func aggregateSampleSets(sources [][]Sample) []Sample {
	populated := sources[:0]
	for _, source := range sources {
		if len(source) > 0 {
			populated = append(populated, source)
		}
	}
	sources = populated
	if len(sources) == 0 {
		return nil
	}
	length := len(sources[0])
	for _, source := range sources[1:] {
		if len(source) < length {
			length = len(source)
		}
	}
	result := make([]Sample, length)
	for index := range length {
		base := sources[0][len(sources[0])-length+index]
		result[index] = base
		for _, source := range sources[1:] {
			sample := source[len(source)-length+index]
			result[index].Running += sample.Running
			result[index].Waiting += sample.Waiting
			result[index].PromptTokPerSec += sample.PromptTokPerSec
			result[index].GenTokPerSec += sample.GenTokPerSec
			result[index].ReqPerSec += sample.ReqPerSec
			result[index].CompletedRequests += sample.CompletedRequests
			result[index].PreemptionsTotal += sample.PreemptionsTotal
			result[index].PromptTokensCum += sample.PromptTokensCum
			result[index].GenTokensCum += sample.GenTokensCum
			result[index].GPUUtilPct = max(result[index].GPUUtilPct, sample.GPUUtilPct)
			result[index].GPUTempC = max(result[index].GPUTempC, sample.GPUTempC)
			result[index].GPUPowerW += sample.GPUPowerW
			result[index].GPUClockMHz = max(result[index].GPUClockMHz, sample.GPUClockMHz)
			result[index].CPUUtilPct = max(result[index].CPUUtilPct, sample.CPUUtilPct)
			result[index].CPUClockMHz = max(result[index].CPUClockMHz, sample.CPUClockMHz)
			result[index].MemUsedPct = max(result[index].MemUsedPct, sample.MemUsedPct)
			result[index].NetRxMbps += sample.NetRxMbps
			result[index].NetTxMbps += sample.NetTxMbps
			result[index].KVCachePct = max(result[index].KVCachePct, sample.KVCachePct)
			result[index].TTFTMs = max(result[index].TTFTMs, sample.TTFTMs)
			result[index].InterTokenMs = max(result[index].InterTokenMs, sample.InterTokenMs)
			result[index].E2ELatencyMs = max(result[index].E2ELatencyMs, sample.E2ELatencyMs)
		}
	}
	return result
}

func aggregateLocalSamples(long bool) []Sample {
	var sources [][]Sample
	for _, model := range models {
		state := modelStates[model.Key]
		if long {
			sources = append(sources, state.LongHistory)
		} else {
			sources = append(sources, state.History)
		}
	}
	return aggregateSampleSets(sources)
}

func samplesFor(key string, long bool) []Sample {
	if key == "all" {
		return aggregateLocalSamples(long)
	}
	localKey, ok := localModelKey(key)
	if !ok {
		return nil
	}
	state := modelStates[localKey]
	if long {
		return state.LongHistory
	}
	return state.History
}

func usageFor(samples []Sample) TokenUsage {
	return usageForStartedAt(samples, time.Time{})
}

func usageForStartedAt(samples []Sample, startedAt time.Time) TokenUsage {
	now := time.Now()
	p1, g1 := tokensInWindow(samples, now, time.Hour)
	p24, g24 := tokensInWindow(samples, now, 24*time.Hour)
	if len(samples) > 0 && !startedAt.IsZero() && !time.UnixMilli(samples[0].Time).Before(startedAt) {
		if startedAt.After(now.Add(-time.Hour)) {
			p1 += samples[0].PromptTokensCum
			g1 += samples[0].GenTokensCum
		}
		if startedAt.After(now.Add(-24 * time.Hour)) {
			p24 += samples[0].PromptTokensCum
			g24 += samples[0].GenTokensCum
		}
	}
	return TokenUsage{
		LastHour: TokenWindow{PromptTokens: p1, GenTokens: g1, TotalTokens: p1 + g1},
		Last24h:  TokenWindow{PromptTokens: p24, GenTokens: g24, TotalTokens: p24 + g24},
	}
}

func addUsage(total, usage TokenUsage) TokenUsage {
	total.LastHour.PromptTokens += usage.LastHour.PromptTokens
	total.LastHour.GenTokens += usage.LastHour.GenTokens
	total.LastHour.TotalTokens += usage.LastHour.TotalTokens
	total.Last24h.PromptTokens += usage.Last24h.PromptTokens
	total.Last24h.GenTokens += usage.Last24h.GenTokens
	total.Last24h.TotalTokens += usage.Last24h.TotalTokens
	return total
}

func usageForState(state *ModelState) TokenUsage {
	return usageForStartedAt(state.LongHistory, state.CounterStartedAt)
}

func usageForKey(key string) TokenUsage {
	if key != "all" {
		return usageForState(modelStates[modelConfig(key).Key])
	}
	var total TokenUsage
	for _, model := range models {
		total = addUsage(total, usageForState(modelStates[model.Key]))
	}
	return total
}

func requestedModel(r *http.Request) string {
	key := r.URL.Query().Get("model")
	if key == "" || key == "all" {
		return key
	}
	return key
}

func combinedSamples(long bool) []Sample {
	mu.RLock()
	local := append([]Sample(nil), aggregateLocalSamples(long)...)
	mu.RUnlock()
	sources := [][]Sample{local}
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	for _, peer := range config.Peers {
		var samples []Sample
		if err := peerAPI(peer, path, "all", &samples); err != nil {
			log.Printf("peer %s metrics: %v", peer.Key, err)
			continue
		}
		sources = append(sources, samples)
	}
	return aggregateSampleSets(sources)
}

func metricsForRequest(key string, long bool) ([]Sample, error) {
	if key == "" || key == "all" {
		return combinedSamples(long), nil
	}
	if localKey, ok := localModelKey(key); ok {
		mu.RLock()
		defer mu.RUnlock()
		return append([]Sample(nil), samplesFor(localKey, long)...), nil
	}
	peer, peerModel, ok := peerForModel(key)
	if !ok {
		return nil, os.ErrNotExist
	}
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	var samples []Sample
	return samples, peerAPI(peer, path, peerModel, &samples)
}

func localOverview() []ModelOverview {
	overview := make([]ModelOverview, 0, len(models))
	for _, model := range models {
		info := buildServerInfo(model)
		mu.RLock()
		state := modelStates[model.Key]
		item := ModelOverview{
			Key: scopedModelKey(config.Node.Key, model.Key), Name: modelDisplayName(info, model.Name),
			NodeKey: config.Node.Key, NodeName: displayName(config.Node.Name, config.Node.Hostname), Info: info,
		}
		if len(state.History) > 0 {
			item.Latest = state.History[len(state.History)-1]
		}
		item.Online = !state.LastScrape.IsZero() && time.Since(state.LastScrape) < 3*scrapeEvery
		item.Tokens = usageForState(state)
		mu.RUnlock()
		overview = append(overview, item)
	}
	return overview
}

func combinedOverview() []ModelOverview {
	overview := localOverview()
	for _, peer := range config.Peers {
		var peerModels []ModelOverview
		if err := peerAPI(peer, "/api/overview", "", &peerModels); err != nil {
			log.Printf("peer %s overview: %v", peer.Key, err)
			continue
		}
		for index := range peerModels {
			modelKey := peerModels[index].Key
			if strings.Contains(modelKey, "/") {
				modelKey = strings.SplitN(modelKey, "/", 2)[1]
			}
			peerModels[index].Key = scopedModelKey(peer.Key, modelKey)
			peerModels[index].NodeKey = peer.Key
			peerModels[index].NodeName = displayName(peer.Name, peer.Hostname)
		}
		overview = append(overview, peerModels...)
	}
	return overview
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func main() {
	cfg, err := loadConfig(os.Getenv("VLLM_DASHBOARD_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	configureRuntime(cfg)
	if out, err := exec.Command("vllm", "--version").Output(); err == nil {
		vllmVersion = strings.TrimSpace(string(out))
	}
	for _, model := range models {
		modelStates[model.Key].CounterStartedAt = modelProcessStartedAt(model)
	}

	go func() {
		for {
			poll()
			time.Sleep(scrapeEvery)
		}
	}()

	http.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		samples, err := metricsForRequest(requestedModel(r), false)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, samples)
	})

	http.HandleFunc("/api/metrics/long", func(w http.ResponseWriter, r *http.Request) {
		samples, err := metricsForRequest(requestedModel(r), true)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, samples)
	})

	http.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		if localKey, ok := localModelKey(key); ok {
			writeJSON(w, buildServerInfo(modelConfig(localKey)))
			return
		}
		peer, peerModel, ok := peerForModel(key)
		if !ok {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		var info ServerInfo
		if err := peerAPI(peer, "/api/info", peerModel, &info); err != nil {
			http.Error(w, "peer unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, info)
	})

	http.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		var usage TokenUsage
		if key == "" || key == "all" {
			mu.RLock()
			usage = usageForKey("all")
			mu.RUnlock()
			for _, peer := range config.Peers {
				var peerUsage TokenUsage
				if err := peerAPI(peer, "/api/tokens", "all", &peerUsage); err == nil {
					usage = addUsage(usage, peerUsage)
				}
			}
		} else if localKey, ok := localModelKey(key); ok {
			mu.RLock()
			usage = usageForKey(localKey)
			mu.RUnlock()
		} else if peer, peerModel, ok := peerForModel(key); ok {
			if err := peerAPI(peer, "/api/tokens", peerModel, &usage); err != nil {
				http.Error(w, "peer unavailable", http.StatusBadGateway)
				return
			}
		} else {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		writeJSON(w, usage)
	})

	http.HandleFunc("/api/overview", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, combinedOverview())
	})

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(sub)))

	log.Printf("vllm-dashboard node %q listening on %s", displayName(config.Node.Name, config.Node.Hostname), config.Listen)
	log.Fatal(http.ListenAndServe(config.Listen, nil))
}
