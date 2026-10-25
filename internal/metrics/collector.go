package metrics

import (
	"math"
	"time"

	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/prometheus/client_golang/prometheus"
)

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
	value func(sample.Sample) float64
}

func modelGauge(name, help string, value func(sample.Sample) float64) sampleGauge {
	return sampleGauge{telemetryDesc("model_"+name, help, true), value}
}

func hostGauge(name, help string, value func(sample.Sample) float64) sampleGauge {
	return sampleGauge{telemetryDesc("node_"+name, help, false), value}
}

var modelGauges = []sampleGauge{
	modelGauge("requests_running", "Currently running requests.", func(s sample.Sample) float64 { return s.Running }),
	modelGauge("requests_waiting", "Currently waiting requests.", func(s sample.Sample) float64 { return s.Waiting }),
	modelGauge("kv_cache_usage_ratio", "KV cache utilization ratio.", func(s sample.Sample) float64 { return s.KVCachePct / 100 }),
	modelGauge("prompt_tokens_per_second", "Prompt token rate over the last successful poll interval.", func(s sample.Sample) float64 { return s.PromptTokPerSec }),
	modelGauge("generation_tokens_per_second", "Generated token rate over the last successful poll interval.", func(s sample.Sample) float64 { return s.GenTokPerSec }),
	modelGauge("requests_per_second", "Completed request rate over the last successful poll interval.", func(s sample.Sample) float64 { return s.ReqPerSec }),
	modelGauge("ttft_average_seconds", "Average time to first token over the last successful poll interval, not a histogram.", func(s sample.Sample) float64 { return s.TTFTMs / 1000 }),
	modelGauge("itl_average_seconds", "Average inter-token latency over the last successful poll interval, not a histogram.", func(s sample.Sample) float64 { return s.InterTokenMs / 1000 }),
	modelGauge("e2e_average_seconds", "Average end-to-end latency over the last successful poll interval, not a histogram.", func(s sample.Sample) float64 { return s.E2ELatencyMs / 1000 }),
	modelGauge("prefix_cache_hit_ratio", "Cumulative prefix cache hit ratio.", func(s sample.Sample) float64 { return s.PrefixCacheHitPct / 100 }),
	modelGauge("speculative_acceptance_ratio", "Speculative token acceptance ratio over the last successful poll interval.", func(s sample.Sample) float64 { return s.SpecAcceptPct / 100 }),
}

var hostGauges = []sampleGauge{
	hostGauge("cpu_utilization_ratio", "Host CPU utilization ratio.", func(s sample.Sample) float64 { return s.CPUUtilPct / 100 }),
	hostGauge("cpu_clock_hertz", "Average sampled CPU clock frequency in hertz.", func(s sample.Sample) float64 { return s.CPUClockMHz * 1e6 }),
	hostGauge("gpu_utilization_ratio", "Sampled GPU utilization ratio.", func(s sample.Sample) float64 { return s.GPUUtilPct / 100 }),
	hostGauge("gpu_temperature_celsius", "Sampled GPU temperature in degrees Celsius.", func(s sample.Sample) float64 { return s.GPUTempC }),
	hostGauge("gpu_power_watts", "Sampled GPU power in watts.", func(s sample.Sample) float64 { return s.GPUPowerW }),
	hostGauge("gpu_clock_hertz", "Sampled GPU clock frequency in hertz.", func(s sample.Sample) float64 { return s.GPUClockMHz * 1e6 }),
	hostGauge("memory_used_ratio", "Host memory utilization ratio.", func(s sample.Sample) float64 { return s.MemUsedPct / 100 }),
	hostGauge("memory_used_bytes", "Host memory used in bytes (cached memory GB fields are GiB).", func(s sample.Sample) float64 { return s.MemUsedGB * (1 << 30) }),
	hostGauge("memory_total_bytes", "Host memory total in bytes (cached memory GB fields are GiB).", func(s sample.Sample) float64 { return s.MemTotalGB * (1 << 30) }),
	hostGauge("network_receive_bytes_per_second", "Non-loopback network receive rate in bytes per second.", func(s sample.Sample) float64 { return s.NetRxMbps * 1e6 / 8 }),
	hostGauge("network_transmit_bytes_per_second", "Non-loopback network transmit rate in bytes per second.", func(s sample.Sample) float64 { return s.NetTxMbps * 1e6 / 8 }),
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
