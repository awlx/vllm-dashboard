package config

import (
	"testing"
	"time"
)

func TestParsePollInterval(t *testing.T) {
	for _, testcase := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", DefaultPollInterval, true},
		{"1s", time.Second, true},
		{"500ms", 500 * time.Millisecond, true},
		{"10s", 10 * time.Second, true},
		{"499ms", 0, false},
		{"11s", 0, false},
		{"-1s", 0, false},
		{"2", 0, false},
		{"fast", 0, false},
	} {
		got, err := ParsePollInterval(testcase.value)
		if (err == nil) != testcase.ok || got != testcase.want {
			t.Errorf("ParsePollInterval(%q) = %v, %v; want %v ok=%v", testcase.value, got, err, testcase.want, testcase.ok)
		}
	}
}

func TestLoadConfigPollInterval(t *testing.T) {
	base := `"listen": ":9090", "node": {"key":"node-a","name":"A","models":[]}`
	cfg, err := Load(writeTestConfig(t, `{`+base+`, "poll_interval": "2s"}`))
	if err != nil || cfg.PollInterval != "2s" || cfg.Interval() != 2*time.Second {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := Load(writeTestConfig(t, `{`+base+`, "poll_interval": "100ms"}`)); err == nil {
		t.Fatal("accepted poll_interval below minimum")
	}
}

func TestDefaultPollInterval(t *testing.T) {
	if got := Default().Interval(); got != time.Second {
		t.Fatalf("default interval = %v, want 1s", got)
	}
}
