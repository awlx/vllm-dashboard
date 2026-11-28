package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/awlx/vllm-dashboard/internal/config"
)

// writeProc writes the cmdline and status of one process to the fake /proc
// root. cmdline is NUL-separated, like the real /proc.
func writeProc(t *testing.T, root string, pid int, cmdline []string, ppid int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(cmdline, "\x00")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(fmt.Sprintf("Name:\ttest\nPPid:\t%d\n", ppid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAttribute(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 1, []string{"/sbin/init"}, 0)
	server := []string{"vllm", "serve", "Qwen/Qwen3-4B-Instruct", "--port", "8000"}
	writeProc(t, root, 100, server, 1)
	// A direct engine worker of the server.
	writeProc(t, root, 200, []string{"python3", "-c", "spawn_main"}, 100)
	// A worker of a worker.
	writeProc(t, root, 600, []string{"python3", "-c", "spawn_main"}, 100)
	writeProc(t, root, 500, []string{"python3", "-c", "spawn_main"}, 600)
	// An unrelated process.
	writeProc(t, root, 300, []string{"firefox"}, 1)
	// A server with a custom served name.
	writeProc(t, root, 700, []string{"vllm", "serve", "--model", "org/big-model", "--served-model-name", "big"}, 1)

	models := []config.Model{
		{Key: "qwen3-4b-instruct", Name: "Qwen/Qwen3-4B-Instruct"},
		{Key: "big", Name: "big"},
	}
	got := AttributeAt(root, models, []int{100, 200, 300, 400, 500, 700})
	want := map[int]string{
		100: "qwen3-4b-instruct",
		200: "qwen3-4b-instruct",
		300: OtherModel,
		400: OtherModel,
		500: "qwen3-4b-instruct",
		700: "big",
	}
	for pid, key := range want {
		if got[pid] != key {
			t.Errorf("pid %d = %q, want %q", pid, got[pid], key)
		}
	}
}

func TestAttributeUnknownServer(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 1, []string{"/sbin/init"}, 0)
	writeProc(t, root, 100, []string{"vllm", "serve", "org/unknown-model", "--port", "8000"}, 1)
	got := AttributeAt(root, nil, []int{100})
	if got[100] != OtherModel {
		t.Fatalf("server of an unknown model = %q, want %q", got[100], OtherModel)
	}
}
