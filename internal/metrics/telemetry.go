// Package metrics exports Prometheus metrics for the local node and its peers,
// and serves the local telemetry snapshot that collectors read.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/host"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// Allow for sequential engine scrapes, rather than the UI's shorter online
	// window. Stale/failed performance series are omitted, never zero-filled.
	telemetryFreshness       = 30 * time.Second
	telemetryClockSkew       = 5 * time.Second
	telemetryPeerTimeout     = 4 * time.Second
	telemetryPeerConcurrency = 8
	telemetryMaxResponse     = 4 << 20
	telemetryVersion         = 1
)

// TelemetrySnapshot contains LOCAL data only. There are no peer addresses,
// service names, full checkpoint paths, or credentials here.
// All wire timestamps are Unix milliseconds; zero means not sampled yet.
type TelemetrySnapshot struct {
	Version       int              `json:"version"`
	Models        []ModelTelemetry `json:"models"`
	Host          sample.Sample    `json:"host"`
	KernelVersion string           `json:"kernel_version,omitempty"`
}

type ModelTelemetry struct {
	Key             string             `json:"key"`
	Name            string             `json:"name,omitempty"`
	Latest          sample.Sample      `json:"latest"`
	LastScrape      int64              `json:"last_scrape"`
	LastPollSuccess bool               `json:"last_poll_success"`
	Counters        *TelemetryCounters `json:"counters"`
}

// Counters are explicit because sample.Sample's cumulative token fields have json:"-".
// They are engine totals, not sums of dashboard samples, and may reset on an
// engine restart. Only a fresh, successful model sample exports these totals.
type TelemetryCounters struct {
	PromptTokens      float64 `json:"prompt_tokens"`
	GenerationTokens  float64 `json:"generation_tokens"`
	CompletedRequests float64 `json:"completed_requests"`
	Preemptions       float64 `json:"preemptions"`
}

var telemetryClient = &http.Client{
	Timeout: telemetryPeerTimeout,
	// A telemetry request must not follow redirects to other routes or nodes.
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// localTelemetry copies values, not history slices or state pointers.
func localTelemetry(st *store.Store) TelemetrySnapshot {
	host, models := st.Snapshot()
	snapshot := TelemetrySnapshot{
		Version:       telemetryVersion,
		Models:        make([]ModelTelemetry, 0, len(models)),
		Host:          host,
		KernelVersion: st.KernelVersion,
	}
	for _, model := range models {
		item := ModelTelemetry{Key: model.Config.Key, Name: model.Config.Name}
		if model.ModelName != "" {
			item.Name = model.ModelName
		}
		item.LastPollSuccess = model.LastPollSuccess
		if !model.LastScrape.IsZero() {
			item.LastScrape = model.LastScrape.UnixMilli()
		}
		item.Latest = model.Latest
		item.Counters = &TelemetryCounters{
			PromptTokens:      item.Latest.PromptTokensCum,
			GenerationTokens:  item.Latest.GenTokensCum,
			CompletedRequests: item.Latest.CompletedRequests,
			Preemptions:       item.Latest.PreemptionsTotal,
		}
		snapshot.Models = append(snapshot.Models, item)
	}
	return snapshot
}

func validTelemetryTimestamp(timestamp int64, now time.Time) bool {
	return timestamp >= 0 && timestamp <= now.Add(telemetryClockSkew).UnixMilli()
}

func freshTelemetryTimestamp(timestamp int64, now time.Time) bool {
	return timestamp > 0 && validTelemetryTimestamp(timestamp, now) &&
		timestamp >= now.Add(-telemetryFreshness).UnixMilli()
}

func validateTelemetry(snapshot TelemetrySnapshot, now time.Time) error {
	if snapshot.Version != telemetryVersion || snapshot.Models == nil || !validTelemetryTimestamp(snapshot.Host.Time, now) {
		return fmt.Errorf("invalid telemetry envelope")
	}
	if snapshot.KernelVersion != "" && !host.KernelVersionRe.MatchString(snapshot.KernelVersion) {
		return fmt.Errorf("invalid telemetry kernel version")
	}
	seen := make(map[string]bool, len(snapshot.Models))
	for _, model := range snapshot.Models {
		if !config.KeyRe.MatchString(model.Key) || seen[model.Key] {
			return fmt.Errorf("invalid or duplicate telemetry model key")
		}
		seen[model.Key] = true
		if !validTelemetryTimestamp(model.Latest.Time, now) || !validTelemetryTimestamp(model.LastScrape, now) ||
			model.LastScrape > model.Latest.Time || (model.LastPollSuccess && model.LastScrape == 0) {
			return fmt.Errorf("invalid telemetry model timestamp")
		}
		if model.Counters == nil {
			return fmt.Errorf("missing telemetry counters")
		}
		for _, value := range []float64{model.Counters.PromptTokens, model.Counters.GenerationTokens, model.Counters.CompletedRequests, model.Counters.Preemptions} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("invalid telemetry counter")
			}
		}
	}
	return nil
}

