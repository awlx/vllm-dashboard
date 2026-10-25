package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func TestPeerAPIForwardsModelAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/metrics" || r.URL.Query().Get("model") != "model-a" {
			t.Fatalf("unexpected peer request: %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode([]sample.Sample{{Time: 123, Running: 2}})
	}))
	defer server.Close()

	f := New(config.Config{}, store.New(config.Config{}), process.NewCache("", ""))
	var samples []sample.Sample
	if err := f.PeerAPI(t.Context(), config.Peer{Key: "node-b", URL: server.URL}, "/api/metrics", "model-a", &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Running != 2 {
		t.Fatalf("unexpected samples: %+v", samples)
	}
}

func TestCombinedOverviewScopesPeerModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]sample.ModelOverview{{Key: "node-b/model-a", Name: "Model A"}})
	}))
	defer server.Close()

	cfg := config.Config{
		Node:  config.Node{Key: "node-a", Name: "Node A"},
		Peers: []config.Peer{{Key: "node-b", Name: "Node B", Hostname: "node-b.example.net", URL: server.URL}},
	}
	overview := New(cfg, store.New(cfg), process.NewCache("", "")).CombinedOverview(t.Context())
	if len(overview) != 1 || overview[0].Key != "node-b/model-a" || overview[0].NodeName != "node-b" {
		t.Fatalf("unexpected overview: %+v", overview)
	}
}
