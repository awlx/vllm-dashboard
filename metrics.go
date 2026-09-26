package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// Allow for sequential engine scrapes, rather than the UI's shorter online
	// window. Stale/failed performance series are omitted, never zero-filled.
	telemetryFreshness       = 30 * time.Second
	telemetryClockSkew       = 5 * time.Second
	telemetryPeerTimeout     = 4 * time.Second
	telemetryPeerConcurrency = 8
	telemetryMaxResponse     = 4 << 20
	telemetryVersion         = 1
)

// TelemetrySnapshot contains LOCAL data only. There are no peer addresses,
// service names, full checkpoint paths, or credentials here.
// All wire timestamps are Unix milliseconds; zero means not sampled yet.
type TelemetrySnapshot struct {
	Version       int              `json:"version"`
	Models        []ModelTelemetry `json:"models"`
	Host          Sample           `json:"host"`
	KernelVersion string           `json:"kernel_version,omitempty"`
}

type ModelTelemetry struct {
	Key             string             `json:"key"`
	Name            string             `json:"name,omitempty"`
	Latest          Sample             `json:"latest"`
	LastScrape      int64              `json:"last_scrape"`
	LastPollSuccess bool               `json:"last_poll_success"`
	Counters        *TelemetryCounters `json:"counters"`
}

// Counters are explicit because Sample's cumulative token fields have json:"-".
// They are engine totals, not sums of dashboard samples, and may reset on an
// engine restart. Only a fresh, successful model sample exports these totals.
type TelemetryCounters struct {
	PromptTokens      float64 `json:"prompt_tokens"`
	GenerationTokens  float64 `json:"generation_tokens"`
	CompletedRequests float64 `json:"completed_requests"`
	Preemptions       float64 `json:"preemptions"`
}

// Protected by mu and updated before polling models, including on nodes with
// no models. Host freshness must not depend on any model's scrape status.
var latestHostSample Sample

var telemetryClient = &http.Client{
	Timeout: telemetryPeerTimeout,
	// A telemetry request must not follow redirects to other routes or nodes.
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// localTelemetryLocked copies values, not history slices or state pointers.
// Callers hold mu for both the snapshot and the accompanying config keys.
func localTelemetryLocked() TelemetrySnapshot {
	snapshot := TelemetrySnapshot{
		Version:       telemetryVersion,
		Models:        make([]ModelTelemetry, 0, len(models)),
		Host:          latestHostSample,
		KernelVersion: kernelVersion,
	}
	for _, model := range models {
		item := ModelTelemetry{Key: model.Key, Name: model.Name, Counters: &TelemetryCounters{}}
		if state := modelStates[model.Key]; state != nil {
			if state.ModelName != "" {
				item.Name = state.ModelName
			}
			item.LastPollSuccess = state.LastPollSuccess
			if !state.LastScrape.IsZero() {
				item.LastScrape = state.LastScrape.UnixMilli()
			}
			if len(state.History) > 0 {
				item.Latest = state.History[len(state.History)-1]
			}
			item.Counters = &TelemetryCounters{
				PromptTokens:      item.Latest.PromptTokensCum,
				GenerationTokens:  item.Latest.GenTokensCum,
				CompletedRequests: item.Latest.CompletedRequests,
				Preemptions:       item.Latest.PreemptionsTotal,
			}
		}
		snapshot.Models = append(snapshot.Models, item)
	}
	return snapshot
}

func validTelemetryTimestamp(timestamp int64, now time.Time) bool {
	return timestamp >= 0 && timestamp <= now.Add(telemetryClockSkew).UnixMilli()
}

func freshTelemetryTimestamp(timestamp int64, now time.Time) bool {
	return timestamp > 0 && validTelemetryTimestamp(timestamp, now) &&
		timestamp >= now.Add(-telemetryFreshness).UnixMilli()
}

func validateTelemetry(snapshot TelemetrySnapshot, now time.Time) error {
	if snapshot.Version != telemetryVersion || snapshot.Models == nil || !validTelemetryTimestamp(snapshot.Host.Time, now) {
		return fmt.Errorf("invalid telemetry envelope")
	}
	if snapshot.KernelVersion != "" && !kernelVersionRe.MatchString(snapshot.KernelVersion) {
		return fmt.Errorf("invalid telemetry kernel version")
	}
	seen := make(map[string]bool, len(snapshot.Models))
	for _, model := range snapshot.Models {
		if !configKeyRe.MatchString(model.Key) || seen[model.Key] {
			return fmt.Errorf("invalid or duplicate telemetry model key")
		}
		seen[model.Key] = true
		if !validTelemetryTimestamp(model.Latest.Time, now) || !validTelemetryTimestamp(model.LastScrape, now) ||
			model.LastScrape > model.Latest.Time || (model.LastPollSuccess && model.LastScrape == 0) {
			return fmt.Errorf("invalid telemetry model timestamp")
		}
		if model.Counters == nil {
			return fmt.Errorf("missing telemetry counters")
		}
		for _, value := range []float64{model.Counters.PromptTokens, model.Counters.GenerationTokens, model.Counters.CompletedRequests, model.Counters.Preemptions} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("invalid telemetry counter")
			}
		}
	}
	return nil
}

