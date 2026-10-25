// Package poller reads the host and the local models at a fixed interval
// and writes the results to the store.
package poller

import (
	"log"
	"sync"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/gpu"
	"github.com/awlx/vllm-dashboard/internal/collect/host"
	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/collect/vllm"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

// Poller keeps the counters that it needs to compute host rates.
type Poller struct {
	store                     *store.Store
	gpus                      *gpu.Sampler
	procs                     *process.Cache
	lastCPUTotal, lastCPUIdle float64
	lastNetRx, lastNetTx      float64
	lastSystemScrape          time.Time
}

// New returns a poller that writes to st. It reads GPU values from the
// gpus sampler and process data from the procs cache.
func New(st *store.Store, gpus *gpu.Sampler, procs *process.Cache) *Poller {
	return &Poller{store: st, gpus: gpus, procs: procs}
}

// Run polls on a fixed cadence. A ticker never runs polls in parallel:
// if one poll is slow, the next poll starts when it ends.
func (p *Poller) Run() {
	ticker := time.NewTicker(p.store.Interval())
	defer ticker.Stop()
	for {
		p.Poll()
		<-ticker.C
	}
}

// Poll reads the host once and then all local models in parallel.
func (p *Poller) Poll() {
	cpuTotal, cpuIdle := host.CPUTimes()
	cpuClock := host.CPUClockMHz()
	memUsedGB, memTotalGB, memUsedPct := host.Memory()
	netRx, netTx := host.Network()
	now := time.Now()
	gpuUtil, gpuTemp, gpuPower, gpuClock := p.gpus.Latest(now)

	shared := sample.Sample{
		Time:       now.UnixMilli(),
		GPUUtilPct: gpuUtil, GPUTempC: gpuTemp, GPUPowerW: gpuPower, GPUClockMHz: gpuClock,
		CPUClockMHz: cpuClock,
		MemUsedGB:   memUsedGB, MemTotalGB: memTotalGB, MemUsedPct: memUsedPct,
	}

	if !p.lastSystemScrape.IsZero() {
		dt := now.Sub(p.lastSystemScrape).Seconds()
		if dt > 0 {
			dTotal := cpuTotal - p.lastCPUTotal
			dIdle := cpuIdle - p.lastCPUIdle
			if dTotal > 0 {
				shared.CPUUtilPct = (1 - dIdle/dTotal) * 100
			}
			shared.NetRxMbps = (netRx - p.lastNetRx) * 8 / 1e6 / dt
			shared.NetTxMbps = (netTx - p.lastNetTx) * 8 / 1e6 / dt
		}
	}
	p.lastCPUTotal, p.lastCPUIdle = cpuTotal, cpuIdle
	p.lastNetRx, p.lastNetTx = netRx, netTx
	p.lastSystemScrape = now

	p.store.SetHost(shared)

	// Scrape models in parallel, so that one slow or offline model does not
	// delay the samples of the other models.
	var wg sync.WaitGroup
	for _, model := range p.store.Models() {
		wg.Go(func() { p.pollModel(model, shared, now) })
	}
	wg.Wait()
}

func (p *Poller) pollModel(model config.Model, shared sample.Sample, now time.Time) {
	metrics, err := vllm.Scrape(model.MetricsURL)
	if err == nil && !p.store.LastPollSuccess(model.Key) {
		// The engine came back, maybe with a new process or model.
		p.procs.Invalidate(model.Key)
	}
	identity, _ := p.procs.Identity(model)
	p.store.Update(model.Key, func(state *store.ModelState) {
		s, at := recordScrape(state, model, identity, shared, metrics, err, now)
		p.store.Append(state, s, at)
	})
}

// recordScrape updates the counters of state and returns the new sample
// and its time. The caller appends the sample to the history.
func recordScrape(state *store.ModelState, model config.Model, identity sample.ServerInfo, shared sample.Sample, metrics map[string]float64, err error, now time.Time) (sample.Sample, time.Time) {
	state.ModelName = process.DisplayName(identity, model.Name)
	s := shared
	// Model timestamps describe completion of this scrape, not the beginning
	// of a potentially long sequential poll of all configured models.
	now = time.Now()
	s.Time = now.UnixMilli()
	state.LastPollSuccess = err == nil

	if err != nil {
		log.Printf("%s scrape error: %v", model.Key, err)
		s.PromptTokensCum = state.LastPromptTokens
		s.GenTokensCum = state.LastGenTokens
	} else {
		s.Running = metrics["vllm:num_requests_running"]
		s.Waiting = metrics["vllm:num_requests_waiting"]
		s.KVCachePct = metrics["vllm:kv_cache_usage_perc"] * 100
		s.PreemptionsTotal = metrics["vllm:num_preemptions_total"]
		s.CompletedRequests = metrics["vllm:request_success_total"]

		queries := metrics["vllm:prefix_cache_queries_total"]
		hits := metrics["vllm:prefix_cache_hits_total"]
		if queries > 0 {
			s.PrefixCacheHitPct = hits / queries * 100
		}

		promptTok := metrics["vllm:prompt_tokens_total"]
		genTok := metrics["vllm:generation_tokens_total"]
		s.PromptTokensCum = promptTok
		s.GenTokensCum = genTok
		ttftSum := metrics["vllm:time_to_first_token_seconds_sum"]
		ttftCount := metrics["vllm:time_to_first_token_seconds_count"]
		itlSum := metrics["vllm:inter_token_latency_seconds_sum"]
		itlCount := metrics["vllm:inter_token_latency_seconds_count"]
		e2eSum := metrics["vllm:e2e_request_latency_seconds_sum"]
		e2eCount := metrics["vllm:e2e_request_latency_seconds_count"]
		specAccepted := metrics["vllm:spec_decode_num_accepted_tokens_total"]
		specDraft := metrics["vllm:spec_decode_num_draft_tokens_total"]

		if !state.LastScrape.IsZero() {
			dt := now.Sub(state.LastScrape).Seconds()
			if dt > 0 {
				pps := (promptTok - state.LastPromptTokens) / dt
				gps := (genTok - state.LastGenTokens) / dt
				if pps > 0 {
					s.PromptTokPerSec = pps
				}
				if gps > 0 {
					s.GenTokPerSec = gps
				}
				dReq := s.CompletedRequests - state.LastCompletedRequests
				if dReq > 0 {
					s.ReqPerSec = dReq / dt
				}
			}
			dCount := ttftCount - state.LastTTFTCount
			if dCount > 0 {
				s.TTFTMs = (ttftSum - state.LastTTFTSum) / dCount * 1000
			}
			dITLCount := itlCount - state.LastITLCount
			if dITLCount > 0 {
				s.InterTokenMs = (itlSum - state.LastITLSum) / dITLCount * 1000
			}
			dE2ECount := e2eCount - state.LastE2ECount
			if dE2ECount > 0 {
				s.E2ELatencyMs = (e2eSum - state.LastE2ESum) / dE2ECount * 1000
			}
			dSpecDraft := specDraft - state.LastSpecDraft
			dSpecAccepted := specAccepted - state.LastSpecAccepted
			if dSpecDraft > 0 {
				s.SpecAcceptPct = dSpecAccepted / dSpecDraft * 100
			}
			dReqTotal := s.CompletedRequests - state.LastCompletedRequests
			if dReqTotal > 0 {
				s.SpecMeanAccepted = dSpecAccepted / dReqTotal
			}
		}
		state.LastPromptTokens = promptTok
		state.LastGenTokens = genTok
		state.LastTTFTSum, state.LastTTFTCount = ttftSum, ttftCount
		state.LastITLSum, state.LastITLCount = itlSum, itlCount
		state.LastE2ESum, state.LastE2ECount = e2eSum, e2eCount
		state.LastSpecAccepted, state.LastSpecDraft = specAccepted, specDraft
		state.LastCompletedRequests = s.CompletedRequests
		state.LastScrape = now
	}
	return s, now
}
