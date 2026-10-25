package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func fillHistory(state *store.ModelState, short, long int) {
	now := time.Now()
	for i := range short {
		state.History = append(state.History, sample.Sample{Time: now.Add(time.Duration(i-short) * time.Second).UnixMilli(), Running: 3, KVCachePct: 41.5, GenTokPerSec: 812.25, PromptTokPerSec: 1534.5, GPUUtilPct: 97, CPUUtilPct: 12.3, MemUsedPct: 66.6, PromptTokensCum: float64(i) * 1000, GenTokensCum: float64(i) * 500})
	}
	for i := range long {
		state.LongHistory = append(state.LongHistory, sample.Sample{Time: now.Add(time.Duration(i-long) * time.Minute).UnixMilli(), Running: 3, KVCachePct: 41.5, GenTokPerSec: 812.25, PromptTokensCum: float64(i) * 1000, GenTokensCum: float64(i) * 500})
	}
}

func benchmarkRoute(b *testing.B, path, encoding string) {
	cfg := config.Config{Node: config.Node{Key: "node-a", Name: "Node A", Models: []config.Model{{Key: "model-a", Name: "A", MetricsURL: "http://127.0.0.1:1/metrics"}, {Key: "model-b", Name: "B", MetricsURL: "http://127.0.0.1:1/metrics"}}}}
	st := store.New(cfg)
	for _, model := range st.Models() {
		st.Update(model.Key, func(state *store.ModelState) { fillHistory(state, st.HistorySize(), store.LongSize) })
	}
	mux := http.NewServeMux()
	Register(mux, fleet.New(cfg, st, process.NewCache("", "")))
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
