// Package store keeps the local model state and the sample history.
package store

import (
	"strings"
	"sync"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
)

// ModelState is the poll state and the history of one local model.
type ModelState struct {
	ModelName             string
	History               []sample.Sample
	LongHistory           []sample.Sample
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
	Rollup                Rollup
}

const (
	HistoryWindow = 5 * time.Minute
	LongInterval  = 60 * time.Second // 24h at 1 min resolution
	LongSize      = 24 * 60
)

// Store holds the state of the local node. The node and the model list do
// not change after New. One lock protects all model states and the host sample.
type Store struct {
	node     config.Node
	models   []config.Model
	interval time.Duration

	// VLLMVersion and KernelVersion are set once, before polling starts.
	VLLMVersion   string
	KernelVersion string

	mu     sync.RWMutex
	states map[string]*ModelState
	host   sample.Sample
}

// New makes an empty state for each model of the local node.
func New(cfg config.Config) *Store {
	s := &Store{
		node:     cfg.Node,
		models:   cfg.Node.Models,
		interval: cfg.Interval(),
		states:   make(map[string]*ModelState, len(cfg.Node.Models)),
	}
	for _, model := range s.models {
		s.states[model.Key] = &ModelState{}
	}
	return s
}

// Interval returns the poll interval.
func (s *Store) Interval() time.Duration {
	return s.interval
}

// HistorySize keeps HistoryWindow of live samples at the poll interval.
func (s *Store) HistorySize() int {
	return max(1, int((HistoryWindow+s.interval-1)/s.interval))
}

// OnlineWindow tolerates two missed polls and slow scrapes.
func (s *Store) OnlineWindow() time.Duration {
	return max(3*s.interval, 6*time.Second)
}

// Append adds a sample to the live history and to the minute rollup.
// The caller must hold the write lock, for example inside Update.
func (s *Store) Append(state *ModelState, sm sample.Sample, now time.Time) {
	state.History = append(state.History, sm)
	if limit := s.HistorySize(); len(state.History) > limit {
		state.History = state.History[len(state.History)-limit:]
	}
	AppendLongSample(state, sm, now)
}

// Node returns the local node configuration.
func (s *Store) Node() config.Node {
	return s.node
}

// Models returns the local model configurations.
func (s *Store) Models() []config.Model {
	return s.models
}

// LocalModelKey accepts a bare or node-scoped key of a local model.
func (s *Store) LocalModelKey(key string) (string, bool) {
	prefix := s.node.Key + "/"
	key = strings.TrimPrefix(key, prefix)
	for _, model := range s.models {
		if model.Key == key {
			return key, true
		}
	}
	return "", false
}

// ModelConfig returns the local model for key, or the first model.
func (s *Store) ModelConfig(key string) config.Model {
	if localKey, ok := s.LocalModelKey(key); ok {
		key = localKey
	}
	for _, model := range s.models {
		if model.Key == key {
			return model
		}
	}
	if len(s.models) > 0 {
		return s.models[0]
	}
	return config.Model{}
}

// Update runs fn with the write lock held.
func (s *Store) Update(key string, fn func(*ModelState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.states[key])
}

// SetHost stores the latest host sample.
func (s *Store) SetHost(host sample.Sample) {
	s.mu.Lock()
	s.host = host
	s.mu.Unlock()
}

// Samples returns a copy of the history for a local key or "all".
func (s *Store) Samples(key string, long bool) []sample.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]sample.Sample(nil), s.samplesFor(key, long)...)
}

// AggregateLocal returns a copy of the sum of all local model histories.
func (s *Store) AggregateLocal(long bool) []sample.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]sample.Sample(nil), s.aggregateLocalSamples(long)...)
}

// UsageForKey returns the token usage of a local key or "all".
func (s *Store) UsageForKey(key string) sample.TokenUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usageForKey(key)
}

// LastPollSuccess reports whether the last scrape of a local model worked.
func (s *Store) LastPollSuccess(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.states[key].LastPollSuccess
}

// ModelStatus is a copy of the values that the overview needs.
type ModelStatus struct {
	Latest     sample.Sample
	LastScrape time.Time
	Tokens     sample.TokenUsage
}

// Status returns the overview values of a local model.
func (s *Store) Status(key string) ModelStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.states[key]
	status := ModelStatus{LastScrape: state.LastScrape, Tokens: usageForState(state)}
	if len(state.History) > 0 {
		status.Latest = state.History[len(state.History)-1]
	}
	return status
}

// ModelSnapshot is a copy of the telemetry values of one local model.
type ModelSnapshot struct {
	Config          config.Model
	ModelName       string
	LastScrape      time.Time
	LastPollSuccess bool
	Latest          sample.Sample
}

// Snapshot returns the host sample and all local model values in one read.
func (s *Store) Snapshot() (sample.Sample, []ModelSnapshot) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := make([]ModelSnapshot, 0, len(s.models))
	for _, model := range s.models {
		item := ModelSnapshot{Config: model}
		if state := s.states[model.Key]; state != nil {
			item.ModelName = state.ModelName
			item.LastScrape = state.LastScrape
			item.LastPollSuccess = state.LastPollSuccess
			if len(state.History) > 0 {
				item.Latest = state.History[len(state.History)-1]
			}
		}
		models = append(models, item)
	}
	return s.host, models
}
