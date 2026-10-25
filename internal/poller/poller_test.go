package poller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/gpu"
	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func TestRecordScrapeComputesRatesAndKeepsCountersOnError(t *testing.T) {
	model := config.Model{Key: "model-a", Name: "Model A"}
	st := store.New(config.Config{})
	state := &store.ModelState{}
	record := func(identity sample.ServerInfo, metrics map[string]float64, err error) {
		s, at := recordScrape(state, model, identity, sample.Sample{}, metrics, err, time.Now())
		st.Append(state, s, at)
	}
	first := map[string]float64{
		"vllm:generation_tokens_total":           100,
		"vllm:time_to_first_token_seconds_sum":   1,
		"vllm:time_to_first_token_seconds_count": 2,
	}
	record(sample.ServerInfo{}, first, nil)
	state.LastScrape = state.LastScrape.Add(-time.Second)

	second := map[string]float64{
		"vllm:generation_tokens_total":           300,
		"vllm:time_to_first_token_seconds_sum":   2,
		"vllm:time_to_first_token_seconds_count": 4,
	}
	record(sample.ServerInfo{ServedModelName: "served"}, second, nil)
	latest := state.History[len(state.History)-1]
	if latest.GenTokPerSec < 150 || latest.GenTokPerSec > 200 || latest.TTFTMs != 500 || state.ModelName != "served" {
		t.Fatalf("unexpected sample: gen=%v ttft=%v name=%q", latest.GenTokPerSec, latest.TTFTMs, state.ModelName)
	}

	record(sample.ServerInfo{}, nil, errors.New("down"))
	failed := state.History[len(state.History)-1]
	if state.LastPollSuccess || failed.GenTokensCum != 300 || len(state.History) != 3 || len(state.LongHistory) != 1 {
		t.Fatalf("unexpected state after error: %+v", state)
	}
}

func TestSlowGPUScrapeDoesNotBlockPoll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "vllm:num_requests_running 1\n")
	}))
	defer server.Close()
	st := store.New(config.Config{Node: config.Node{Key: "n", Models: []config.Model{{Key: "m", MetricsURL: server.URL + "/metrics"}}}})
	release := make(chan struct{})
	gpus := gpu.NewSamplerFunc(time.Second, func() (float64, float64, float64, float64) {
		<-release
		return 1, 1, 1, 1
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		gpus.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		<-done
	})

	start := time.Now()
	New(st, gpus, process.NewCache("", ""), nil).Poll()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("poll took %v while the GPU tool was blocked", elapsed)
	}
	if samples := len(st.Samples("m", false)); samples != 1 {
		t.Fatalf("history has %d samples, want 1", samples)
	}
}

func TestDiscoverAddsModelsAndKeepsKnownKeys(t *testing.T) {
	known := config.Model{Port: "8000"}
	if err := config.CompleteModel(&known); err != nil {
		t.Fatal(err)
	}
	st := store.New(config.Config{Node: config.Node{Key: "node", Models: []config.Model{known}}})
	found := []config.Model{
		{Key: "qwen", Name: "Qwen", MetricsURL: "http://127.0.0.1:8000/metrics", Port: "8000", Unit: "vllm-qwen.service"},
		{Key: "coder", Name: "Coder", MetricsURL: "http://127.0.0.1:8001/metrics", Port: "8001"},
	}
	p := New(st, nil, process.NewCache("", ""), func() []config.Model { return found })
	p.Discover()
	p.Discover()
	models := st.Models()
	if len(models) != 2 || models[0].Key != "port-8000" || models[0].Unit != "vllm-qwen.service" || models[1].Key != "coder" {
		t.Fatalf("models = %+v", models)
	}
	if _, ok := st.LocalModelKey("node/coder"); !ok {
		t.Fatal("discovered model has no state")
	}
}
