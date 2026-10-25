package store

import (
	"time"

	"github.com/awlx/vllm-dashboard/internal/sample"
)

// tokensInWindow returns cumulative prompt/gen token deltas over the trailing
// window, using the closest sample at or before (now - window) as the anchor.
// It sums adjacent deltas so counter resets inside the window remain visible
// even after the new process counters surpass the pre-reset values.
func tokensInWindow(samples []sample.Sample, now time.Time, window time.Duration) (promptTokens, genTokens float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	cutoffMs := now.Add(-window).UnixMilli()
	anchorIndex := 0
	for index, point := range samples {
		if point.Time > cutoffMs {
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

// AggregateSampleSets merges sample sets by tail index. It sums counts and
// rates and keeps the maximum of percentages and latencies.
func AggregateSampleSets(sources [][]sample.Sample) []sample.Sample {
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
	result := make([]sample.Sample, length)
	for index := range length {
		base := sources[0][len(sources[0])-length+index]
		result[index] = base
		for _, source := range sources[1:] {
			point := source[len(source)-length+index]
			result[index].Running += point.Running
			result[index].Waiting += point.Waiting
			result[index].PromptTokPerSec += point.PromptTokPerSec
			result[index].GenTokPerSec += point.GenTokPerSec
			result[index].ReqPerSec += point.ReqPerSec
			result[index].CompletedRequests += point.CompletedRequests
			result[index].PreemptionsTotal += point.PreemptionsTotal
			result[index].PromptTokensCum += point.PromptTokensCum
			result[index].GenTokensCum += point.GenTokensCum
			result[index].GPUUtilPct = max(result[index].GPUUtilPct, point.GPUUtilPct)
			result[index].GPUTempC = max(result[index].GPUTempC, point.GPUTempC)
			result[index].GPUPowerW += point.GPUPowerW
			result[index].GPUClockMHz = max(result[index].GPUClockMHz, point.GPUClockMHz)
			result[index].CPUUtilPct = max(result[index].CPUUtilPct, point.CPUUtilPct)
			result[index].CPUClockMHz = max(result[index].CPUClockMHz, point.CPUClockMHz)
			result[index].MemUsedPct = max(result[index].MemUsedPct, point.MemUsedPct)
			result[index].NetRxMbps += point.NetRxMbps
			result[index].NetTxMbps += point.NetTxMbps
			result[index].KVCachePct = max(result[index].KVCachePct, point.KVCachePct)
			result[index].TTFTMs = max(result[index].TTFTMs, point.TTFTMs)
			result[index].InterTokenMs = max(result[index].InterTokenMs, point.InterTokenMs)
			result[index].E2ELatencyMs = max(result[index].E2ELatencyMs, point.E2ELatencyMs)
		}
	}
	return result
}

func (s *Store) aggregateLocalSamples(long bool) []sample.Sample {
	var sources [][]sample.Sample
	for _, model := range s.models {
		state := s.states[model.Key]
		if long {
			sources = append(sources, state.LongHistory)
		} else {
			sources = append(sources, state.History)
		}
	}
	return AggregateSampleSets(sources)
}

func (s *Store) samplesFor(key string, long bool) []sample.Sample {
	if key == "all" {
		return s.aggregateLocalSamples(long)
	}
	localKey, ok := s.localModelKey(key)
	if !ok {
		return nil
	}
	state := s.states[localKey]
	if long {
		return state.LongHistory
	}
	return state.History
}

func usageFor(samples []sample.Sample) sample.TokenUsage {
	return usageForStartedAt(samples, time.Time{})
}

func usageForStartedAt(samples []sample.Sample, startedAt time.Time) sample.TokenUsage {
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
	return sample.TokenUsage{
		LastHour: sample.TokenWindow{PromptTokens: p1, GenTokens: g1, TotalTokens: p1 + g1},
		Last24h:  sample.TokenWindow{PromptTokens: p24, GenTokens: g24, TotalTokens: p24 + g24},
	}
}

// AddUsage returns the sum of two token usages.
func AddUsage(total, usage sample.TokenUsage) sample.TokenUsage {
	total.LastHour.PromptTokens += usage.LastHour.PromptTokens
	total.LastHour.GenTokens += usage.LastHour.GenTokens
	total.LastHour.TotalTokens += usage.LastHour.TotalTokens
	total.Last24h.PromptTokens += usage.Last24h.PromptTokens
	total.Last24h.GenTokens += usage.Last24h.GenTokens
	total.Last24h.TotalTokens += usage.Last24h.TotalTokens
	return total
}

func usageForState(state *ModelState) sample.TokenUsage {
	return usageForStartedAt(state.LongHistory, state.CounterStartedAt)
}

func (s *Store) usageForKey(key string) sample.TokenUsage {
	if key != "all" {
		state := s.states[s.modelConfig(key).Key]
		if state == nil {
			return sample.TokenUsage{}
		}
		return usageForState(state)
	}
	var total sample.TokenUsage
	for _, model := range s.models {
		total = AddUsage(total, usageForState(s.states[model.Key]))
	}
	return total
}
