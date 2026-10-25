package process

import (
	"testing"

	"github.com/awlx/vllm-dashboard/internal/sample"
)

func TestSpeculativeConfigFromDottedArgs(t *testing.T) {
	args := []string{
		"--speculative-config.model", "example/Model-DFlash",
		"--speculative-config.num_speculative_tokens", "3",
	}
	got := speculativeConfig(args)
	want := `{"method":"dflash","model":"example/Model-DFlash","num_speculative_tokens":3}`
	if got != want {
		t.Fatalf("speculative config = %s, want %s", got, want)
	}
}

func TestModelDisplayNameUsesRuntimeIdentity(t *testing.T) {
	info := sample.ServerInfo{ServedModelName: "nemotron-3.5-lightning-30b-a3b"}
	if got := DisplayName(info, "Secondary model"); got != info.ServedModelName {
		t.Fatalf("display name = %q, want %q", got, info.ServedModelName)
	}
}

func TestServesVLLMOnPort(t *testing.T) {
	for _, testcase := range []struct {
		args []string
		want bool
	}{
		{[]string{"/usr/bin/vllm", "serve", "org/model", "--port", "8888"}, true},
		{[]string{"/opt/venv/bin/python", "-m", "vllm.entrypoints.cli.main", "serve", "/models/tp1", "--port=8888"}, true},
		{[]string{"/usr/bin/vllm", "serve", "org/model", "--port", "88880"}, false},
		{[]string{"bash", "-lc", "vllm serve org/model --port 8888"}, false},
		{[]string{"/home/user/.local/bin/vllm-dashboard"}, false},
	} {
		if got := servesVLLMOnPort(testcase.args, "8888"); got != testcase.want {
			t.Errorf("servesVLLMOnPort(%q) = %v, want %v", testcase.args, got, testcase.want)
		}
	}
}
