package gpu

import (
	"context"
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

func TestLatestReturnsZeroWhenStale(t *testing.T) {
	s := NewSamplerFunc(time.Second, func() (float64, float64, float64, float64) { return 50, 60, 300, 1900 })
	if util, _, _, _ := s.Latest(time.Now()); util != 0 {
		t.Fatalf("util before first sample = %v, want 0", util)
	}
	s.Sample()
	if util, temp, power, clock := s.Latest(time.Now()); util != 50 || temp != 60 || power != 300 || clock != 1900 {
		t.Fatalf("got %v %v %v %v", util, temp, power, clock)
	}
	if util, _, _, _ := s.Latest(time.Now().Add(s.StaleAfter() + time.Second)); util != 0 {
		t.Fatalf("stale util = %v, want 0", util)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	s := NewSamplerFunc(time.Second, func() (float64, float64, float64, float64) { return 1, 1, 1, 1 })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
