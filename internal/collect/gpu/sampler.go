package gpu

import (
	"context"
	"sync"
	"time"
)

// Sampler runs the GPU tool in its own goroutine. nvidia-smi and amd-smi can
// take from tens of milliseconds to more than a second, and the poll loop
// must not wait for them.
type Sampler struct {
	interval time.Duration
	scrape   func() (util, temp, power, clock float64)

	mu     sync.Mutex
	latest reading
}

type reading struct {
	util, temp, power, clock float64
	at                       time.Time
}

// NewSampler returns a sampler that reads the GPUs at interval.
func NewSampler(interval time.Duration) *Sampler {
	return &Sampler{interval: interval, scrape: Scrape}
}

// StaleAfter is the age after which a GPU reading is reported as zero,
// for example when the GPU tool hangs until its timeout on every call.
func (s *Sampler) StaleAfter() time.Duration {
	return max(5*s.interval, 2*commandTimeout)
}

// Sample reads the GPUs once and returns how long the read took.
func (s *Sampler) Sample() time.Duration {
	start := time.Now()
	util, temp, power, clock := s.scrape()
	now := time.Now()
	s.mu.Lock()
	s.latest = reading{util: util, temp: temp, power: power, clock: clock, at: now}
	s.mu.Unlock()
	return now.Sub(start)
}

// Run samples at the poll interval until ctx ends. When the GPU tool is
// slower than the interval, it waits as long as the last call took, so that
// the tool uses at most half of one CPU.
func (s *Sampler) Run(ctx context.Context) {
	for {
		elapsed := s.Sample()
		timer := time.NewTimer(max(s.interval-elapsed, elapsed))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Latest returns the last reading, or zeros if it is missing or stale.
func (s *Sampler) Latest(now time.Time) (util, temp, power, clock float64) {
	s.mu.Lock()
	r := s.latest
	s.mu.Unlock()
	if r.at.IsZero() || now.Sub(r.at) > s.StaleAfter() {
		return 0, 0, 0, 0
	}
	return r.util, r.temp, r.power, r.clock
}

// NewSamplerFunc returns a sampler that calls scrape instead of the GPU tools.
func NewSamplerFunc(interval time.Duration, scrape func() (util, temp, power, clock float64)) *Sampler {
	return &Sampler{interval: interval, scrape: scrape}
}
