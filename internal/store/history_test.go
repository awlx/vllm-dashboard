package store

import (
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
)

func TestAggregateSampleSetsCombinesNodes(t *testing.T) {
	got := AggregateSampleSets([][]sample.Sample{
		{{Time: 100, Running: 1, GPUUtilPct: 40, GPUPowerW: 20, GPUClockMHz: 1800, CPUClockMHz: 2800}},
		{{Time: 100, Running: 2, GPUUtilPct: 70, GPUPowerW: 30, GPUClockMHz: 2400, CPUClockMHz: 3900}},
	})
	if len(got) != 1 || got[0].Running != 3 || got[0].GPUUtilPct != 70 || got[0].GPUPowerW != 50 || got[0].GPUClockMHz != 2400 || got[0].CPUClockMHz != 3900 {
		t.Fatalf("unexpected aggregate: %+v", got)
	}
}

func TestTokensInWindowCountsResetAfterNewCounterSurpassesAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	samples := []sample.Sample{
		{Time: now.Add(-2 * time.Hour).UnixMilli(), PromptTokensCum: 32_000_000, GenTokensCum: 100_000},
		{Time: now.Add(-30 * time.Minute).UnixMilli(), PromptTokensCum: 500_000, GenTokensCum: 2_000},
		{Time: now.UnixMilli(), PromptTokensCum: 40_000_000, GenTokensCum: 120_000},
	}

	prompt, generated := tokensInWindow(samples, now, 24*time.Hour)
	if prompt != 40_000_000 {
		t.Fatalf("prompt tokens = %.0f, want 40000000", prompt)
	}
	if generated != 120_000 {
		t.Fatalf("generated tokens = %.0f, want 120000", generated)
	}
}

func TestTokensInWindowUsesTrailingWindowAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	samples := []sample.Sample{
		{Time: now.Add(-2 * time.Hour).UnixMilli(), PromptTokensCum: 100},
		{Time: now.Add(-time.Hour).UnixMilli(), PromptTokensCum: 200},
		{Time: now.Add(-30 * time.Minute).UnixMilli(), PromptTokensCum: 260},
		{Time: now.UnixMilli(), PromptTokensCum: 300},
	}

	prompt, _ := tokensInWindow(samples, now, time.Hour)
	if prompt != 100 {
		t.Fatalf("prompt tokens = %.0f, want 100", prompt)
	}
}

func TestAddUsageSumsModelsIndependently(t *testing.T) {
	qwen := sample.TokenUsage{Last24h: sample.TokenWindow{PromptTokens: 40, GenTokens: 4, TotalTokens: 44}}
	gemma := sample.TokenUsage{Last24h: sample.TokenWindow{PromptTokens: 30, GenTokens: 3, TotalTokens: 33}}

	total := AddUsage(qwen, gemma)
	if total.Last24h.PromptTokens != 70 || total.Last24h.GenTokens != 7 || total.Last24h.TotalTokens != 77 {
		t.Fatalf("combined usage = %+v, want prompt=70 gen=7 total=77", total.Last24h)
	}
}

func TestUsageSeedsWindowWhenModelStartedInsideIt(t *testing.T) {
	now := time.Now()
	samples := []sample.Sample{{
		Time:            now.UnixMilli(),
		PromptTokensCum: 40_000_000,
		GenTokensCum:    500_000,
	}}

	usage := usageForStartedAt(samples, now.Add(-11*time.Hour))
	if usage.Last24h.TotalTokens != 40_500_000 {
		t.Fatalf("24h tokens = %.0f, want 40500000", usage.Last24h.TotalTokens)
	}
	if usage.LastHour.TotalTokens != 0 {
		t.Fatalf("1h tokens = %.0f, want 0", usage.LastHour.TotalTokens)
	}
}

func TestSetModelsAddsAndRemovesState(t *testing.T) {
	st := New(config.Config{Node: config.Node{Key: "node", Models: []config.Model{{Key: "a"}, {Key: "b"}}}})
	added := st.SetModels([]config.Model{{Key: "b"}, {Key: "c"}})
	if len(added) != 1 || added[0].Key != "c" {
		t.Fatalf("added = %+v", added)
	}
	if _, ok := st.LocalModelKey("a"); ok {
		t.Fatal("removed model a is still known")
	}
	if status := st.Status("a"); !status.LastScrape.IsZero() {
		t.Fatalf("status of removed model = %+v", status)
	}
	st.Update("a", func(*ModelState) { t.Fatal("update of removed model ran") })
	if _, ok := st.LocalModelKey("node/c"); !ok {
		t.Fatal("added model c is not known")
	}
}
