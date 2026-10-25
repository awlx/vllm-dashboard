package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockVLLMMetrics returns a /metrics body close in size and shape to a real
// vLLM server: counters, gauges, and histograms with many buckets.
func mockVLLMMetrics(counter float64) string {
	var b strings.Builder
	labels := `{engine="0",model_name="mock-model"}`
	for _, name := range []string{"num_requests_running", "num_requests_waiting", "kv_cache_usage_perc"} {
		fmt.Fprintf(&b, "# HELP vllm:%s gauge\n# TYPE vllm:%s gauge\nvllm:%s%s %g\n", name, name, name, labels, 0.5)
	}
	for _, name := range []string{"prompt_tokens_total", "generation_tokens_total", "num_preemptions_total", "prefix_cache_queries_total", "prefix_cache_hits_total", "spec_decode_num_accepted_tokens_total", "spec_decode_num_draft_tokens_total"} {
		fmt.Fprintf(&b, "# TYPE vllm:%s counter\nvllm:%s%s %g\n", name, name, labels, counter)
	}
	for _, reason := range []string{"stop", "length", "abort"} {
		fmt.Fprintf(&b, "vllm:request_success_total{engine=\"0\",finished_reason=%q,model_name=\"mock-model\"} %g\n", reason, counter/10)
	}
	for _, name := range []string{"time_to_first_token_seconds", "inter_token_latency_seconds", "e2e_request_latency_seconds", "request_queue_time_seconds", "request_prefill_time_seconds", "request_decode_time_seconds", "request_prompt_tokens", "request_generation_tokens", "iteration_tokens_total"} {
		fmt.Fprintf(&b, "# TYPE vllm:%s histogram\n", name)
		for bucket := 0; bucket < 30; bucket++ {
			fmt.Fprintf(&b, "vllm:%s_bucket{engine=\"0\",le=\"%g\",model_name=\"mock-model\"} %g\n", name, float64(bucket)*0.25, counter)
		}
		fmt.Fprintf(&b, "vllm:%s_sum%s %g\nvllm:%s_count%s %g\n", name, labels, counter/3, name, labels, counter/7)
	}
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "python_gc_objects_collected_total{generation=\"%d\"} 1234\n", i)
	}
	return b.String()
}

func newMockVLLM(tb testing.TB) *httptest.Server {
	tb.Helper()
	var counter float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter += 100
		io.WriteString(w, mockVLLMMetrics(counter))
	}))
	tb.Cleanup(server.Close)
	return server
}

func BenchmarkScrapeVLLM(b *testing.B) {
	server := newMockVLLM(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := scrapeVLLM(server.URL + "/metrics"); err != nil {
			b.Fatal(err)
		}
	}
}

func fillHistory(state *ModelState, short, long int) {
	now := time.Now()
	for i := range short {
		state.History = append(state.History, Sample{Time: now.Add(time.Duration(i-short) * time.Second).UnixMilli(), Running: 3, KVCachePct: 41.5, GenTokPerSec: 812.25, PromptTokPerSec: 1534.5, GPUUtilPct: 97, CPUUtilPct: 12.3, MemUsedPct: 66.6, PromptTokensCum: float64(i) * 1000, GenTokensCum: float64(i) * 500})
	}
	for i := range long {
		state.LongHistory = append(state.LongHistory, Sample{Time: now.Add(time.Duration(i-long) * time.Minute).UnixMilli(), Running: 3, KVCachePct: 41.5, GenTokPerSec: 812.25, PromptTokensCum: float64(i) * 1000, GenTokensCum: float64(i) * 500})
	}
}

func benchmarkRoute(b *testing.B, path, encoding string) {
	configureRuntime(DashboardConfig{Node: NodeConfig{Key: "node-a", Name: "Node A", Models: []ModelConfig{{Key: "model-a", Name: "A", MetricsURL: "http://127.0.0.1:1/metrics"}, {Key: "model-b", Name: "B", MetricsURL: "http://127.0.0.1:1/metrics"}}}})
	for _, state := range modelStates {
		fillHistory(state, historySize(), longSize)
	}
	mux := newServeMux()
	b.ReportAllocs()
	var size int
	for b.Loop() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", path, nil)
		if encoding != "" {
			request.Header.Set("Accept-Encoding", encoding)
		}
		mux.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			b.Fatalf("status %d", recorder.Code)
		}
		size = recorder.Body.Len()
	}
	b.ReportMetric(float64(size), "bytes/resp")
}

func BenchmarkAPIMetricsShort(b *testing.B) { benchmarkRoute(b, "/api/metrics?model=all", "") }
func BenchmarkAPIMetricsShortGzip(b *testing.B) {
	benchmarkRoute(b, "/api/metrics?model=all", "gzip")
}
func BenchmarkAPIMetricsLong(b *testing.B) {
	benchmarkRoute(b, "/api/metrics/long?model=node-a/model-a", "")
}
func BenchmarkAPIMetricsLongGzip(b *testing.B) {
	benchmarkRoute(b, "/api/metrics/long?model=node-a/model-a", "gzip")
}
func BenchmarkAPITokens(b *testing.B) { benchmarkRoute(b, "/api/tokens?model=all", "") }
