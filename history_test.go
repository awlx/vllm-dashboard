package main

import (
	"testing"
	"time"
)

func TestAggregateSampleSetsCombinesNodes(t *testing.T) {
	got := aggregateSampleSets([][]Sample{
		{{Time: 100, Running: 1, GPUUtilPct: 40, GPUPowerW: 20, GPUClockMHz: 1800, CPUClockMHz: 2800}},
		{{Time: 100, Running: 2, GPUUtilPct: 70, GPUPowerW: 30, GPUClockMHz: 2400, CPUClockMHz: 3900}},
	})
	if len(got) != 1 || got[0].Running != 3 || got[0].GPUUtilPct != 70 || got[0].GPUPowerW != 50 || got[0].GPUClockMHz != 2400 || got[0].CPUClockMHz != 3900 {
		t.Fatalf("unexpected aggregate: %+v", got)
	}
}

func TestTokensInWindowCountsResetAfterNewCounterSurpassesAnchor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	samples := []Sample{
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
	samples := []Sample{
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
	qwen := TokenUsage{Last24h: TokenWindow{PromptTokens: 40, GenTokens: 4, TotalTokens: 44}}
	gemma := TokenUsage{Last24h: TokenWindow{PromptTokens: 30, GenTokens: 3, TotalTokens: 33}}

	total := addUsage(qwen, gemma)
	if total.Last24h.PromptTokens != 70 || total.Last24h.GenTokens != 7 || total.Last24h.TotalTokens != 77 {
		t.Fatalf("combined usage = %+v, want prompt=70 gen=7 total=77", total.Last24h)
	}
}

func TestUsageSeedsWindowWhenModelStartedInsideIt(t *testing.T) {
	now := time.Now()
	samples := []Sample{{
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
