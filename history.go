package main

import "time"

// appendLongSample adds one 24h point per longInterval. The point is the
// average of all polls since the previous point, so the 24h charts do not
// depend on the poll interval. The schedule stays anchored to whole minutes
// after the first point, unless polling stops for two intervals or more.
func appendLongSample(state *ModelState, s Sample, now time.Time) {
	state.Rollup.add(s)
	if !state.LastLongSample.IsZero() && now.Sub(state.LastLongSample) < longInterval {
		return
	}
	state.LongHistory = append(state.LongHistory, state.Rollup.result(s))
	if len(state.LongHistory) > longSize {
		state.LongHistory = state.LongHistory[len(state.LongHistory)-longSize:]
	}
	state.Rollup = sampleRollup{}
	if state.LastLongSample.IsZero() || now.Sub(state.LastLongSample) >= 2*longInterval {
		state.LastLongSample = now
	} else {
		state.LastLongSample = state.LastLongSample.Add(longInterval)
	}
}

// sampleRollup averages samples. Gauges and rates use all samples. Latency
// and speculative-decoding values use only samples with activity, because a
// zero there means "no requests in this poll", not "zero latency".
// Counters and cumulative ratios use the latest sample.
type sampleRollup struct {
	count       int
	sum         Sample
	activeCount [5]int
}

func averagedFields(s *Sample) [16]*float64 {
	return [16]*float64{&s.Running, &s.Waiting, &s.KVCachePct, &s.PromptTokPerSec, &s.GenTokPerSec, &s.ReqPerSec,
		&s.GPUUtilPct, &s.GPUTempC, &s.GPUPowerW, &s.GPUClockMHz, &s.CPUUtilPct, &s.CPUClockMHz,
		&s.MemUsedPct, &s.MemUsedGB, &s.NetRxMbps, &s.NetTxMbps}
}

func activeFields(s *Sample) [5]*float64 {
	return [5]*float64{&s.TTFTMs, &s.InterTokenMs, &s.E2ELatencyMs, &s.SpecAcceptPct, &s.SpecMeanAccepted}
}

func (r *sampleRollup) add(s Sample) {
	r.count++
	sums := averagedFields(&r.sum)
	for index, value := range averagedFields(&s) {
		*sums[index] += *value
	}
	activeSums := activeFields(&r.sum)
	for index, value := range activeFields(&s) {
		if *value > 0 {
			*activeSums[index] += *value
			r.activeCount[index]++
		}
	}
}

func (r *sampleRollup) result(latest Sample) Sample {
	out := latest
	if r.count == 0 {
		return out
	}
	sums := averagedFields(&r.sum)
	for index, field := range averagedFields(&out) {
		*field = *sums[index] / float64(r.count)
	}
	activeSums := activeFields(&r.sum)
	for index, field := range activeFields(&out) {
		*field = 0
		if r.activeCount[index] > 0 {
			*field = *activeSums[index] / float64(r.activeCount[index])
		}
	}
	return out
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
