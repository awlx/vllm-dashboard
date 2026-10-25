package main

import (
	"compress/gzip"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU sampling runs in its own goroutine. nvidia-smi and amd-smi can take
// from tens of milliseconds to more than a second, and the poll loop must
// not wait for them.

type gpuReading struct {
	util, temp, power, clock float64
	at                       time.Time
}

var (
	gpuMu      sync.Mutex
	gpuLatest  gpuReading
	gpuScraper = scrapeGPU
)

// gpuStaleAfter is the age after which a GPU reading is reported as zero,
// for example when the GPU tool hangs until its timeout on every call.
func gpuStaleAfter() time.Duration {
	return max(5*pollInterval, 2*gpuCommandTimeout)
}

func sampleGPU() time.Duration {
	start := time.Now()
	util, temp, power, clock := gpuScraper()
	now := time.Now()
	gpuMu.Lock()
	gpuLatest = gpuReading{util: util, temp: temp, power: power, clock: clock, at: now}
	gpuMu.Unlock()
	return now.Sub(start)
}

// runGPUSampler samples at the poll interval. When the GPU tool is slower
// than the interval, it waits as long as the last call took, so that the
// tool uses at most half of one CPU.
func runGPUSampler(ctx context.Context) {
	for {
		elapsed := sampleGPU()
		timer := time.NewTimer(max(pollInterval-elapsed, elapsed))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func latestGPU(now time.Time) (util, temp, power, clock float64) {
	gpuMu.Lock()
	reading := gpuLatest
	gpuMu.Unlock()
	if reading.at.IsZero() || now.Sub(reading.at) > gpuStaleAfter() {
		return 0, 0, 0, 0
	}
	return reading.util, reading.temp, reading.power, reading.clock
}

// parseNvidiaSMI reads one CSV line per GPU. Utilization, temperature, and
// clock are averaged over the GPUs that report them. Power is summed. Values
// such as "[N/A]" are skipped.
func parseNvidiaSMI(out []byte) (util, temp, power, clock float64) {
	var sums [4]float64
	var counts [4]int
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 4 {
			continue
		}
		for index := range 4 {
			value, err := strconv.ParseFloat(strings.TrimSpace(fields[index]), 64)
			if err != nil || value != value || value < 0 {
				continue
			}
			sums[index] += value
			counts[index]++
		}
	}
	average := func(index int) float64 {
		if counts[index] == 0 {
			return 0
		}
		return sums[index] / float64(counts[index])
	}
	return average(0), average(1), sums[2], average(3)
}

// ttlCache holds one value per key for a fixed time. Each key has its own
// lock, so a slow load for one model does not block other models.
type ttlCache[T any] struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*ttlEntry[T]
}

type ttlEntry[T any] struct {
	mu    sync.Mutex
	value T
	at    time.Time
}

func newTTLCache[T any](ttl time.Duration) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, now: time.Now, entries: map[string]*ttlEntry[T]{}}
}

func (c *ttlCache[T]) entry(key string) *ttlEntry[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		entry = &ttlEntry[T]{}
		c.entries[key] = entry
	}
	return entry
}

// get returns the cached value and the time it was loaded.
func (c *ttlCache[T]) get(key string, load func() T) (T, time.Time) {
	entry := c.entry(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.at.IsZero() || c.now().Sub(entry.at) >= c.ttl {
		entry.value = load()
		entry.at = c.now()
	}
	return entry.value, entry.at
}

func (c *ttlCache[T]) invalidate(key string) {
	entry := c.entry(key)
	entry.mu.Lock()
	entry.at = time.Time{}
	entry.mu.Unlock()
}

func (c *ttlCache[T]) reset() {
	c.mu.Lock()
	c.entries = map[string]*ttlEntry[T]{}
	c.mu.Unlock()
}

type modelIdentity struct {
	info ServerInfo
	pid  string
}

// Process discovery runs pgrep, ps, and systemctl. Cache the results so that
// polls and API requests do not start new processes every time.
var (
	identityCache   = newTTLCache[modelIdentity](30 * time.Second)
	serverInfoCache = newTTLCache[ServerInfo](10 * time.Second)
)

func cachedModelIdentity(model ModelConfig) (ServerInfo, string) {
	identity, _ := identityCache.get(model.Key, func() modelIdentity {
		info, pid := buildModelIdentity(model)
		return modelIdentity{info: info, pid: pid}
	})
	return identity.info, identity.pid
}

func cachedServerInfo(model ModelConfig) ServerInfo {
	info, at := serverInfoCache.get(model.Key, func() ServerInfo { return buildServerInfo(model) })
	if info.ServiceUptimeSeconds > 0 {
		info.ServiceUptimeSeconds += int64(time.Since(at) / time.Second)
	}
	return info
}

// fanOutPeers calls fn for each peer in parallel. All calls share one
// deadline, so many peers do not multiply the peer timeout.
func fanOutPeers(ctx context.Context, peers []PeerConfig, fn func(ctx context.Context, index int, peer PeerConfig)) {
	ctx, cancel := context.WithTimeout(ctx, telemetryPeerTimeout)
	defer cancel()
	limit := make(chan struct{}, telemetryPeerConcurrency)
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

var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return writer
}}

// gzipJSON compresses successful responses when the client accepts gzip.
// History responses are large and compress well.
func gzipJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}
		writer := &gzipResponseWriter{ResponseWriter: w}
		defer writer.close()
		next(writer, r)
	}
}

func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			return strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0"
		}
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status == http.StatusOK {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.gz = gzipWriters.Get().(*gzip.Writer)
		w.gz.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz == nil {
		return w.ResponseWriter.Write(data)
	}
	return w.gz.Write(data)
}

func (w *gzipResponseWriter) close() {
	if w.gz != nil {
		w.gz.Close()
		gzipWriters.Put(w.gz)
		w.gz = nil
	}
}
