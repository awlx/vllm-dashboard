// Package api serves the JSON API that the UI and the fleet collector use.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func requestedModel(r *http.Request) string {
	key := r.URL.Query().Get("model")
	if key == "" || key == "all" {
		return key
	}
	return key
}

// WriteJSON writes value as a JSON response.
func WriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// Settings lets the browser follow the server poll cadence.
type Settings struct {
	PollIntervalMs       int64 `json:"poll_interval_ms"`
	HistoryWindowSeconds int64 `json:"history_window_seconds"`
	LongIntervalSeconds  int64 `json:"long_interval_seconds"`
}

// CurrentSettings returns the settings of the local store.
func CurrentSettings(st *store.Store) Settings {
	return Settings{
		PollIntervalMs:       st.Interval().Milliseconds(),
		HistoryWindowSeconds: int64(store.HistoryWindow / time.Second),
		LongIntervalSeconds:  int64(store.LongInterval / time.Second),
	}
}

// Register adds the /api/metrics, /api/metrics/long, /api/info, /api/tokens,
// /api/settings, /api/overview, and /api/node handlers to mux. All of them
// use GzipJSON.
func Register(mux *http.ServeMux, f *fleet.Fleet) {
	st := f.Store()
	mux.HandleFunc("/api/node", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		node := st.Node()
		WriteJSON(w, fleet.NodeInfo{Key: node.Key, Name: node.Name, Hostname: node.Hostname})
	}))

	mux.HandleFunc("/api/metrics", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		samples, err := f.MetricsForRequest(r.Context(), requestedModel(r), false)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		WriteJSON(w, samples)
	}))

	mux.HandleFunc("/api/metrics/long", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		samples, err := f.MetricsForRequest(r.Context(), requestedModel(r), true)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		WriteJSON(w, samples)
	}))

	mux.HandleFunc("/api/info", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		if localKey, ok := st.LocalModelKey(key); ok {
			WriteJSON(w, f.ServerInfo(st.ModelConfig(localKey)))
			return
		}
		peer, peerModel, ok := f.PeerForModel(key)
		if !ok {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		var info sample.ServerInfo
		if err := f.PeerAPI(r.Context(), peer, "/api/info", peerModel, &info); err != nil {
			http.Error(w, "peer unavailable", http.StatusBadGateway)
			return
		}
		WriteJSON(w, info)
	}))

	mux.HandleFunc("/api/tokens", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		var usage sample.TokenUsage
		if key == "" || key == "all" {
			usage = st.UsageForKey("all")
			peers := f.Peers()
			peerUsage := make([]sample.TokenUsage, len(peers))
			fleet.FanOut(r.Context(), peers, func(ctx context.Context, index int, peer config.Peer) {
				if err := f.PeerAPI(ctx, peer, "/api/tokens", "all", &peerUsage[index]); err != nil {
					peerUsage[index] = sample.TokenUsage{}
				}
			})
			for _, item := range peerUsage {
				usage = store.AddUsage(usage, item)
			}
		} else if localKey, ok := st.LocalModelKey(key); ok {
			usage = st.UsageForKey(localKey)
		} else if peer, peerModel, ok := f.PeerForModel(key); ok {
			if err := f.PeerAPI(r.Context(), peer, "/api/tokens", peerModel, &usage); err != nil {
				http.Error(w, "peer unavailable", http.StatusBadGateway)
				return
			}
		} else {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		WriteJSON(w, usage)
	}))

	mux.HandleFunc("/api/settings", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, CurrentSettings(st))
	}))

	mux.HandleFunc("/api/overview", GzipJSON(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, f.CombinedOverview(r.Context()))
	}))
}
