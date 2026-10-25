package process

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/awlx/vllm-dashboard/internal/config"
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

func TestFindVLLMPidUsesProcRoot(t *testing.T) {
	root := t.TempDir()
	for pid, args := range map[string]string{
		"40": "python\x00-m\x00vllm.entrypoints.openai.api_server\x00--model\x00org/m\x00--port\x008001\x00",
		"30": "vllm\x00serve\x00org/m\x00",
		"20": "bash\x00-lc\x00vllm serve org/m --port 8001\x00",
	} {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "cmdline"), []byte(args), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	original := procRoot
	procRoot = root
	defer func() { procRoot = original }()
	for port, want := range map[string]string{"8001": "40", "8000": "30", "8002": ""} {
		if got, _ := findVLLMPid(port); got != want {
			t.Errorf("findVLLMPid(%s) = %q, want %q", port, got, want)
		}
	}
}

func TestSystemdPropertyDetectsUnitScope(t *testing.T) {
	units := map[string]string{"system": "vllm-a.service", "user": "vllm-b.service"}
	var calls []string
	original := systemctl
	systemctl = func(args ...string) ([]byte, error) {
		scope, unit := "system", args[1]
		if args[0] == "--user" {
			scope, unit = "user", args[2]
		}
		calls = append(calls, scope+":"+unit)
		if units[scope] == unit {
			return []byte("LoadState=loaded\nNRestarts=" + scope + "\n"), nil
		}
		return []byte("LoadState=not-found\nNRestarts=0\n"), nil
	}
	defer func() { systemctl = original }()

	for _, testcase := range []struct {
		model config.Model
		want  string
		calls int
	}{
		{config.Model{Unit: "vllm-a.service"}, "system", 1},
		{config.Model{Unit: "vllm-b.service"}, "user", 2},
		{config.Model{Unit: "vllm-b.service", UserUnit: true}, "user", 1},
		{config.Model{Unit: "missing.service"}, "", 2},
		{config.Model{}, "", 0},
	} {
		calls = nil
		if got := systemdProperty(testcase.model, "NRestarts"); got != testcase.want || len(calls) != testcase.calls {
			t.Errorf("systemdProperty(%+v) = %q after %v, want %q after %d calls", testcase.model, got, calls, testcase.want, testcase.calls)
		}
	}
}
