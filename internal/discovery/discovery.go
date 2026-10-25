// Package discovery finds local vLLM and TensorFold servers in /proc and
// merges them with the configured models.
package discovery

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
)

// DefaultPort is the vLLM API port when the command line has no --port.
const DefaultPort = "8000"

// Server is one server process that was found from its command line.
type Server struct {
	PID        int
	Host       string
	Port       string
	ServedName string
	Model      string
	Unit       string
	UserUnit   bool
}

// ParseCmdline returns the server that args start. The second value is
// false if args do not start a vLLM or TensorFold API server.
func ParseCmdline(args []string) (Server, bool) {
	if !IsServer(args) {
		return Server{}, false
	}
	server := Server{
		Host:       FlagValue(args, "--host"),
		Port:       FlagValue(args, "--port"),
		ServedName: FlagValue(args, "--served-model-name"),
		Model:      FlagValue(args, "--model"),
	}
	if server.Model == "" {
		server.Model = positionalModel(args)
	}
	if server.Port == "" {
		server.Port = DefaultPort
	}
	if port, err := strconv.Atoi(server.Port); err != nil || port < 1 || port > 65535 {
		return Server{}, false
	}
	switch server.Host {
	case "", "0.0.0.0", "::", "[::]":
		server.Host = "127.0.0.1"
	}
	return server, true
}

func positionalModel(args []string) string {
	for index, arg := range args {
		if arg == "serve" && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
			return args[index+1]
		}
	}
	return ""
}

// IsServer accepts `vllm serve`, `python -m vllm.entrypoints.openai.api_server`,
// `tensorfold serve`, and `python -m tensorfold... server`. A shell wrapper
// has the whole command in one argument and does not match.
func IsServer(args []string) bool {
	serve := false
	for _, arg := range args {
		if arg == "serve" {
			serve = true
		}
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			continue
		}
		base := filepath.Base(arg)
		switch {
		case strings.HasPrefix(arg, "vllm.entrypoints.") && strings.Contains(arg, "api_server"):
			return true
		case strings.HasPrefix(arg, "vllm.entrypoints") && serve:
			return true
		case base == "vllm" && serve:
			return true
		case strings.HasPrefix(base, "tensorfold") && (serve || strings.Contains(base, "server")):
			return true
		}
	}
	return false
}

// FlagValue reads "--flag value" and "--flag=value". It also accepts the
// underscore form of the flag name, for example --served_model_name.
func FlagValue(args []string, flag string) string {
	underscore := "--" + strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_")
	for index, arg := range args {
		for _, name := range []string{flag, underscore} {
			if arg == name && index+1 < len(args) {
				return args[index+1]
			}
			if value, found := strings.CutPrefix(arg, name+"="); found {
				return value
			}
		}
	}
	return ""
}

// ParseCgroup returns the systemd service of a /proc/<pid>/cgroup file. It
// reads the cgroup v2 line and the v1 name=systemd line. user is true for a
// service of a user manager.
func ParseCgroup(data []byte) (unit string, user bool) {
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || (parts[1] != "" && parts[1] != "name=systemd") {
			continue
		}
		elements := strings.Split(parts[2], "/")
		for index := len(elements) - 1; index >= 0; index-- {
			if strings.HasSuffix(elements[index], ".service") && !strings.HasPrefix(elements[index], "user@") {
				return elements[index], strings.Contains(parts[2], "/user@")
			}
		}
	}
	return "", false
}

// Scan reads all processes under procRoot and returns one server per port.
// For a port with more than one process, for example a server and its
// forked workers, Scan keeps the process whose parent is not in the group.
// If that does not select one process, the lowest PID wins.
func Scan(procRoot string) []Server {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	byPort := map[string][]Server{}
	parents := map[int]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		server, ok := ParseCmdline(SplitCmdline(data))
		if !ok {
			continue
		}
		server.PID = pid
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			parents[pid] = parentPID(status)
		}
		if cgroup, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			server.Unit, server.UserUnit = ParseCgroup(cgroup)
		}
		byPort[server.Port] = append(byPort[server.Port], server)
	}
	servers := make([]Server, 0, len(byPort))
	for _, group := range byPort {
		servers = append(servers, rootServer(group, parents))
	}
	sort.Slice(servers, func(i, j int) bool {
		a, _ := strconv.Atoi(servers[i].Port)
		b, _ := strconv.Atoi(servers[j].Port)
		return a < b
	})
	return servers
}

func rootServer(group []Server, parents map[int]int) Server {
	sort.Slice(group, func(i, j int) bool { return group[i].PID < group[j].PID })
	inGroup := map[int]bool{}
	for _, server := range group {
		inGroup[server.PID] = true
	}
	for _, server := range group {
		if !inGroup[parents[server.PID]] {
			return server
		}
	}
	return group[0]
}

func parentPID(status []byte) int {
	for _, line := range strings.Split(string(status), "\n") {
		if value, found := strings.CutPrefix(line, "PPid:"); found {
			pid, _ := strconv.Atoi(strings.TrimSpace(value))
			return pid
		}
	}
	return 0
}

