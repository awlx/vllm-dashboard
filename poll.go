package main

import (
	"log"
	"sync"
	"time"
)

func poll() {
	cpuTotal, cpuIdle := scrapeCPUTimes()
	cpuClock := scrapeCPUClockMHz()
	memUsedGB, memTotalGB, memUsedPct := scrapeMem()
	netRx, netTx := scrapeNet()
	now := time.Now()
	gpuUtil, gpuTemp, gpuPower, gpuClock := latestGPU(now)

	shared := Sample{
		Time:       now.UnixMilli(),
		GPUUtilPct: gpuUtil, GPUTempC: gpuTemp, GPUPowerW: gpuPower, GPUClockMHz: gpuClock,
		CPUClockMHz: cpuClock,
		MemUsedGB:   memUsedGB, MemTotalGB: memTotalGB, MemUsedPct: memUsedPct,
	}

	if !lastSystemScrape.IsZero() {
		dt := now.Sub(lastSystemScrape).Seconds()
		if dt > 0 {
			dTotal := cpuTotal - lastCPUTotal
			dIdle := cpuIdle - lastCPUIdle
			if dTotal > 0 {
				shared.CPUUtilPct = (1 - dIdle/dTotal) * 100
			}
			shared.NetRxMbps = (netRx - lastNetRx) * 8 / 1e6 / dt
			shared.NetTxMbps = (netTx - lastNetTx) * 8 / 1e6 / dt
		}
	}
	lastCPUTotal, lastCPUIdle = cpuTotal, cpuIdle
	lastNetRx, lastNetTx = netRx, netTx
	lastSystemScrape = now

	mu.Lock()
	latestHostSample = shared
	mu.Unlock()

	// Scrape models in parallel, so that one slow or offline model does not
	// delay the samples of the other models.
	var wg sync.WaitGroup
	for _, model := range models {
		wg.Go(func() { pollModel(model, shared, now) })
	}
	wg.Wait()
}

func pollModel(model ModelConfig, shared Sample, now time.Time) {
	metrics, err := scrapeVLLM(model.MetricsURL)
	mu.RLock()
	recovered := err == nil && !modelStates[model.Key].LastPollSuccess
	mu.RUnlock()
	if recovered {
		// The engine came back, maybe with a new process or model.
		identityCache.invalidate(model.Key)
		serverInfoCache.invalidate(model.Key)
	}
	identity, _ := cachedModelIdentity(model)
	mu.Lock()
	defer mu.Unlock()
	state := modelStates[model.Key]
	state.ModelName = modelDisplayName(identity, model.Name)
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

	state.History = append(state.History, s)
	if limit := historySize(); len(state.History) > limit {
		state.History = state.History[len(state.History)-limit:]
	}
	appendLongSample(state, s, now)
}

// runPollLoop polls on a fixed cadence. A ticker never runs polls in
// parallel: if one poll is slow, the next poll starts when it ends.
func runPollLoop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		poll()
		<-ticker.C
	}
}
