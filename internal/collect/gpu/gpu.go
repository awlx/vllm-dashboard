// Package gpu reads per-GPU and aggregate GPU data from nvidia-smi or amd-smi.
package gpu

import (
	"strconv"
	"strings"

	"github.com/awlx/vllm-dashboard/internal/sample"
)

// Reading is one snapshot of all GPUs of the host. Util, Temp, and Clock are
// the average over the GPUs that report them. Power is the sum. Procs lists
// the per-process GPU memory of nvidia hosts.
type Reading struct {
	GPUs  []sample.GPUStat
	Procs []ProcMem
	Util  float64
	Temp  float64
	Power float64
	Clock float64
}

// ProcMem is the VRAM that one process uses on one GPU, identified by the
// GPU's UUID.
type ProcMem struct {
	PID  int
	UUID string
	MB   float64
}

// Scrape returns the state of all GPUs, reading nvidia-smi, or amd-smi if
// nvidia-smi reports no GPUs.
func Scrape() Reading {
	if reading := scrapeNvidia(); len(reading.GPUs) > 0 {
		return reading
	}
	return scrapeAMD()
}

func scrapeNvidia() Reading {
	out, err := commandOutput("nvidia-smi",
		"--query-gpu=index,uuid,name,memory.used,memory.total,utilization.gpu,utilization.memory,temperature.gpu,power.draw,clocks.current.graphics",
		"--format=csv,noheader,nounits")
	if err != nil {
		return Reading{}
	}
	reading := parseNvidiaSMI(out)
	if apps, err := commandOutput("nvidia-smi",
		"--query-compute-apps=pid,gpu_uuid,used_memory",
		"--format=csv,noheader,nounits"); err == nil {
		reading.Procs = parseComputeApps(apps)
	}
	return reading
}

const (
	nvidiaIndex = iota
	nvidiaUUID
	nvidiaName
	nvidiaMemUsed
	nvidiaMemTotal
	nvidiaUtil
	nvidiaMemUtil
	nvidiaTemp
	nvidiaPower
	nvidiaClock
	nvidiaFieldCount
)

// parseNvidiaSMI reads one CSV line per GPU. Utilization, temperature, and
// clock are averaged over the GPUs that report them. Power is summed. Values
// such as "[N/A]" are skipped.
func parseNvidiaSMI(out []byte) Reading {
	reading := Reading{}
	// Sums and counts of the fields that are averaged into the aggregate
	// reading. Power is added directly.
	var utilSum, tempSum, clockSum float64
	var utilN, tempN, clockN int
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < nvidiaFieldCount {
			continue
		}
		gpu := sample.GPUStat{
			UUID: strings.TrimSpace(fields[nvidiaUUID]),
			Name: strings.TrimSpace(fields[nvidiaName]),
		}
		if index, err := strconv.Atoi(strings.TrimSpace(fields[nvidiaIndex])); err == nil {
			gpu.Index = index
		}
		if v, ok := nvidiaField(fields[nvidiaMemUsed]); ok {
			gpu.VRAMUsedMB = v
		}
		if v, ok := nvidiaField(fields[nvidiaMemTotal]); ok {
			gpu.VRAMTotalMB = v
		}
		if v, ok := nvidiaField(fields[nvidiaUtil]); ok {
			gpu.UtilPct = v
			utilSum += v
			utilN++
		}
		if v, ok := nvidiaField(fields[nvidiaMemUtil]); ok {
			gpu.MemUtilPct = v
		}
		if v, ok := nvidiaField(fields[nvidiaTemp]); ok {
			gpu.TempC = v
			tempSum += v
			tempN++
		}
		if v, ok := nvidiaField(fields[nvidiaPower]); ok {
			gpu.PowerW = v
			reading.Power += v
		}
		if v, ok := nvidiaField(fields[nvidiaClock]); ok {
			gpu.ClockMHz = v
			clockSum += v
			clockN++
		}
		reading.GPUs = append(reading.GPUs, gpu)
	}
	if utilN > 0 {
		reading.Util = utilSum / float64(utilN)
	}
	if tempN > 0 {
		reading.Temp = tempSum / float64(tempN)
	}
	if clockN > 0 {
		reading.Clock = clockSum / float64(clockN)
	}
	return reading
}

// parseComputeApps reads one "pid, gpu_uuid, used_memory" line per process
// that holds a CUDA context on a GPU.
func parseComputeApps(out []byte) []ProcMem {
	var procs []ProcMem
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || pid <= 0 {
			continue
		}
		proc := ProcMem{PID: pid, UUID: strings.TrimSpace(fields[1])}
		if v, ok := nvidiaField(fields[2]); ok {
			proc.MB = v
		}
		procs = append(procs, proc)
	}
	return procs
}

// nvidiaField parses one CSV field. Empty values and markers such as "[N/A]"
// are reported as missing.
func nvidiaField(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "[") || strings.EqualFold(value, "N/A") {
		return 0, false
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number != number || number < 0 {
		return 0, false
	}
	return number, true
}
