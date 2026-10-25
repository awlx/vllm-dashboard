package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func localModelKey(key string) (string, bool) {
	prefix := config.Node.Key + "/"
	key = strings.TrimPrefix(key, prefix)
	for _, model := range models {
		if model.Key == key {
			return key, true
		}
	}
	return "", false
}

func peerForModel(key string) (PeerConfig, string, bool) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return PeerConfig{}, "", false
	}
	for _, peer := range config.Peers {
		if peer.Key == parts[0] {
			return peer, parts[1], true
		}
	}
	return PeerConfig{}, "", false
}

func peerAPI(ctx context.Context, peer PeerConfig, path, model string, target any) error {
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
	response, err := peerClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s returned %s", peer.Key, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func combinedSamples(ctx context.Context, long bool) []Sample {
	mu.RLock()
	local := append([]Sample(nil), aggregateLocalSamples(long)...)
	mu.RUnlock()
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	peerSamples := make([][]Sample, len(config.Peers))
	fanOutPeers(ctx, config.Peers, func(ctx context.Context, index int, peer PeerConfig) {
		if err := peerAPI(ctx, peer, path, "all", &peerSamples[index]); err != nil {
			log.Printf("peer %s metrics: %v", peer.Key, err)
			peerSamples[index] = nil
		}
	})
	return aggregateSampleSets(append([][]Sample{local}, peerSamples...))
}

func metricsForRequest(ctx context.Context, key string, long bool) ([]Sample, error) {
	if key == "" || key == "all" {
		return combinedSamples(ctx, long), nil
	}
	if localKey, ok := localModelKey(key); ok {
		mu.RLock()
		defer mu.RUnlock()
		return append([]Sample(nil), samplesFor(localKey, long)...), nil
	}
	peer, peerModel, ok := peerForModel(key)
	if !ok {
		return nil, os.ErrNotExist
	}
	path := "/api/metrics"
	if long {
		path += "/long"
	}
	var samples []Sample
	return samples, peerAPI(ctx, peer, path, peerModel, &samples)
}

func localOverview() []ModelOverview {
	overview := make([]ModelOverview, 0, len(models))
	for _, model := range models {
		info := cachedServerInfo(model)
		mu.RLock()
		state := modelStates[model.Key]
		item := ModelOverview{
			Key: scopedModelKey(config.Node.Key, model.Key), Name: modelDisplayName(info, model.Name),
			NodeKey: config.Node.Key, NodeName: displayName(config.Node.Name, config.Node.Hostname), Info: info,
		}
		if len(state.History) > 0 {
			item.Latest = state.History[len(state.History)-1]
		}
		item.Online = !state.LastScrape.IsZero() && time.Since(state.LastScrape) < onlineWindow()
		item.Tokens = usageForState(state)
		mu.RUnlock()
		overview = append(overview, item)
	}
	return overview
}

func combinedOverview(ctx context.Context) []ModelOverview {
	overview := localOverview()
	results := make([][]ModelOverview, len(config.Peers))
	fanOutPeers(ctx, config.Peers, func(ctx context.Context, index int, peer PeerConfig) {
		if err := peerAPI(ctx, peer, "/api/overview", "", &results[index]); err != nil {
			log.Printf("peer %s overview: %v", peer.Key, err)
			results[index] = nil
		}
	})
	for peerIndex, peer := range config.Peers {
		peerModels := results[peerIndex]
		for index := range peerModels {
			modelKey := peerModels[index].Key
			if strings.Contains(modelKey, "/") {
				modelKey = strings.SplitN(modelKey, "/", 2)[1]
			}
			peerModels[index].Key = scopedModelKey(peer.Key, modelKey)
			peerModels[index].NodeKey = peer.Key
			peerModels[index].NodeName = displayName(peer.Name, peer.Hostname)
		}
		overview = append(overview, peerModels...)
	}
	return overview
}
