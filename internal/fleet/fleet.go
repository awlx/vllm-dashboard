// Package fleet routes model keys to the local store or to peer dashboards
// and merges the results.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

const (
	// PeerTimeout bounds one peer call, and all peer calls of one request.
	PeerTimeout = 4 * time.Second
	// PeerConcurrency is the maximum number of parallel peer calls.
	PeerConcurrency = 8
)

// Fleet combines the local store with the configured peers.
type Fleet struct {
	cfg    config.Config
	store  *store.Store
	procs  *process.Cache
	client *http.Client

	mu    sync.RWMutex
	peers []config.Peer
}

// New returns a fleet view for cfg, the local store, and the process cache.
func New(cfg config.Config, st *store.Store, procs *process.Cache) *Fleet {
	return &Fleet{cfg: cfg, store: st, procs: procs, client: &http.Client{Timeout: PeerTimeout}, peers: append([]config.Peer(nil), cfg.Peers...)}
}

// FanOut calls fn for each peer in parallel. All calls share one
// deadline, so many peers do not multiply the peer timeout.
func FanOut(ctx context.Context, peers []config.Peer, fn func(ctx context.Context, index int, peer config.Peer)) {
	ctx, cancel := context.WithTimeout(ctx, PeerTimeout)
	defer cancel()
	limit := make(chan struct{}, PeerConcurrency)
	var wg sync.WaitGroup
	for index, peer := range peers {
		wg.Go(func() {
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			fn(ctx, index, peer)
		})
	}
	wg.Wait()
}

// Store returns the local store.
func (f *Fleet) Store() *store.Store {
	return f.store
}

// Peers returns the peers with their current keys and names.
func (f *Fleet) Peers() []config.Peer {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]config.Peer(nil), f.peers...)
}

