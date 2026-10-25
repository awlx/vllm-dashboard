package gpu

import (
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// commandTimeout bounds GPU tool calls so that a slow or hung tool cannot
// stall the poll loop. amd-smi is a Python tool and often needs more than 1s.
const commandTimeout = 5 * time.Second

func commandOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func scrapeAMD() (util, temp, power, clock float64) {
	out, err := commandOutput("amd-smi", "metric", "--usage", "--temp", "--power", "--clock", "--json")
	if err != nil {
		return 0, 0, 0, 0
	}
	return parseAMDSMIMetrics(out)
}

// parseAMDSMIMetrics reads `amd-smi metric --json` output. It accepts the
// {"gpu_data": [...]} wrapper (ROCm 7.0 and newer) and the top-level array
// (ROCm 6.x). Values can be numbers, numeric strings, {"value": ...} objects,
// or "N/A". Utilization, temperature, and clock are averaged over the GPUs
// that report them. Power is summed.
func parseAMDSMIMetrics(out []byte) (util, temp, power, clock float64) {
	gpus := amdSMIGPUs(out)
	var utilSum, tempSum, clockSum float64
	var utilN, tempN, clockN int
	for _, gpu := range gpus {
		if v, ok := firstAMDValue(gpu,
			[]string{"usage", "gfx_activity"},
			[]string{"usage", "gfx_usage"}); ok {
			utilSum += v
			utilN++
		}
		if v, ok := firstAMDValue(gpu,
			[]string{"temperature", "edge"},
			[]string{"temperature", "hotspot"},
			[]string{"temperature", "junction"}); ok {
			tempSum += v
			tempN++
		}
		if v, ok := firstAMDValue(gpu,
			[]string{"power", "socket_power"},
			[]string{"power", "current_power"}); ok {
			power += v
		}
		if v, ok := firstAMDValue(gpu,
			[]string{"clock", "gfx_0", "clk"},
			[]string{"clock", "gfx", "clk"},
			[]string{"clock", "gfx", "cur_clk"}); ok {
			clockSum += v
			clockN++
		}
	}
	if utilN > 0 {
		util = utilSum / float64(utilN)
	}
	if tempN > 0 {
		temp = tempSum / float64(tempN)
	}
	if clockN > 0 {
		clock = clockSum / float64(clockN)
	}
	return util, temp, power, clock
}

func amdSMIGPUs(out []byte) []map[string]any {
	var root any
	if err := json.Unmarshal(out, &root); err != nil {
		return nil
	}
	var items []any
	switch v := root.(type) {
	case []any:
		items = v
	case map[string]any:
		items, _ = v["gpu_data"].([]any)
	}
	gpus := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if gpu, ok := item.(map[string]any); ok {
			gpus = append(gpus, gpu)
		}
	}
	return gpus
}

func firstAMDValue(gpu map[string]any, paths ...[]string) (float64, bool) {
	for _, path := range paths {
		var node any = gpu
		for _, key := range path {
			m, ok := node.(map[string]any)
			if !ok {
				node = nil
				break
			}
			node = m[key]
		}
		if v, ok := amdNumber(node); ok {
			return v, true
		}
	}
	return 0, false
}

func amdNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return amdNumber(f)
	case map[string]any:
		return amdNumber(x["value"])
	}
	return 0, false
}
