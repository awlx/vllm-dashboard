package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func TestRegisterRoutesLocalPeerAndUnknownKeys(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tokens":
			_ = json.NewEncoder(w).Encode(sample.TokenUsage{LastHour: sample.TokenWindow{PromptTokens: 5, TotalTokens: 5}})
		case "/api/metrics":
			if r.URL.Query().Get("model") != "model-b" {
				t.Errorf("unexpected peer model %q", r.URL.Query().Get("model"))
			}
			_ = json.NewEncoder(w).Encode([]sample.Sample{{Time: 1, Running: 7}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()

	cfg := config.Config{
		Node:   config.Node{Key: "node-a", Name: "Node A", Models: []config.Model{{Key: "model-a", Name: "Model A", MetricsURL: "http://127.0.0.1:1/metrics"}}},
		Peers:  []config.Peer{{Key: "node-b", Name: "Node B", URL: peer.URL}},
		Listen: ":0",
	}
	st := store.New(cfg)
	st.Update("model-a", func(state *store.ModelState) {
		state.History = []sample.Sample{{Time: time.Now().UnixMilli(), Running: 3}}
	})

	mux := http.NewServeMux()
	Register(mux, fleet.New(cfg, st, process.NewCache("", "")))
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/api/metrics?model=node-a/model-a", http.StatusOK, `"running":3`},
		{"/api/metrics?model=model-a", http.StatusOK, `"running":3`},
		{"/api/metrics?model=node-b/model-b", http.StatusOK, `"running":7`},
		{"/api/metrics?model=node-c/model-x", http.StatusBadGateway, "model unavailable"},
		{"/api/info?model=all", http.StatusNotFound, "model not found"},
		{"/api/tokens?model=node-c/model-x", http.StatusNotFound, "model not found"},
		{"/api/tokens?model=all", http.StatusOK, `"prompt_tokens":5`},
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), tc.body) {
			t.Errorf("%s: got %d %q, want %d containing %q", tc.path, recorder.Code, recorder.Body.String(), tc.status, tc.body)
		}
	}
}

func TestNodeReportsLocalIdentity(t *testing.T) {
	cfg := config.Config{Node: config.Node{Key: "gpu-a", Hostname: "gpu-a.example.net"}}
	mux := http.NewServeMux()
	Register(mux, fleet.New(cfg, store.New(cfg), process.NewCache("", "")))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/node", nil))
	var info fleet.NodeInfo
	if err := json.NewDecoder(recorder.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info != (fleet.NodeInfo{Key: "gpu-a", Hostname: "gpu-a.example.net"}) {
		t.Fatalf("node = %+v", info)
	}
}
