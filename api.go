package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"
)

func requestedModel(r *http.Request) string {
	key := r.URL.Query().Get("model")
	if key == "" || key == "all" {
		return key
	}
	return key
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// DashboardSettings lets the browser follow the server poll cadence.
type DashboardSettings struct {
	PollIntervalMs       int64 `json:"poll_interval_ms"`
	HistoryWindowSeconds int64 `json:"history_window_seconds"`
	LongIntervalSeconds  int64 `json:"long_interval_seconds"`
}

func currentSettings() DashboardSettings {
	return DashboardSettings{
		PollIntervalMs:       pollInterval.Milliseconds(),
		HistoryWindowSeconds: int64(historyWindow / time.Second),
		LongIntervalSeconds:  int64(longInterval / time.Second),
	}
}

func newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerMetricsHandlers(mux)

	mux.HandleFunc("/api/metrics", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		samples, err := metricsForRequest(r.Context(), requestedModel(r), false)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, samples)
	}))

	mux.HandleFunc("/api/metrics/long", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		samples, err := metricsForRequest(r.Context(), requestedModel(r), true)
		if err != nil {
			http.Error(w, "model unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, samples)
	}))

	mux.HandleFunc("/api/info", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		if localKey, ok := localModelKey(key); ok {
			writeJSON(w, cachedServerInfo(modelConfig(localKey)))
			return
		}
		peer, peerModel, ok := peerForModel(key)
		if !ok {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		var info ServerInfo
		if err := peerAPI(r.Context(), peer, "/api/info", peerModel, &info); err != nil {
			http.Error(w, "peer unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, info)
	}))

	mux.HandleFunc("/api/tokens", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		key := requestedModel(r)
		var usage TokenUsage
		if key == "" || key == "all" {
			mu.RLock()
			usage = usageForKey("all")
			mu.RUnlock()
			peerUsage := make([]TokenUsage, len(config.Peers))
			fanOutPeers(r.Context(), config.Peers, func(ctx context.Context, index int, peer PeerConfig) {
				if err := peerAPI(ctx, peer, "/api/tokens", "all", &peerUsage[index]); err != nil {
					peerUsage[index] = TokenUsage{}
				}
			})
			for _, item := range peerUsage {
				usage = addUsage(usage, item)
			}
		} else if localKey, ok := localModelKey(key); ok {
			mu.RLock()
			usage = usageForKey(localKey)
			mu.RUnlock()
		} else if peer, peerModel, ok := peerForModel(key); ok {
			if err := peerAPI(r.Context(), peer, "/api/tokens", peerModel, &usage); err != nil {
				http.Error(w, "peer unavailable", http.StatusBadGateway)
				return
			}
		} else {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		writeJSON(w, usage)
	}))

	mux.HandleFunc("/api/settings", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, currentSettings())
	}))

	mux.HandleFunc("/api/overview", gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, combinedOverview(r.Context()))
	}))

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}
