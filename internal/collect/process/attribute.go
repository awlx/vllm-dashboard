package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/discovery"
)

// OtherModel is the pseudo-model that GPU memory belongs to when no
// configured model owns the process.
const OtherModel = "other"

// attributeAncestorDepth is how far up the parent chain Attribute looks for
// a server command line. vLLM engine workers are direct or indirect children
// of the API server process.
const attributeAncestorDepth = 5

// Attribute maps each PID to the key of the configured model that owns it,
// or to OtherModel. A PID belongs to a model when its own command line, or
// the command line of one of its ancestors, starts a vLLM or TensorFold
// server for a known model. PIDs whose process is not visible are mapped to
// OtherModel as well.
func Attribute(models []config.Model, pids []int) map[int]string {
	return AttributeAt(procRoot, models, pids)
}

// AttributeAt is Attribute with an explicit /proc root, for tests.
func AttributeAt(root string, models []config.Model, pids []int) map[int]string {
	known := map[string]string{}
	for _, model := range models {
		if model.Key == "" {
			continue
		}
		if model.Name != "" {
			known[model.Name] = model.Key
		}
		known[model.Key] = model.Key
	}
	result := make(map[int]string, len(pids))
	for _, pid := range pids {
		if key := serverModelKey(root, pid, known); key != "" {
			result[pid] = key
		} else {
			result[pid] = OtherModel
		}
	}
	return result
}

// serverModelKey returns the model key of the server that owns pid. It
// inspects the command line of pid and walks up to
// attributeAncestorDepth parents, so that engine workers are attributed to
// their API server.
func serverModelKey(root string, pid int, known map[string]string) string {
	for depth := 0; depth < attributeAncestorDepth; depth++ {
		args, err := readCmdlineAt(root, pid)
		if err != nil {
			return ""
		}
		server, isServer := discovery.ParseCmdline(args)
		if isServer {
			// A server that matches no configured model is not owned by any
			// model; its parents cannot be servers either.
			return matchKnown(server, known)
		}
		parent := parentPIDAt(root, pid)
		if parent <= 0 || parent == pid {
			return ""
		}
		pid = parent
	}
	return ""
}

// matchKnown maps the model or served name of a server to a model key.
func matchKnown(server discovery.Server, known map[string]string) string {
	for _, name := range []string{server.ServedName, server.Model} {
		if name == "" {
			continue
		}
		if key, ok := known[name]; ok {
			return key
		}
	}
	return ""
}

func readCmdlineAt(root string, pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return discovery.SplitCmdline(data), nil
}

func parentPIDAt(root string, pid int) int {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, found := strings.CutPrefix(line, "PPid:"); found {
			parent, _ := strconv.Atoi(strings.TrimSpace(value))
			return parent
		}
	}
	return 0
}
