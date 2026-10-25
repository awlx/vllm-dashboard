package main

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
)

func TestParseNvidiaSMI(t *testing.T) {
	tests := []struct {
		name                     string
		out                      string
		util, temp, power, clock float64
	}{
		{"one GPU", "45, 61, 250.5, 1980\n", 45, 61, 250.5, 1980},
		{"two GPUs", "40, 60, 200, 1800\n60, 70, 300, 2000\n", 50, 65, 500, 1900},
		{"missing values", "40, 60, [N/A], 1800\n[N/A], 70, 300, [N/A]\n", 40, 65, 300, 1800},
		{"no trailing newline", "10, 50, 100, 1000\n30, 50, 100, 1200", 20, 50, 200, 1100},
		{"empty", "", 0, 0, 0, 0},
		{"short line", "10, 20\n", 0, 0, 0, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			util, temp, power, clock := parseNvidiaSMI([]byte(test.out))
			if util != test.util || temp != test.temp || power != test.power || clock != test.clock {
				t.Fatalf("got %v %v %v %v, want %v %v %v %v", util, temp, power, clock,
					test.util, test.temp, test.power, test.clock)
			}
		})
	}
}

func setGPUScraper(t *testing.T, scraper func() (float64, float64, float64, float64)) {
	t.Helper()
	previous := gpuScraper
	gpuScraper = scraper
	t.Cleanup(func() {
		gpuScraper = previous
		gpuMu.Lock()
		gpuLatest = gpuReading{}
		gpuMu.Unlock()
	})
}

func TestLatestGPUReturnsZeroWhenStale(t *testing.T) {
	configureRuntime(DashboardConfig{})
	setGPUScraper(t, func() (float64, float64, float64, float64) { return 50, 60, 300, 1900 })
	now := time.Now()
	if util, _, _, _ := latestGPU(now); util != 0 {
		t.Fatalf("util before first sample = %v, want 0", util)
	}
	sampleGPU()
	if util, temp, power, clock := latestGPU(time.Now()); util != 50 || temp != 60 || power != 300 || clock != 1900 {
		t.Fatalf("got %v %v %v %v", util, temp, power, clock)
	}
	if util, _, _, _ := latestGPU(time.Now().Add(gpuStaleAfter() + time.Second)); util != 0 {
		t.Fatalf("stale util = %v, want 0", util)
	}
}

func TestSlowGPUScrapeDoesNotBlockPoll(t *testing.T) {
	server := newMockVLLM(t)
	configureRuntime(DashboardConfig{Node: NodeConfig{Key: "n", Models: []ModelConfig{{Key: "m", MetricsURL: server.URL + "/metrics"}}}})
	release := make(chan struct{})
	setGPUScraper(t, func() (float64, float64, float64, float64) {
		<-release
		return 1, 1, 1, 1
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runGPUSampler(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		<-done
	})

	start := time.Now()
	poll()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("poll took %v while the GPU tool was blocked", elapsed)
	}
	mu.RLock()
	samples := len(modelStates["m"].History)
	mu.RUnlock()
	if samples != 1 {
		t.Fatalf("history has %d samples, want 1", samples)
	}
}

func TestTTLCacheExpiresAndInvalidates(t *testing.T) {
	cache := newTTLCache[int](10 * time.Second)
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }
	loads := 0
	load := func() int { loads++; return loads }

	if value, at := cache.get("a", load); value != 1 || !at.Equal(now) {
		t.Fatalf("first get = %d at %v", value, at)
	}
	now = now.Add(9 * time.Second)
	if value, _ := cache.get("a", load); value != 1 {
		t.Fatalf("get inside TTL = %d, want cached 1", value)
	}
	if value, _ := cache.get("b", load); value != 2 {
		t.Fatalf("other key = %d, want new load 2", value)
	}
	now = now.Add(time.Second)
	if value, _ := cache.get("a", load); value != 3 {
		t.Fatalf("get after TTL = %d, want 3", value)
	}
	cache.invalidate("a")
	if value, _ := cache.get("a", load); value != 4 {
		t.Fatalf("get after invalidate = %d, want 4", value)
	}
	cache.reset()
	if value, _ := cache.get("b", load); value != 5 {
		t.Fatalf("get after reset = %d, want 5", value)
	}
}

