package discovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
)

func found(port, served string) config.Model {
	return Server{Host: "127.0.0.1", Port: port, ServedName: served}.ModelConfig()
}

func keys(models []config.Model) []string {
	var result []string
	for _, model := range models {
		result = append(result, model.Key)
	}
	return result
}

func TestTrackerAddsKeepsAndRemovesModels(t *testing.T) {
	explicit := complete(t, config.Model{Key: "fixed", Name: "Fixed", Port: "9000"})
	tracker := NewTracker(time.Minute)
	start := time.Unix(1000, 0)

	models, removed := tracker.Update([]config.Model{explicit}, []config.Model{found("8000", "chat"), found("8001", "coder")}, start)
	if got := keys(models); len(got) != 3 || got[0] != "fixed" || got[1] != "chat" || got[2] != "coder" || len(removed) != 0 {
		t.Fatalf("first update = %v, removed %v", got, keys(removed))
	}

	// coder stops. It stays during the grace period.
	models, removed = tracker.Update(models, []config.Model{found("8000", "chat")}, start.Add(30*time.Second))
	if got := keys(models); len(got) != 3 || len(removed) != 0 {
		t.Fatalf("update in grace period = %v, removed %v", got, keys(removed))
	}

	// After the grace period, coder is removed. The configured model stays
	// although no process serves it.
	models, removed = tracker.Update(models, []config.Model{found("8000", "chat")}, start.Add(2*time.Minute))
	if got := keys(models); len(got) != 2 || got[0] != "fixed" || got[1] != "chat" || len(removed) != 1 || removed[0].Key != "coder" {
		t.Fatalf("update after grace period = %v, removed %v", got, keys(removed))
	}

	// coder starts again and is added again.
	models, _ = tracker.Update(models, []config.Model{found("8000", "chat"), found("8001", "coder")}, start.Add(3*time.Minute))
	if got := keys(models); len(got) != 3 || got[2] != "coder" {
		t.Fatalf("update after restart = %v", got)
	}

	// A different model on port 8000 replaces chat at once.
	models, removed = tracker.Update(models, []config.Model{found("8000", "reasoner"), found("8001", "coder")}, start.Add(4*time.Minute))
	if got := keys(models); len(got) != 3 || got[1] != "coder" || got[2] != "reasoner" || len(removed) != 1 || removed[0].Key != "chat" {
		t.Fatalf("update after replacement = %v, removed %v", got, keys(removed))
	}
}

func TestScanPrefersParentProcess(t *testing.T) {
	root := t.TempDir()
	// PIDs wrapped: the forked worker has a lower PID than its parent.
	writeProc(t, root, 900, []string{"vllm", "serve", "org/a", "--port", "8000"}, "")
	writeProc(t, root, 15, []string{"vllm", "serve", "org/a", "--port", "8000"}, "")
	for pid, status := range map[string]string{"900": "Name:\tvllm\nPPid:\t1\n", "15": "Name:\tvllm\nPPid:\t900\n"} {
		if err := os.WriteFile(filepath.Join(root, pid, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	servers := Scan(root)
	if len(servers) != 1 || servers[0].PID != 900 {
		t.Fatalf("Scan = %+v, want PID 900", servers)
	}
}

func TestFlagValueAcceptsUnderscores(t *testing.T) {
	args := []string{"vllm", "serve", "m", "--served_model_name", "chat", "--port=8001"}
	if got := FlagValue(args, "--served-model-name"); got != "chat" {
		t.Errorf("FlagValue = %q, want chat", got)
	}
	if got := FlagValue(args, "--port"); got != "8001" {
		t.Errorf("FlagValue = %q, want 8001", got)
	}
}