// NodeInfo is the response of /api/node.
type NodeInfo struct {
	Key      string `json:"key"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// ResolveEvery is the time between two reads of the peer identities.
const ResolveEvery = 30 * time.Second

// RunResolver calls ResolvePeers now and then every ResolveEvery.
func (f *Fleet) RunResolver() {
	for {
		f.ResolvePeers()
		time.Sleep(ResolveEvery)
	}
}

// ResolvePeers reads /api/node of each peer and sets the key and the name
// that the peer reports. A peer that does not answer, or that reports a
// key that is in use, keeps its current values.
func (f *Fleet) ResolvePeers() {
	current := f.Peers()
	reported := make([]NodeInfo, len(current))
	FanOut(context.Background(), current, func(ctx context.Context, index int, peer config.Peer) {
		var info NodeInfo
		if err := f.PeerAPI(ctx, peer, "/api/node", "", &info); err != nil {
			return
		}
		if config.KeyRe.MatchString(info.Key) && config.DisplayName(info.Name, info.Hostname) != "" && (info.Hostname == "" || config.ValidHostname(info.Hostname)) {
			reported[index] = info
		}
	})

	used := map[string]bool{f.cfg.Node.Key: true}
	for index, info := range reported {
		if info.Key == "" {
			used[current[index].Key] = true
		}
	}
	for index, info := range reported {
		if info.Key == "" {
			continue
		}
		peer := &current[index]
		if info.Key != peer.Key && used[info.Key] {
			log.Printf("peer %s reports key %q, which is in use; keeping %q", peer.URL, info.Key, peer.Key)
			used[peer.Key] = true
			continue
		}
		if info.Key != peer.Key {
			log.Printf("peer %s is node %q", peer.URL, info.Key)
		}
		peer.Key, peer.Name, peer.Hostname = info.Key, info.Name, info.Hostname
		used[peer.Key] = true
	}
	f.mu.Lock()
	f.peers = current
	f.mu.Unlock()
}

// PeerForModel splits a "<peer.key>/<model key>" key.
func (f *Fleet) PeerForModel(key string) (config.Peer, string, bool) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return config.Peer{}, "", false
	}
	for _, peer := range f.Peers() {
		if peer.Key == parts[0] {
			return peer, parts[1], true
		}
	}
	return config.Peer{}, "", false
}

// PeerAPI calls a JSON endpoint of a peer and decodes the response.
func (f *Fleet) PeerAPI(ctx context.Context, peer config.Peer, path, model string, target any) error {
	endpoint, err := url.Parse(strings.TrimRight(peer.URL, "/") + path)
	if err != nil {
		return err
	}
	if model != "" {
		query := endpoint.Query()
		query.Set("model", model)
		endpoint.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := f.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s returned %s", peer.Key, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (f *Fleet) combinedSamples(ctx context.Context, long bool) []sample.Sample {
	local := f.store.AggregateLocal(long)
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	peers := f.Peers()
	peerSamples := make([][]sample.Sample, len(peers))
	FanOut(ctx, peers, func(ctx context.Context, index int, peer config.Peer) {
		if err := f.PeerAPI(ctx, peer, path, "all", &peerSamples[index]); err != nil {
			log.Printf("peer %s metrics: %v", peer.Key, err)
			peerSamples[index] = nil
		}
	})
	return store.AggregateSampleSets(append([][]sample.Sample{local}, peerSamples...))
}

// MetricsForRequest returns the history for "all", a local key, or a peer key.
func (f *Fleet) MetricsForRequest(ctx context.Context, key string, long bool) ([]sample.Sample, error) {
	if key == "" || key == "all" {
		return f.combinedSamples(ctx, long), nil
	}
	if localKey, ok := f.store.LocalModelKey(key); ok {
		return f.store.Samples(localKey, long), nil
	}
	peer, peerModel, ok := f.PeerForModel(key)
	if !ok {
		return nil, os.ErrNotExist
	}
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	var samples []sample.Sample
	return samples, f.PeerAPI(ctx, peer, path, peerModel, &samples)
}

// ServerInfo returns the cached runtime information of a local model.
func (f *Fleet) ServerInfo(model config.Model) sample.ServerInfo {
	return f.procs.ServerInfo(model)
}

// LocalOverview returns one overview item for each local model.
func (f *Fleet) LocalOverview() []sample.ModelOverview {
	node := f.store.Node()
	models := f.store.Models()
	overview := make([]sample.ModelOverview, 0, len(models))
	for _, model := range models {
		info := f.ServerInfo(model)
		status := f.store.Status(model.Key)
		item := sample.ModelOverview{
			Key: config.ScopedModelKey(node.Key, model.Key), Name: process.DisplayName(info, model.Name),
			NodeKey: node.Key, NodeName: config.DisplayName(node.Name, node.Hostname), Info: info,
			Latest: status.Latest,
		}
		item.Online = !status.LastScrape.IsZero() && time.Since(status.LastScrape) < f.store.OnlineWindow()
		item.Tokens = status.Tokens
		overview = append(overview, item)
	}
	return overview
}

// CombinedOverview returns the local items and the items of each peer.
func (f *Fleet) CombinedOverview(ctx context.Context) []sample.ModelOverview {
	overview := f.LocalOverview()
	peers := f.Peers()
	results := make([][]sample.ModelOverview, len(peers))
	FanOut(ctx, peers, func(ctx context.Context, index int, peer config.Peer) {
		if err := f.PeerAPI(ctx, peer, "/api/overview", "", &results[index]); err != nil {
			log.Printf("peer %s overview: %v", peer.Key, err)
			results[index] = nil
		}
	})
	for peerIndex, peer := range peers {
		peerModels := results[peerIndex]
		for index := range peerModels {
			modelKey := peerModels[index].Key
			if strings.Contains(modelKey, "/") {
				modelKey = strings.SplitN(modelKey, "/", 2)[1]
			}
			peerModels[index].Key = config.ScopedModelKey(peer.Key, modelKey)
			peerModels[index].NodeKey = peer.Key
			// The peer reports its own node name. The URL host is the fallback.
			if peerModels[index].NodeName == "" {
				peerModels[index].NodeName = config.DisplayName(peer.Name, peer.Hostname)
			}
		}
		overview = append(overview, peerModels...)
	}
	return overview
}
