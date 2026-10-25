package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePollInterval(t *testing.T) {
	for _, testcase := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", defaultPollInterval, true},
		{"1s", time.Second, true},
		{"500ms", 500 * time.Millisecond, true},
		{"10s", 10 * time.Second, true},
		{"499ms", 0, false},
		{"11s", 0, false},
		{"-1s", 0, false},
		{"2", 0, false},
		{"fast", 0, false},
	} {
		got, err := parsePollInterval(testcase.value)
		if (err == nil) != testcase.ok || got != testcase.want {
			t.Errorf("parsePollInterval(%q) = %v, %v; want %v ok=%v", testcase.value, got, err, testcase.want, testcase.ok)
		}
	}
}

func TestLoadConfigPollInterval(t *testing.T) {
	base := `"listen": ":9090", "node": {"key":"node-a","name":"A","models":[]}`
	cfg, err := loadConfig(writeTestConfig(t, `{`+base+`, "poll_interval": "1s"}`))
	if err != nil || cfg.PollInterval != "1s" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := loadConfig(writeTestConfig(t, `{`+base+`, "poll_interval": "100ms"}`)); err == nil {
		t.Fatal("accepted poll_interval below minimum")
	}
	configureRuntime(cfg)
	defer configureRuntime(defaultConfig())
	if pollInterval != time.Second || historySize() != 300 || onlineWindow() != 6*time.Second {
		t.Fatalf("interval=%v history=%d online=%v", pollInterval, historySize(), onlineWindow())
	}
}

func TestHistorySizeCoversWindow(t *testing.T) {
	defer func() { pollInterval = defaultPollInterval }()
	for interval, want := range map[time.Duration]int{
		2 * time.Second:        150,
		time.Second:            300,
		500 * time.Millisecond: 600,
		7 * time.Second:        43,
		10 * time.Second:       30,
	} {
		pollInterval = interval
		if got := historySize(); got != want {
			t.Errorf("historySize(%v) = %d, want %d", interval, got, want)
		}
	}
}

func TestPollModelTrimsHistoryToWindow(t *testing.T) {
	server := newMockVLLM(t)
	cfg := DashboardConfig{PollInterval: "10s", Node: NodeConfig{Key: "node-a", Name: "A", Models: []ModelConfig{{Key: "model-a", Name: "A", MetricsURL: server.URL + "/metrics"}}}}
	configureRuntime(cfg)
	defer configureRuntime(defaultConfig())
	for range 40 {
		pollModel(models[0], Sample{}, time.Now())
	}
	if got := len(modelStates["model-a"].History); got != 30 {
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
			sample := Sample{Time: now.UnixMilli(), GenTokPerSec: 100, GPUUtilPct: 50, GenTokensCum: float64(index), PrefixCacheHitPct: float64(index)}
			if index%2 == 1 {
				sample.GenTokPerSec = 300
				sample.TTFTMs = 40
			}
			appendLongSample(state, sample, now)
		}
		if len(state.LongHistory) != 11 {
			t.Fatalf("%v: %d long samples, want 11", interval, len(state.LongHistory))
		}
		for index, sample := range state.LongHistory[1:] {
			if got := time.UnixMilli(sample.Time).Sub(time.UnixMilli(state.LongHistory[index].Time)); got != longInterval {
				t.Fatalf("%v: long sample %d spacing %v", interval, index+1, got)
			}
			if sample.GenTokPerSec < 190 || sample.GenTokPerSec > 210 || sample.GPUUtilPct != 50 {
				t.Fatalf("%v: rollup not averaged: %+v", interval, sample)
			}
			if sample.TTFTMs != 40 {
				t.Fatalf("%v: idle polls diluted latency: %v", interval, sample.TTFTMs)
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
	appendLongSample(state, Sample{}, start)
	appendLongSample(state, Sample{}, start.Add(61*time.Second))
	if !state.LastLongSample.Equal(start.Add(longInterval)) {
		t.Fatalf("schedule not anchored: %v", state.LastLongSample)
	}
	gap := start.Add(10 * time.Minute)
	appendLongSample(state, Sample{}, gap)
	if !state.LastLongSample.Equal(gap) || len(state.LongHistory) != 3 {
		t.Fatalf("schedule not restarted after gap: %v, %d", state.LastLongSample, len(state.LongHistory))
	}
}

func TestSettingsRoute(t *testing.T) {
	cfg := defaultConfig()
	cfg.PollInterval = "1500ms"
	configureRuntime(cfg)
	defer configureRuntime(defaultConfig())
	recorder := httptest.NewRecorder()
	newServeMux().ServeHTTP(recorder, httptest.NewRequest("GET", "/api/settings", nil))
	var settings DashboardSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings != (DashboardSettings{PollIntervalMs: 1500, HistoryWindowSeconds: 300, LongIntervalSeconds: 60}) {
		t.Fatalf("settings %+v", settings)
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type %q", recorder.Header().Get("Content-Type"))
	}
}
