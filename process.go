package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// servesVLLMOnPort matches `vllm serve` and `python -m vllm.entrypoints... serve`
// argv exactly, so shell wrappers and neighbouring ports do not match.
func servesVLLMOnPort(args []string, port string) bool {
	isVLLM, isServe, onPort := false, false, false
	for index, arg := range args {
		if filepath.Base(arg) == "vllm" || strings.HasPrefix(arg, "vllm.entrypoints") {
			isVLLM = true
		}
		if arg == "serve" {
			isServe = true
		}
		if arg == "--port="+port || (arg == "--port" && index+1 < len(args) && args[index+1] == port) {
			onPort = true
		}
	}
	return isVLLM && isServe && onPort
}

// findVLLMPid locates the running vLLM API server process, including containers.
func findVLLMPid(port string) (string, error) {
	if port == "" {
		return "", os.ErrNotExist
	}
	out, err := exec.Command("pgrep", "-f", "vllm").Output()
	if err != nil {
		return "", err
	}
	for _, pid := range strings.Fields(string(out)) {
		if args, err := readCmdlineArgs(pid); err == nil && servesVLLMOnPort(args, port) {
			return pid, nil
		}
	}
	return "", os.ErrNotExist
}

func modelProcessStartedAt(model ModelConfig) time.Time {
	pid, err := findVLLMPid(model.Port)
	if err != nil {
		return time.Time{}
	}
	out, err := exec.Command("ps", "-o", "etimes=", "-p", pid).Output()
	if err != nil {
		return time.Time{}
	}
	elapsed, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(elapsed) * time.Second)
}

// readCmdlineArgs reads a process's argv from /proc/<pid>/cmdline, which is
// NUL-separated and therefore avoids shell-quoting ambiguity entirely.
func readCmdlineArgs(pid string) ([]string, error) {
	data, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil {
		return nil, err
	}
	var args []string
	for _, p := range strings.Split(string(data), "\x00") {
		if p != "" {
			args = append(args, p)
		}
	}
	return args, nil
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func speculativeConfig(args []string) string {
	if config := argValue(args, "--speculative-config"); config != "" {
		return config
	}
	model := argValue(args, "--speculative-config.model")
	tokens := argValue(args, "--speculative-config.num_speculative_tokens")
	if model == "" && tokens == "" {
		return ""
	}
	method := "speculative"
	lowerModel := strings.ToLower(model)
	for _, candidate := range []string{"dspark", "dflash", "mtp"} {
		if strings.Contains(lowerModel, candidate) {
			method = candidate
			break
		}
	}
	config := map[string]any{"method": method, "model": model}
	if parsed, err := strconv.Atoi(tokens); err == nil {
		config["num_speculative_tokens"] = parsed
	}
	encoded, _ := json.Marshal(config)
	return string(encoded)
}

func modelDisplayName(info ServerInfo, fallback string) string {
	if info.ServedModelName != "" {
		return info.ServedModelName
	}
	if info.Model != "" {
		parts := strings.Split(info.Model, "/")
		return parts[len(parts)-1]
	}
	return fallback
}

func systemdProperty(model ModelConfig, prop string) string {
	args := []string{"show", model.Unit, "--property=" + prop, "--value"}
	if model.UserUnit {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func buildModelIdentity(model ModelConfig) (ServerInfo, string) {
	info := ServerInfo{}
	processPID := ""

	if pid, err := findVLLMPid(model.Port); err == nil {
		processPID = pid
		if args, aerr := readCmdlineArgs(pid); aerr == nil {
			info.Model = argValue(args, "--model")
			if info.Model == "" {
				for i, a := range args {
					if a == "serve" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						info.Model = args[i+1]
						break
					}
				}
			}
			info.ServedModelName = argValue(args, "--served-model-name")
			info.MaxModelLen = argValue(args, "--max-model-len")
			info.GPUMemUtilization = argValue(args, "--gpu-memory-utilization")
			info.MaxNumBatchedTokens = argValue(args, "--max-num-batched-tokens")
			info.SpeculativeConfig = speculativeConfig(args)
		}
	}

	if info.ServedModelName == "" {
		info.ServedModelName = discoverServedModelName(model.MetricsURL)
	}
	return info, processPID
}

func buildServerInfo(model ModelConfig) ServerInfo {
	info, processPID := cachedModelIdentity(model)
	activeSince := systemdProperty(model, "ActiveEnterTimestamp")
	info.ServiceActiveSince = activeSince
	// Process age is authoritative; oneshot compose units stay active across container restarts.
	if processPID != "" {
		if out, err := exec.Command("ps", "-o", "etimes=", "-p", processPID).Output(); err == nil {
			if elapsed, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); parseErr == nil {
				info.ServiceUptimeSeconds = elapsed
			}
		}
	}
	if info.ServiceUptimeSeconds == 0 && activeSince != "" {
		if t, terr := time.Parse("Mon 2006-01-02 15:04:05 MST", activeSince); terr == nil {
			info.ServiceUptimeSeconds = int64(time.Since(t).Seconds())
		}
	}
	info.ServiceRestarts = systemdProperty(model, "NRestarts")
	info.VLLMVersion = vllmVersion
	info.KernelVersion = kernelVersion

	return info
}