func fetchPeerTelemetry(ctx context.Context, peer PeerConfig) (TelemetrySnapshot, error) {
	var snapshot TelemetrySnapshot
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(peer.URL, "/")+"/api/telemetry", nil)
	if err != nil {
		return snapshot, err
	}
	response, err := telemetryClient.Do(request)
	if err != nil {
		return snapshot, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("peer telemetry returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, telemetryMaxResponse+1))
	if err != nil {
		return snapshot, err
	}
	if len(data) > telemetryMaxResponse {
		return snapshot, fmt.Errorf("peer telemetry exceeds size limit")
	}
	// Unmarshal rejects trailing data as well as malformed/non-finite numbers.
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, validateTelemetry(snapshot, time.Now())
}

type telemetryNode struct {
	key      string
	name     string
	up       bool
	snapshot TelemetrySnapshot
}

func collectTelemetry(ctx context.Context) []telemetryNode {
	mu.RLock()
	local := telemetryNode{key: config.Node.Key, name: displayName(config.Node.Name, config.Node.Hostname), up: true, snapshot: localTelemetryLocked()}
	peers := append([]PeerConfig(nil), config.Peers...)
	mu.RUnlock()

	nodes := make([]telemetryNode, len(peers)+1)
	nodes[0] = local
	for index, peer := range peers {
		nodes[index+1].key = peer.Key
		nodes[index+1].name = displayName(peer.Name, peer.Hostname)
	}
	// One shared deadline includes time spent waiting for a worker. A large
	// peer list cannot multiply the HTTP scrape's timeout or concurrency.
	ctx, cancel := context.WithTimeout(ctx, telemetryPeerTimeout)
	defer cancel()
	workers := min(len(peers), telemetryPeerConcurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := worker; index < len(peers); index += workers {
				if ctx.Err() != nil {
					return
				}
				snapshot, err := fetchPeerTelemetry(ctx, peers[index])
				if err == nil {
					nodes[index+1].snapshot = snapshot
					nodes[index+1].up = true
				}
			}
		}()
	}
	wg.Wait()
	return nodes
}

func telemetryGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func registerMetricsHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/telemetry", func(w http.ResponseWriter, r *http.Request) {
		if !telemetryGET(w, r) {
			return
		}
		mu.RLock()
		snapshot := localTelemetryLocked()
		mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodHead {
			writeJSON(w, snapshot)
		}
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !telemetryGET(w, r) {
			return
		}
		// An isolated registry and immutable request snapshot avoid cross-scrape
		// state, stale peer labels, and default Go/process collector identifiers.
		nodes := collectTelemetry(r.Context())
		registry := prometheus.NewRegistry()
		registry.MustRegister(&telemetryCollector{nodes: nodes, now: time.Now()})
		if r.Method == http.MethodHead {
			w = telemetryHeadWriter{w}
		}
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}

// Discard only the body; keep the same negotiation, headers and status as GET,
// including when handlers are invoked directly rather than by an HTTP server.
type telemetryHeadWriter struct {
	http.ResponseWriter
}

