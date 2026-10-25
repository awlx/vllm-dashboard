package store

import (
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
)

func TestHistorySizeCoversWindow(t *testing.T) {
	for interval, want := range map[string]int{
		"2s":    150,
		"1s":    300,
		"500ms": 600,
		"7s":    43,
		"10s":   30,
	} {
		st := New(config.Config{PollInterval: interval})
		if got := st.HistorySize(); got != want {
			t.Errorf("HistorySize(%v) = %d, want %d", interval, got, want)
		}
	}
}

func TestDefaultIntervalWindows(t *testing.T) {
	st := New(config.Default())
	if st.Interval() != time.Second || st.HistorySize() != 300 || st.OnlineWindow() != 6*time.Second {
		t.Fatalf("interval=%v history=%d online=%v", st.Interval(), st.HistorySize(), st.OnlineWindow())
	}
	st = New(config.Config{PollInterval: "2s"})
	if st.HistorySize() != 150 || st.OnlineWindow() != 6*time.Second {
		t.Fatalf("2s: history=%d online=%v", st.HistorySize(), st.OnlineWindow())
	}
}

func TestAppendTrimsHistoryToWindow(t *testing.T) {
	st := New(config.Config{PollInterval: "10s"})
	state := &ModelState{}
	now := time.Now()
	for index := range 40 {
		st.Append(state, sample.Sample{}, now.Add(time.Duration(index)*10*time.Second))
	}
	if got := len(state.History); got != 30 {
		t.Fatalf("history length %d, want 30", got)
	}
}

func TestLongHistoryAveragesPollsPerMinute(t *testing.T) {
	for _, interval := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 10 * time.Second} {
		state := &ModelState{}
		start := time.Unix(1_700_000_000, 0)
		polls := int(10*time.Minute/interval) + 1
		for index := range polls {
			now := start.Add(time.Duration(index) * interval)
			s := sample.Sample{Time: now.UnixMilli(), GenTokPerSec: 100, GPUUtilPct: 50, GenTokensCum: float64(index), PrefixCacheHitPct: float64(index)}
			if index%2 == 1 {
				s.GenTokPerSec = 300
				s.TTFTMs = 40
			}
			AppendLongSample(state, s, now)
		}
		if len(state.LongHistory) != 11 {
			t.Fatalf("%v: %d long samples, want 11", interval, len(state.LongHistory))
		}
		for index, s := range state.LongHistory[1:] {
			if got := time.UnixMilli(s.Time).Sub(time.UnixMilli(state.LongHistory[index].Time)); got != LongInterval {
				t.Fatalf("%v: long sample %d spacing %v", interval, index+1, got)
			}
			if s.GenTokPerSec < 190 || s.GenTokPerSec > 210 || s.GPUUtilPct != 50 {
				t.Fatalf("%v: rollup not averaged: %+v", interval, s)
			}
			if s.TTFTMs != 40 {
				t.Fatalf("%v: idle polls diluted latency: %v", interval, s.TTFTMs)
			}
		}
		last := state.LongHistory[len(state.LongHistory)-1]
		if last.GenTokensCum != float64(polls-1) || last.PrefixCacheHitPct != float64(polls-1) {
			t.Fatalf("%v: counters must use the latest sample: %+v", interval, last)
		}
	}
}

func TestLongHistoryRestartsScheduleAfterGap(t *testing.T) {
	state := &ModelState{}
	start := time.Unix(1_700_000_000, 0)
	AppendLongSample(state, sample.Sample{}, start)
	AppendLongSample(state, sample.Sample{}, start.Add(61*time.Second))
	if !state.LastLongSample.Equal(start.Add(LongInterval)) {
		t.Fatalf("schedule not anchored: %v", state.LastLongSample)
	}
	gap := start.Add(10 * time.Minute)
	AppendLongSample(state, sample.Sample{}, gap)
	if !state.LastLongSample.Equal(gap) || len(state.LongHistory) != 3 {
		t.Fatalf("schedule not restarted after gap: %v, %d", state.LastLongSample, len(state.LongHistory))
	}
}
