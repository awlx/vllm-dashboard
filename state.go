package main

import (
	"net/http"
	"sync"
	"time"
)

type ModelState struct {
	ModelName             string
	History               []Sample
	LongHistory           []Sample
	LastLongSample        time.Time
	CounterStartedAt      time.Time
	LastPromptTokens      float64
	LastGenTokens         float64
	LastTTFTSum           float64
	LastTTFTCount         float64
	LastITLSum            float64
	LastITLCount          float64
	LastE2ESum            float64
	LastE2ECount          float64
	LastCompletedRequests float64
	LastSpecAccepted      float64
	LastSpecDraft         float64
	LastScrape            time.Time
	LastPollSuccess       bool
	Rollup                sampleRollup
}

const (
	defaultPollInterval = time.Second
	minPollInterval     = 500 * time.Millisecond
	// The 30s telemetry freshness window must cover several polls.
	maxPollInterval = 10 * time.Second
	historyWindow   = 5 * time.Minute
	longInterval    = 60 * time.Second // 24h at 1 min resolution
	longSize        = 24 * 60
	pollIntervalEnv = "VLLM_DASHBOARD_POLL_INTERVAL"
)

// pollInterval is set by configureRuntime before polling starts.
var pollInterval = defaultPollInterval

// historySize keeps historyWindow of live samples at the poll interval.
func historySize() int {
	return max(1, int((historyWindow+pollInterval-1)/pollInterval))
}

// onlineWindow tolerates two missed polls and slow sequential scrapes.
func onlineWindow() time.Duration {
	return max(3*pollInterval, 6*time.Second)
}

var (
	mu                        sync.RWMutex
	lastCPUTotal, lastCPUIdle float64
	lastNetRx, lastNetTx      float64
	lastSystemScrape          time.Time
	vllmVersion               string
	kernelVersion             string
	config                    DashboardConfig
	models                    []ModelConfig
	modelStates               map[string]*ModelState
	peerClient                = &http.Client{Timeout: 4 * time.Second}
)

func configureRuntime(cfg DashboardConfig) {
	mu.Lock()
	defer mu.Unlock()
	config = cfg
	models = cfg.Node.Models
	pollInterval = defaultPollInterval
	if interval, err := parsePollInterval(cfg.PollInterval); err == nil {
		pollInterval = interval
	}
	latestHostSample = Sample{}
	lastCPUTotal, lastCPUIdle = 0, 0
	lastNetRx, lastNetTx = 0, 0
	lastSystemScrape = time.Time{}
	modelStates = make(map[string]*ModelState, len(models))
	for _, model := range models {
		modelStates[model.Key] = &ModelState{}
	}
	identityCache.reset()
	serverInfoCache.reset()
}

func modelConfig(key string) ModelConfig {
	if localKey, ok := localModelKey(key); ok {
		key = localKey
	}
	for _, model := range models {
		if model.Key == key {
			return model
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ModelConfig{}
}
