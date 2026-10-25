package fleet

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
	"github.com/awlx/vllm-dashboard/internal/store"
)

func TestFanOutRunsInParallelAndKeepsOrder(t *testing.T) {
	peers := make([]config.Peer, 6)
	for index := range peers {
		peers[index] = config.Peer{Key: string(rune('a' + index))}
	}
	results := make([]string, len(peers))
	var running, peak atomic.Int32
	start := time.Now()
	FanOut(t.Context(), peers, func(ctx context.Context, index int, peer config.Peer) {
		current := running.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		running.Add(-1)
		results[index] = peer.Key
	})
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("fan-out took %v, want parallel calls", elapsed)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrency = %d", peak.Load())
	}
	for index, key := range results {
		if key != peers[index].Key {
			t.Fatalf("results[%d] = %q, want %q", index, key, peers[index].Key)
		}
	}
}

func TestFanOutSharesOneDeadline(t *testing.T) {
	peers := make([]config.Peer, PeerConcurrency*2)
	var mu sync.Mutex
	deadlines := map[time.Time]bool{}
	FanOut(t.Context(), peers, func(ctx context.Context, index int, peer config.Peer) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("peer call has no deadline")
		}
		mu.Lock()
		deadlines[deadline] = true
		mu.Unlock()
	})
	if len(deadlines) != 1 {
		t.Fatalf("got %d deadlines, want 1", len(deadlines))
	}
}

func TestPeerAPIDecodesGzipResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			_ = json.NewEncoder(w).Encode(sample.TokenUsage{})
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_ = json.NewEncoder(gz).Encode(sample.TokenUsage{})
		_ = gz.Close()
	}))
	defer server.Close()
	f := New(config.Config{}, store.New(config.Config{}), process.NewCache("", ""))
	var usage sample.TokenUsage
	if err := f.PeerAPI(t.Context(), config.Peer{Key: "p", URL: server.URL}, "/api/tokens", "all", &usage); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !response.Uncompressed {
		t.Fatal("transport did not request and decode gzip")
	}
	_, _ = io.Copy(io.Discard, response.Body)
}
