package store

import (
	"time"

	"github.com/awlx/vllm-dashboard/internal/sample"
)

// AppendLongSample adds one 24h point per LongInterval. The point is the
// average of all polls since the previous point, so the 24h charts do not
// depend on the poll interval. The schedule stays anchored to whole minutes
// after the first point, unless polling stops for two intervals or more.
func AppendLongSample(state *ModelState, s sample.Sample, now time.Time) {
	state.Rollup.add(s)
	if !state.LastLongSample.IsZero() && now.Sub(state.LastLongSample) < LongInterval {
		return
	}
	state.LongHistory = append(state.LongHistory, state.Rollup.result(s))
	if len(state.LongHistory) > LongSize {
		state.LongHistory = state.LongHistory[len(state.LongHistory)-LongSize:]
	}
	state.Rollup = Rollup{}
	if state.LastLongSample.IsZero() || now.Sub(state.LastLongSample) >= 2*LongInterval {
		state.LastLongSample = now
	} else {
		state.LastLongSample = state.LastLongSample.Add(LongInterval)
	}
}

// Rollup averages samples. Gauges and rates use all samples. Latency
// and speculative-decoding values use only samples with activity, because a
// zero there means "no requests in this poll", not "zero latency".
// Counters and cumulative ratios use the latest sample.
type Rollup struct {
	count       int
	sum         sample.Sample
	activeCount [5]int
}

func averagedFields(s *sample.Sample) [16]*float64 {
	return [16]*float64{&s.Running, &s.Waiting, &s.KVCachePct, &s.PromptTokPerSec, &s.GenTokPerSec, &s.ReqPerSec,
		&s.GPUUtilPct, &s.GPUTempC, &s.GPUPowerW, &s.GPUClockMHz, &s.CPUUtilPct, &s.CPUClockMHz,
		&s.MemUsedPct, &s.MemUsedGB, &s.NetRxMbps, &s.NetTxMbps}
}

func activeFields(s *sample.Sample) [5]*float64 {
	return [5]*float64{&s.TTFTMs, &s.InterTokenMs, &s.E2ELatencyMs, &s.SpecAcceptPct, &s.SpecMeanAccepted}
}

func (r *Rollup) add(s sample.Sample) {
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

func (r *Rollup) result(latest sample.Sample) sample.Sample {
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