func fetchPeerTelemetry(ctx context.Context, peer config.Peer) (TelemetrySnapshot, error) {
	var snapshot TelemetrySnapshot
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(peer.URL, "/")+"/api/telemetry", nil)
	if err != nil {
		return snapshot, err
	}
	response, err := telemetryClient.Do(request)
	if err != nil {
		return snapshot, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("peer telemetry returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, telemetryMaxResponse+1))
	if err != nil {
		return snapshot, err
	}
	if len(data) > telemetryMaxResponse {
		return snapshot, fmt.Errorf("peer telemetry exceeds size limit")
	}
	// Unmarshal rejects trailing data as well as malformed/non-finite numbers.
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, validateTelemetry(snapshot, time.Now())
}

type telemetryNode struct {
	key      string
	name     string
	up       bool
	snapshot TelemetrySnapshot
}

func collectTelemetry(ctx context.Context, cfg config.Config, peerList []config.Peer, st *store.Store) []telemetryNode {
	local := telemetryNode{key: cfg.Node.Key, name: config.DisplayName(cfg.Node.Name, cfg.Node.Hostname), up: true, snapshot: localTelemetry(st)}
	peers := peerList

	nodes := make([]telemetryNode, len(peers)+1)
	nodes[0] = local
	for index, peer := range peers {
		nodes[index+1].key = peer.Key
		nodes[index+1].name = config.DisplayName(peer.Name, peer.Hostname)
	}
	// One shared deadline includes time spent waiting for a worker. A large
	// peer list cannot multiply the HTTP scrape's timeout or concurrency.
	ctx, cancel := context.WithTimeout(ctx, telemetryPeerTimeout)
	defer cancel()
	workers := min(len(peers), telemetryPeerConcurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := worker; index < len(peers); index += workers {
				if ctx.Err() != nil {
					return
				}
				snapshot, err := fetchPeerTelemetry(ctx, peers[index])
				if err == nil {
					nodes[index+1].snapshot = snapshot
					nodes[index+1].up = true
				}
			}
		}()
	}
	wg.Wait()
	return nodes
}

func telemetryGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// Register adds the /api/telemetry and /metrics handlers to mux.
// peers returns the current peer list. Keys and names can change at runtime.
func Register(mux *http.ServeMux, cfg config.Config, st *store.Store, peers func() []config.Peer) {
	mux.HandleFunc("/api/telemetry", func(w http.ResponseWriter, r *http.Request) {
		if !telemetryGET(w, r) {
			return
		}
		snapshot := localTelemetry(st)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodHead {
			writeJSON(w, snapshot)
		}
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !telemetryGET(w, r) {
			return
		}
		// An isolated registry and immutable request snapshot avoid cross-scrape
		// state, stale peer labels, and default Go/process collector identifiers.
		nodes := collectTelemetry(r.Context(), cfg, peers(), st)
		registry := prometheus.NewRegistry()
		registry.MustRegister(&telemetryCollector{nodes: nodes, now: time.Now()})
		if r.Method == http.MethodHead {
			w = telemetryHeadWriter{w}
		}
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}

// Discard only the body; keep the same negotiation, headers and status as GET,
// including when handlers are invoked directly rather than by an HTTP server.
type telemetryHeadWriter struct {
	http.ResponseWriter
}

func (w telemetryHeadWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
