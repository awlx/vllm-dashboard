package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPeerAPIForwardsModelAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/metrics" || r.URL.Query().Get("model") != "model-a" {
			t.Fatalf("unexpected peer request: %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode([]Sample{{Time: 123, Running: 2}})
	}))
	defer server.Close()

	var samples []Sample
	if err := peerAPI(t.Context(), PeerConfig{Key: "node-b", URL: server.URL}, "/api/metrics", "model-a", &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Running != 2 {
		t.Fatalf("unexpected samples: %+v", samples)
	}
}

func TestCombinedOverviewScopesPeerModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]ModelOverview{{Key: "node-b/model-a", Name: "Model A"}})
	}))
	defer server.Close()

	configureRuntime(DashboardConfig{
		Node:  NodeConfig{Key: "node-a", Name: "Node A"},
		Peers: []PeerConfig{{Key: "node-b", Name: "Node B", Hostname: "node-b.example.net", URL: server.URL}},
	})
	overview := combinedOverview(t.Context())
	if len(overview) != 1 || overview[0].Key != "node-b/model-a" || overview[0].NodeName != "node-b" {
		t.Fatalf("unexpected overview: %+v", overview)
	}
}
