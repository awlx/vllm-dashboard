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

func TestResolvePeersUsesReportedIdentity(t *testing.T) {
	reports := map[string]NodeInfo{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/node":
				info, ok := reports[name]
				if !ok {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(info)
			case "/api/overview":
				_ = json.NewEncoder(w).Encode([]sample.ModelOverview{{Key: "m", Name: "M", NodeName: reports[name].Hostname}})
			}
		}
	}
	reports["b"] = NodeInfo{Key: "gpu-b", Hostname: "gpu-b.example.net"}
	reports["c"] = NodeInfo{Key: "local", Name: "Clash"}
	b, c, d := httptest.NewServer(handler("b")), httptest.NewServer(handler("c")), httptest.NewServer(handler("d"))
	defer b.Close()
	defer c.Close()
	defer d.Close()

	cfg := config.Config{
		Node:  config.Node{Key: "local", Name: "Local"},
		Peers: []config.Peer{{Key: "fallback-b", Name: "b", URL: b.URL}, {Key: "fallback-c", Name: "c", URL: c.URL}, {Key: "fallback-d", Name: "d", URL: d.URL}},
	}
	f := New(cfg, store.New(cfg), process.NewCache("", ""))
	f.ResolvePeers()
	peers := f.Peers()
	if peers[0].Key != "gpu-b" || peers[0].Hostname != "gpu-b.example.net" || peers[0].Name != "" {
		t.Errorf("resolved peer = %+v", peers[0])
	}
	if peers[1].Key != "fallback-c" {
		t.Errorf("peer with a key in use = %+v", peers[1])
	}
	if peers[2].Key != "fallback-d" || peers[2].Name != "d" {
		t.Errorf("peer without /api/node = %+v", peers[2])
	}
	if _, model, ok := f.PeerForModel("gpu-b/m"); !ok || model != "m" {
		t.Errorf("PeerForModel(gpu-b/m) = %q, %v", model, ok)
	}
	overview := f.CombinedOverview(t.Context())
	if len(overview) != 3 || overview[0].Key != "gpu-b/m" || overview[0].NodeName != "gpu-b.example.net" || overview[2].NodeName != "d" {
		t.Errorf("overview = %+v", overview)
	}
}