// SplitCmdline splits the NUL-separated content of /proc/<pid>/cmdline.
func SplitCmdline(data []byte) []string {
	var args []string
	for _, part := range strings.Split(string(data), "\x00") {
		if part != "" {
			args = append(args, part)
		}
	}
	return args
}

// MetricsURL returns the /metrics URL of the server.
func (s Server) MetricsURL() string {
	return "http://" + net.JoinHostPort(s.Host, s.Port) + "/metrics"
}

// ModelConfig returns the model configuration for the server. The key comes
// from the served model name, then the model path, then the port.
func (s Server) ModelConfig() config.Model {
	name := s.ServedName
	if name == "" && s.Model != "" {
		name = filepath.Base(strings.TrimRight(s.Model, "/"))
	}
	model := config.Model{
		Key:        config.SanitizeKey(filepath.Base(name)),
		Name:       name,
		MetricsURL: s.MetricsURL(),
		Port:       s.Port,
		Unit:       s.Unit,
		UserUnit:   s.UserUnit,
	}
	if model.Key == "" {
		model.Key = "port-" + s.Port
	}
	if model.Name == "" {
		model.Name = model.Key
	}
	model.Discovered = true
	model.Identity = s.ServedName + "\x00" + s.Model
	// Without --served-model-name, the served name from /v1/models is better.
	model.KeyAuto = s.ServedName == ""
	model.NameAuto = s.ServedName == ""
	return model
}

// NameModels sets the key and the name of each model with an automatic key
// or name to the served model name. servedName returns that name for a
// metrics URL, or "" if it is not known. A key that is in use does not change.
func NameModels(models []config.Model, servedName func(metricsURL string) string) []config.Model {
	result := append([]config.Model(nil), models...)
	used := map[string]bool{}
	for _, model := range result {
		used[model.Key] = true
	}
	for index := range result {
		model := &result[index]
		if !model.KeyAuto && !model.NameAuto {
			continue
		}
		name := servedName(model.MetricsURL)
		if name == "" {
			continue
		}
		if key := config.SanitizeKey(filepath.Base(name)); model.KeyAuto && key != "" && !used[key] {
			delete(used, model.Key)
			model.Key = key
			used[key] = true
		}
		if model.NameAuto {
			model.Name = name
		}
		model.KeyAuto, model.NameAuto = false, false
	}
	return result
}

var probeClient = &http.Client{Timeout: 2 * time.Second}

// Probe reports whether url serves vLLM or TensorFold Prometheus metrics.
func Probe(ctx context.Context, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	response, err := probeClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return false
	}
	return bytes.Contains(body, []byte("vllm:")) || bytes.Contains(body, []byte("tensorfold:"))
}

// Discover scans procRoot and returns a model for each server whose
// /metrics endpoint answers.
func Discover(ctx context.Context, procRoot string) []config.Model {
	var models []config.Model
	for _, server := range Scan(procRoot) {
		if Probe(ctx, server.MetricsURL()) {
			models = append(models, server.ModelConfig())
		}
	}
	return models
}

// Merge adds the discovered models to the known models. A known model wins
// over a discovered model on the same server. The known model takes the
// unit and any automatic key or name from the discovered model. A new
// discovered model gets a unique key. Merge never removes a model.
func Merge(known, discovered []config.Model) []config.Model {
	result := append([]config.Model(nil), known...)
	used := map[string]bool{}
	for _, model := range result {
		used[model.Key] = true
	}
	for _, found := range discovered {
		index := matching(result, found)
		if index < 0 {
			found.Key = uniqueKey(found.Key, found.Port, used)
			used[found.Key] = true
			result = append(result, found)
			continue
		}
		model := &result[index]
		if model.Unit == "" {
			model.Unit, model.UserUnit = found.Unit, found.UserUnit
		}
		if model.KeyAuto {
			if found.Key != model.Key && !used[found.Key] {
				delete(used, model.Key)
				model.Key = found.Key
				used[model.Key] = true
			}
			model.KeyAuto = found.KeyAuto
		}
		if model.NameAuto {
			model.Name, model.NameAuto = found.Name, found.NameAuto
		}
	}
	return result
}

// matching finds the known model of a discovered server. The URLs are the
// same, or the port is the same and the known port is explicit or local.
func matching(models []config.Model, found config.Model) int {
	for index, model := range models {
		if model.MetricsURL == found.MetricsURL {
			return index
		}
		if model.Port == found.Port && (!model.PortAuto || isLocalURL(model.MetricsURL)) {
			return index
		}
	}
	return -1
}

func isLocalURL(raw string) bool {
	host := raw
	if _, rest, found := strings.Cut(raw, "://"); found {
		host, _, _ = strings.Cut(rest, "/")
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func uniqueKey(key, port string, used map[string]bool) string {
	if !used[key] {
		return key
	}
	candidate := config.SanitizeKey(key + "-" + port)
	for suffix := 2; used[candidate]; suffix++ {
		candidate = config.SanitizeKey(key + "-" + port + "-" + strconv.Itoa(suffix))
	}
	return candidate
}
