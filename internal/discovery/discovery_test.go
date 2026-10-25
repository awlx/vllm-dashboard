package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/awlx/vllm-dashboard/internal/config"
)

func TestParseCmdline(t *testing.T) {
	for _, testcase := range []struct {
		name string
		args []string
		want Server
		ok   bool
	}{
		{"vllm serve defaults", []string{"/opt/venv/bin/vllm", "serve", "Qwen/Qwen3-8B"},
			Server{Host: "127.0.0.1", Port: "8000", Model: "Qwen/Qwen3-8B"}, true},
		{"flags and equals form", []string{"vllm", "serve", "/models/tp1", "--port=8001", "--host", "0.0.0.0", "--served-model-name", "chat"},
			Server{Host: "127.0.0.1", Port: "8001", Model: "/models/tp1", ServedName: "chat"}, true},
		{"api server module", []string{"python3", "-m", "vllm.entrypoints.openai.api_server", "--model", "org/m", "--port", "8002", "--host", "192.0.2.5"},
			Server{Host: "192.0.2.5", Port: "8002", Model: "org/m"}, true},
		{"cli module serve", []string{"python", "-m", "vllm.entrypoints.cli.main", "serve", "org/m", "--port", "8003"},
			Server{Host: "127.0.0.1", Port: "8003", Model: "org/m"}, true},
		{"tensorfold", []string{"/usr/local/bin/tensorfold", "serve", "org/m", "--port", "8004"},
			Server{Host: "127.0.0.1", Port: "8004", Model: "org/m"}, true},
		{"ipv6 any", []string{"vllm", "serve", "m", "--host", "::"},
			Server{Host: "127.0.0.1", Port: "8000", Model: "m"}, true},
		{"shell wrapper", []string{"bash", "-lc", "vllm serve org/m --port 8000"}, Server{}, false},
		{"engine core child", []string{"VLLM::EngineCore"}, Server{}, false},
		{"dashboard", []string{"/usr/local/bin/vllm-dashboard"}, Server{}, false},
		{"vllm without serve", []string{"vllm", "--version"}, Server{}, false},
		{"bad port", []string{"vllm", "serve", "m", "--port", "88888"}, Server{}, false},
	} {
		got, ok := ParseCmdline(testcase.args)
		if ok != testcase.ok || got != testcase.want {
			t.Errorf("%s: ParseCmdline = %+v, %v; want %+v, %v", testcase.name, got, ok, testcase.want, testcase.ok)
		}
	}
}

func TestParseCgroup(t *testing.T) {
	for _, testcase := range []struct {
		data string
		unit string
		user bool
	}{
		{"0::/system.slice/vllm-chat.service\n", "vllm-chat.service", false},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/vllm-coder.service\n", "vllm-coder.service", true},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-term.scope\n", "", false},
		{"0::/user.slice/user-1000.slice/session-3.scope\n", "", false},
		{"12:cpu,cpuacct:/system.slice/other.service\n1:name=systemd:/system.slice/vllm-a.service\n", "vllm-a.service", false},
		{"0::/system.slice/docker-0123abcd.scope\n", "", false},
		{"", "", false},
	} {
		unit, user := ParseCgroup([]byte(testcase.data))
		if unit != testcase.unit || user != testcase.user {
			t.Errorf("ParseCgroup(%q) = %q, %v; want %q, %v", testcase.data, unit, user, testcase.unit, testcase.user)
		}
	}
}

func writeProc(t *testing.T, root string, pid int, args []string, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cgroup != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanKeepsLowestPIDPerPort(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 300, []string{"vllm", "serve", "org/a", "--port", "8000"}, "0::/system.slice/vllm-a.service\n")
	writeProc(t, root, 200, []string{"vllm", "serve", "org/a", "--port", "8000"}, "0::/system.slice/vllm-a.service\n")
	writeProc(t, root, 100, []string{"vllm", "serve", "org/b", "--port", "8001"}, "")
	writeProc(t, root, 50, []string{"bash"}, "")
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Scan(root)
	want := []Server{
		{PID: 200, Host: "127.0.0.1", Port: "8000", Model: "org/a", Unit: "vllm-a.service"},
		{PID: 100, Host: "127.0.0.1", Port: "8001", Model: "org/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan = %+v, want %+v", got, want)
	}
}

func TestModelConfig(t *testing.T) {
	for _, testcase := range []struct {
		server Server
		key    string
		name   string
	}{
		{Server{Host: "127.0.0.1", Port: "8000", ServedName: "Qwen/Qwen3-8B", Model: "/models/x"}, "qwen3-8b", "Qwen/Qwen3-8B"},
		{Server{Host: "127.0.0.1", Port: "8000", Model: "/models/Llama-3.1-8B/"}, "llama-3-1-8b", "Llama-3.1-8B"},
		{Server{Host: "127.0.0.1", Port: "8005"}, "port-8005", "port-8005"},
	} {
		model := testcase.server.ModelConfig()
		if model.Key != testcase.key || model.Name != testcase.name || model.Port != testcase.server.Port ||
			model.MetricsURL != "http://127.0.0.1:"+testcase.server.Port+"/metrics" {
			t.Errorf("ModelConfig(%+v) = %+v", testcase.server, model)
		}
	}
}

func TestProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vllm":
			fmt.Fprintln(w, "vllm:num_requests_running 0")
		case "/tensorfold":
			fmt.Fprintln(w, "tensorfold:requests_running 0")
		case "/other":
			fmt.Fprintln(w, "go_goroutines 5")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for path, want := range map[string]bool{"/vllm": true, "/tensorfold": true, "/other": false, "/missing": false} {
		if got := Probe(context.Background(), server.URL+path); got != want {
			t.Errorf("Probe(%s) = %v, want %v", path, got, want)
		}
	}
}

func complete(t *testing.T, model config.Model) config.Model {
	t.Helper()
	if err := config.CompleteModel(&model); err != nil {
		t.Fatal(err)
	}
	return model
}

func TestMerge(t *testing.T) {
	explicit := complete(t, config.Model{Key: "chat", Name: "Chat", Port: "8000"})
	auto := complete(t, config.Model{Port: "8001"})
	proxied := complete(t, config.Model{Key: "proxied", Name: "Proxied", MetricsURL: "https://proxy.example.net/m/metrics", Port: "8002"})
	remote := complete(t, config.Model{Key: "remote", Name: "Remote", MetricsURL: "http://192.0.2.5:8003/metrics"})
	discovered := []config.Model{
		{Key: "qwen", Name: "Qwen", MetricsURL: "http://127.0.0.1:8000/metrics", Port: "8000", Unit: "vllm-chat.service"},
		{Key: "coder", Name: "org/coder", MetricsURL: "http://127.0.0.1:8001/metrics", Port: "8001", Unit: "vllm-coder.service", UserUnit: true},
		{Key: "x", Name: "x", MetricsURL: "http://127.0.0.1:8002/metrics", Port: "8002"},
		{Key: "chat", Name: "other", MetricsURL: "http://127.0.0.1:8003/metrics", Port: "8003"},
	}
	got := Merge([]config.Model{explicit, auto, proxied, remote}, discovered)
	if len(got) != 5 {
		t.Fatalf("Merge returned %d models: %+v", len(got), got)
	}
	if got[0].Key != "chat" || got[0].Name != "Chat" || got[0].Unit != "vllm-chat.service" {
		t.Errorf("explicit model = %+v", got[0])
	}
	if got[1].Key != "coder" || got[1].Name != "org/coder" || got[1].Unit != "vllm-coder.service" || !got[1].UserUnit || got[1].KeyAuto || got[1].NameAuto {
		t.Errorf("automatic model = %+v", got[1])
	}
	if got[2].Key != "proxied" || got[2].MetricsURL != "https://proxy.example.net/m/metrics" {
		t.Errorf("proxied model = %+v", got[2])
	}
	if got[3].Key != "remote" {
		t.Errorf("remote model = %+v", got[3])
	}
	if got[4].Key != "chat-8003" || got[4].Port != "8003" {
		t.Errorf("new model with taken key = %+v", got[4])
	}
	again := Merge(got, discovered)
	if !reflect.DeepEqual(again, got) {
		t.Errorf("second Merge changed the models: %+v", again)
	}
}

func TestNameModels(t *testing.T) {
	models := []config.Model{
		{Key: "port-8000", Name: "port-8000", MetricsURL: "http://127.0.0.1:8000/metrics", KeyAuto: true, NameAuto: true},
		{Key: "fixed", Name: "Fixed", MetricsURL: "http://127.0.0.1:8001/metrics"},
		{Key: "port-8002", Name: "port-8002", MetricsURL: "http://127.0.0.1:8002/metrics", KeyAuto: true, NameAuto: true},
		{Key: "port-8003", Name: "port-8003", MetricsURL: "http://127.0.0.1:8003/metrics", KeyAuto: true, NameAuto: true},
	}
	names := map[string]string{
		"http://127.0.0.1:8000/metrics": "Qwen/Qwen3-8B",
		"http://127.0.0.1:8001/metrics": "not-used",
		"http://127.0.0.1:8002/metrics": "fixed",
	}
	got := NameModels(models, func(url string) string { return names[url] })
	want := []struct{ key, name string }{{"qwen3-8b", "Qwen/Qwen3-8B"}, {"fixed", "Fixed"}, {"port-8002", "fixed"}, {"port-8003", "port-8003"}}
	for index, w := range want {
		if got[index].Key != w.key || got[index].Name != w.name {
			t.Errorf("model %d = %+v, want %+v", index, got[index], w)
		}
	}
	if !got[3].KeyAuto || got[0].KeyAuto {
		t.Errorf("automatic flags = %v, %v", got[0].KeyAuto, got[3].KeyAuto)
	}
}
