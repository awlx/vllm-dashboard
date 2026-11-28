package gpu

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/sample"
)

func TestParseNvidiaSMI(t *testing.T) {
	tests := []struct {
		name string
		out  string
		agg  [4]float64 // util, temp, power, clock
		gpus []sample.GPUStat
	}{
		{
			"one GPU",
			"0, GPU-uuid-0, NVIDIA A100 80GB, 51200, 81920, 45, 61, 61, 250.5, 1980\n",
			[4]float64{45, 61, 250.5, 1980},
			[]sample.GPUStat{{Index: 0, UUID: "GPU-uuid-0", Name: "NVIDIA A100 80GB", VRAMUsedMB: 51200, VRAMTotalMB: 81920, UtilPct: 45, MemUtilPct: 61, TempC: 61, PowerW: 250.5, ClockMHz: 1980}},
		},
		{
			"two GPUs",
			"0, GPU-uuid-0, GPU One, 1000, 8000, 40, 20, 60, 200, 1800\n1, GPU-uuid-1, GPU Two, 2000, 8000, 60, 40, 70, 300, 2000\n",
			[4]float64{50, 65, 500, 1900},
			[]sample.GPUStat{
				{Index: 0, UUID: "GPU-uuid-0", Name: "GPU One", VRAMUsedMB: 1000, VRAMTotalMB: 8000, UtilPct: 40, MemUtilPct: 20, TempC: 60, PowerW: 200, ClockMHz: 1800},
				{Index: 1, UUID: "GPU-uuid-1", Name: "GPU Two", VRAMUsedMB: 2000, VRAMTotalMB: 8000, UtilPct: 60, MemUtilPct: 40, TempC: 70, PowerW: 300, ClockMHz: 2000},
			},
		},
		{
			"missing values",
			"0, GPU-uuid-0, GPU, [N/A], [N/A], 40, 60, [N/A], 300, 1800\n1, GPU-uuid-1, GPU, 2000, 8000, [N/A], 70, 300, [N/A], 1800\n",
			[4]float64{40, 300, 300, 1800},
			[]sample.GPUStat{
				{Index: 0, UUID: "GPU-uuid-0", Name: "GPU", UtilPct: 40, MemUtilPct: 60, PowerW: 300, ClockMHz: 1800},
				{Index: 1, UUID: "GPU-uuid-1", Name: "GPU", VRAMUsedMB: 2000, VRAMTotalMB: 8000, MemUtilPct: 70, TempC: 300, ClockMHz: 1800},
			},
		},
		{
			"no trailing newline",
			"0, GPU-uuid-0, GPU, 100, 1000, 10, 50, 100, 1000, 1000\n1, GPU-uuid-1, GPU, 200, 1000, 30, 50, 100, 1000, 1200",
			[4]float64{20, 100, 2000, 1100},
			[]sample.GPUStat{
				{Index: 0, UUID: "GPU-uuid-0", Name: "GPU", VRAMUsedMB: 100, VRAMTotalMB: 1000, UtilPct: 10, MemUtilPct: 50, TempC: 100, PowerW: 1000, ClockMHz: 1000},
				{Index: 1, UUID: "GPU-uuid-1", Name: "GPU", VRAMUsedMB: 200, VRAMTotalMB: 1000, UtilPct: 30, MemUtilPct: 50, TempC: 100, PowerW: 1000, ClockMHz: 1200},
			},
		},
		{"empty", "", [4]float64{0, 0, 0, 0}, nil},
		{"short line", "10, 20\n", [4]float64{0, 0, 0, 0}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reading := parseNvidiaSMI([]byte(test.out))
			agg := [4]float64{reading.Util, reading.Temp, reading.Power, reading.Clock}
			for i, name := range []string{"util", "temp", "power", "clock"} {
				if math.Abs(agg[i]-test.agg[i]) > 1e-9 {
					t.Errorf("%s = %v, want %v", name, agg[i], test.agg[i])
				}
			}
			if len(reading.GPUs) != len(test.gpus) {
				t.Fatalf("got %d GPUs, want %d: %+v", len(reading.GPUs), len(test.gpus), reading.GPUs)
			}
			for index, want := range test.gpus {
				if got := reading.GPUs[index]; got != want {
					t.Errorf("GPU %d = %+v, want %+v", index, got, want)
				}
			}
		})
	}
}

func TestParseComputeApps(t *testing.T) {
	reading := parseComputeApps([]byte("100, GPU-uuid-0, 48123\n200, GPU-uuid-1, 3200\n300, GPU-uuid-0, [N/A]\nbad line\n400, GPU-uuid-2, 100"))
	want := []ProcMem{
		{PID: 100, UUID: "GPU-uuid-0", MB: 48123},
		{PID: 200, UUID: "GPU-uuid-1", MB: 3200},
		{PID: 300, UUID: "GPU-uuid-0"},
		{PID: 400, UUID: "GPU-uuid-2", MB: 100},
	}
	if len(reading) != len(want) {
		t.Fatalf("got %d procs, want %d: %+v", len(reading), len(want), reading)
	}
	for i, proc := range want {
		if reading[i] != proc {
			t.Errorf("proc %d = %+v, want %+v", i, reading[i], proc)
		}
	}
	if parseComputeApps([]byte("")) != nil {
		t.Fatal("empty output should have no procs")
	}
}

func TestLatestReturnsZeroWhenStale(t *testing.T) {
	s := NewSamplerFunc(time.Second, func() Reading {
		return Reading{Util: 50, Temp: 60, Power: 300, Clock: 1900, GPUs: []sample.GPUStat{{Index: 0}}}
	})
	if r := s.Latest(time.Now()); r.Util != 0 {
		t.Fatalf("util before first sample = %v, want 0", r.Util)
	}
	s.Sample()
	r := s.Latest(time.Now())
	if r.Util != 50 || r.Temp != 60 || r.Power != 300 || r.Clock != 1900 {
		t.Fatalf("got %+v", r)
	}
	if len(r.GPUs) != 1 || r.GPUs[0].Index != 0 {
		t.Fatalf("gpus = %+v", r.GPUs)
	}
	if r := s.Latest(time.Now().Add(s.StaleAfter() + time.Second)); r.Util != 0 || len(r.GPUs) != 0 {
		t.Fatalf("stale reading = %+v, want empty", r)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	s := NewSamplerFunc(time.Second, func() Reading { return Reading{Util: 1, Temp: 1, Power: 1, Clock: 1} })
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