func (w telemetryHeadWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func telemetryDesc(name, help string, model bool) *prometheus.Desc {
	labels := []string{"node", "node_key"}
	if model {
		labels = append(labels, "model", "model_key")
	}
	return prometheus.NewDesc("vllm_dashboard_"+name, help, labels, nil)
}

var (
	nodeUpDesc       = telemetryDesc("node_up", "Whether local data or the peer telemetry transport is available; independent of sample freshness.", false)
	nodeSampleDesc   = telemetryDesc("node_sample_timestamp_seconds", "Unix timestamp of the latest host sample, including stale samples.", false)
	nodeInfoDesc     = prometheus.NewDesc("vllm_dashboard_node_info", "Static node metadata; always 1.", []string{"node", "node_key", "kernel_version"}, nil)
	modelUpDesc      = telemetryDesc("model_up", "Whether the latest model poll succeeded and its sample and last success are fresh.", true)
	modelSuccessDesc = telemetryDesc("model_last_success_timestamp_seconds", "Unix timestamp of the last successful model scrape, when known.", true)
	modelSampleDesc  = telemetryDesc("model_sample_timestamp_seconds", "Unix timestamp of the latest model sample, including failed polls.", true)
	promptTokensDesc = telemetryDesc("model_prompt_tokens_total", "Cumulative engine prompt tokens; resets on engine restart.", true)
	genTokensDesc    = telemetryDesc("model_generation_tokens_total", "Cumulative engine generated tokens; resets on engine restart.", true)
	completedDesc    = telemetryDesc("model_requests_completed_total", "Cumulative engine completed requests; resets on engine restart.", true)
	preemptionsDesc  = telemetryDesc("model_preemptions_total", "Cumulative engine preemptions; resets on engine restart.", true)
)

type sampleGauge struct {
	desc  *prometheus.Desc
	value func(Sample) float64
}

func modelGauge(name, help string, value func(Sample) float64) sampleGauge {
	return sampleGauge{telemetryDesc("model_"+name, help, true), value}
}

func hostGauge(name, help string, value func(Sample) float64) sampleGauge {
	return sampleGauge{telemetryDesc("node_"+name, help, false), value}
}

var modelGauges = []sampleGauge{
	modelGauge("requests_running", "Currently running requests.", func(s Sample) float64 { return s.Running }),
	modelGauge("requests_waiting", "Currently waiting requests.", func(s Sample) float64 { return s.Waiting }),
	modelGauge("kv_cache_usage_ratio", "KV cache utilization ratio.", func(s Sample) float64 { return s.KVCachePct / 100 }),
	modelGauge("prompt_tokens_per_second", "Prompt token rate over the last successful poll interval.", func(s Sample) float64 { return s.PromptTokPerSec }),
	modelGauge("generation_tokens_per_second", "Generated token rate over the last successful poll interval.", func(s Sample) float64 { return s.GenTokPerSec }),
	modelGauge("requests_per_second", "Completed request rate over the last successful poll interval.", func(s Sample) float64 { return s.ReqPerSec }),
	modelGauge("ttft_average_seconds", "Average time to first token over the last successful poll interval, not a histogram.", func(s Sample) float64 { return s.TTFTMs / 1000 }),
	modelGauge("itl_average_seconds", "Average inter-token latency over the last successful poll interval, not a histogram.", func(s Sample) float64 { return s.InterTokenMs / 1000 }),
	modelGauge("e2e_average_seconds", "Average end-to-end latency over the last successful poll interval, not a histogram.", func(s Sample) float64 { return s.E2ELatencyMs / 1000 }),
	modelGauge("prefix_cache_hit_ratio", "Cumulative prefix cache hit ratio.", func(s Sample) float64 { return s.PrefixCacheHitPct / 100 }),
	modelGauge("speculative_acceptance_ratio", "Speculative token acceptance ratio over the last successful poll interval.", func(s Sample) float64 { return s.SpecAcceptPct / 100 }),
}

var hostGauges = []sampleGauge{
	hostGauge("cpu_utilization_ratio", "Host CPU utilization ratio.", func(s Sample) float64 { return s.CPUUtilPct / 100 }),
	hostGauge("cpu_clock_hertz", "Average sampled CPU clock frequency in hertz.", func(s Sample) float64 { return s.CPUClockMHz * 1e6 }),
	hostGauge("gpu_utilization_ratio", "Sampled GPU utilization ratio.", func(s Sample) float64 { return s.GPUUtilPct / 100 }),
	hostGauge("gpu_temperature_celsius", "Sampled GPU temperature in degrees Celsius.", func(s Sample) float64 { return s.GPUTempC }),
	hostGauge("gpu_power_watts", "Sampled GPU power in watts.", func(s Sample) float64 { return s.GPUPowerW }),
	hostGauge("gpu_clock_hertz", "Sampled GPU clock frequency in hertz.", func(s Sample) float64 { return s.GPUClockMHz * 1e6 }),
	hostGauge("memory_used_ratio", "Host memory utilization ratio.", func(s Sample) float64 { return s.MemUsedPct / 100 }),
	hostGauge("memory_used_bytes", "Host memory used in bytes (cached memory GB fields are GiB).", func(s Sample) float64 { return s.MemUsedGB * (1 << 30) }),
	hostGauge("memory_total_bytes", "Host memory total in bytes (cached memory GB fields are GiB).", func(s Sample) float64 { return s.MemTotalGB * (1 << 30) }),
	hostGauge("network_receive_bytes_per_second", "Non-loopback network receive rate in bytes per second.", func(s Sample) float64 { return s.NetRxMbps * 1e6 / 8 }),
	hostGauge("network_transmit_bytes_per_second", "Non-loopback network transmit rate in bytes per second.", func(s Sample) float64 { return s.NetTxMbps * 1e6 / 8 }),
}

type telemetryCollector struct {
	nodes []telemetryNode
	now   time.Time
}

func (c *telemetryCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{nodeUpDesc, nodeSampleDesc, nodeInfoDesc, modelUpDesc, modelSuccessDesc, modelSampleDesc, promptTokensDesc, genTokensDesc, completedDesc, preemptionsDesc} {
		ch <- desc
	}
	for _, gauge := range modelGauges {
		ch <- gauge.desc
	}
	for _, gauge := range hostGauges {
		ch <- gauge.desc
	}
}

func emitTelemetry(ch chan<- prometheus.Metric, desc *prometheus.Desc, kind prometheus.ValueType, value float64, labels ...string) {
	if math.IsNaN(value) || math.IsInf(value, 0) || (kind == prometheus.CounterValue && value < 0) {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, kind, value, labels...)
}

func (c *telemetryCollector) Collect(ch chan<- prometheus.Metric) {
	for _, node := range c.nodes {
		name := node.name
		if name == "" {
			name = node.key
		}
		nodeLabels := []string{name, node.key}
		up := 0.0
		if node.up {
			up = 1
		}
		emitTelemetry(ch, nodeUpDesc, prometheus.GaugeValue, up, nodeLabels...)
		if !node.up {
			continue
		}
		if node.snapshot.KernelVersion != "" {
			emitTelemetry(ch, nodeInfoDesc, prometheus.GaugeValue, 1, name, node.key, node.snapshot.KernelVersion)
		}
		host := node.snapshot.Host
		if host.Time > 0 {
			emitTelemetry(ch, nodeSampleDesc, prometheus.GaugeValue, float64(host.Time)/1000, nodeLabels...)
		}
		if freshTelemetryTimestamp(host.Time, c.now) {
			for _, gauge := range hostGauges {
				emitTelemetry(ch, gauge.desc, prometheus.GaugeValue, gauge.value(host), nodeLabels...)
			}
		}
		for _, model := range node.snapshot.Models {
			name := model.Name
			if name == "" {
				name = model.Key
			}
			labels := []string{nodeLabels[0], node.key, name, model.Key}
			up := model.LastPollSuccess && freshTelemetryTimestamp(model.LastScrape, c.now) && freshTelemetryTimestamp(model.Latest.Time, c.now)
			value := 0.0
			if up {
				value = 1
			}
			emitTelemetry(ch, modelUpDesc, prometheus.GaugeValue, value, labels...)
			if model.LastScrape > 0 {
				emitTelemetry(ch, modelSuccessDesc, prometheus.GaugeValue, float64(model.LastScrape)/1000, labels...)
			}
			if model.Latest.Time > 0 {
				emitTelemetry(ch, modelSampleDesc, prometheus.GaugeValue, float64(model.Latest.Time)/1000, labels...)
			}
			if !up {
				continue
			}
			for _, gauge := range modelGauges {
				emitTelemetry(ch, gauge.desc, prometheus.GaugeValue, gauge.value(model.Latest), labels...)
			}
			emitTelemetry(ch, promptTokensDesc, prometheus.CounterValue, model.Counters.PromptTokens, labels...)
			emitTelemetry(ch, genTokensDesc, prometheus.CounterValue, model.Counters.GenerationTokens, labels...)
			emitTelemetry(ch, completedDesc, prometheus.CounterValue, model.Counters.CompletedRequests, labels...)
			emitTelemetry(ch, preemptionsDesc, prometheus.CounterValue, model.Counters.Preemptions, labels...)
		}
	}
}