func TestTTLCacheSlowKeyDoesNotBlockOtherKeys(t *testing.T) {
	cache := newTTLCache[int](time.Minute)
	release := make(chan struct{})
	started := make(chan struct{})
	go cache.get("slow", func() int {
		close(started)
		<-release
		return 1
	})
	<-started
	defer close(release)
	done := make(chan int)
	go func() {
		value, _ := cache.get("fast", func() int { return 2 })
		done <- value
	}()
	select {
	case value := <-done:
		if value != 2 {
			t.Fatalf("fast key = %d", value)
		}
	case <-time.After(time.Second):
		t.Fatal("fast key waited for slow key")
	}
}

func TestFanOutPeersRunsInParallelAndKeepsOrder(t *testing.T) {
	peers := make([]PeerConfig, 6)
	for index := range peers {
		peers[index] = PeerConfig{Key: string(rune('a' + index))}
	}
	results := make([]string, len(peers))
	var running, peak atomic.Int32
	start := time.Now()
	fanOutPeers(t.Context(), peers, func(ctx context.Context, index int, peer PeerConfig) {
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

func TestFanOutPeersSharesOneDeadline(t *testing.T) {
	peers := make([]PeerConfig, telemetryPeerConcurrency*2)
	var mu sync.Mutex
	deadlines := map[time.Time]bool{}
	fanOutPeers(t.Context(), peers, func(ctx context.Context, index int, peer PeerConfig) {
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

func TestGzipJSON(t *testing.T) {
	handler := gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})
	request := func(method, target, encoding string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		if encoding != "" {
			r.Header.Set("Accept-Encoding", encoding)
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}

	response := request(http.MethodGet, "/", "br, gzip")
	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("response is not compressed")
	}
	if response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("missing Vary header")
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.NewDecoder(reader).Decode(&body); err != nil || body["status"] != "ok" {
		t.Fatalf("decoded %v, %v", body, err)
	}

	for _, test := range []struct{ name, method, target, encoding string }{
		{"no header", http.MethodGet, "/", ""},
		{"gzip refused", http.MethodGet, "/", "gzip;q=0"},
		{"error status", http.MethodGet, "/?fail=1", "gzip"},
		{"head", http.MethodHead, "/", "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(test.method, test.target, test.encoding)
			if encoding := response.Header().Get("Content-Encoding"); encoding != "" {
				t.Fatalf("Content-Encoding = %q, want none", encoding)
			}
		})
	}
}

func TestPeerAPIDecodesGzipResponse(t *testing.T) {
	server := httptest.NewServer(gzipJSON(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, TokenUsage{})
	}))
	defer server.Close()
	var usage TokenUsage
	if err := peerAPI(t.Context(), PeerConfig{Key: "p", URL: server.URL}, "/api/tokens", "all", &usage); err != nil {
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

func TestScrapeVLLMSkipsBucketsAndOtherSeries(t *testing.T) {
	body := "# HELP vllm:e2e_request_latency_seconds x\n" +
		"vllm:e2e_request_latency_seconds_bucket{le=\"1.0\"} 5\n" +
		"vllm:e2e_request_latency_seconds_bucket{le=\"+Inf\"} 9\n" +
		"vllm:e2e_request_latency_seconds_sum 4.5\n" +
		"vllm:e2e_request_latency_seconds_count 9\n" +
		"process_cpu_seconds_total 12\n" +
		"vllm:num_requests_running{model_name=\"m\"} 2\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	metrics, err := scrapeVLLM(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"vllm:e2e_request_latency_seconds_sum":   4.5,
		"vllm:e2e_request_latency_seconds_count": 9,
		"vllm:num_requests_running":              2,
	}
	if len(metrics) != len(want) {
		t.Fatalf("got %v, want %v", metrics, want)
	}
	for name, value := range want {
		if metrics[name] != value {
			t.Fatalf("%s = %v, want %v", name, metrics[name], value)
		}
	}
}
