package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var kernelVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,127}$`)

func parseKernelVersion(release string) string {
	release = strings.TrimSpace(release)
	if !kernelVersionRe.MatchString(release) {
		return ""
	}
	return release
}

// readKernelVersion is read once: the running kernel only changes on reboot.
func readKernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return parseKernelVersion(string(data))
}

func averageClockKHz(values []string) float64 {
	var total float64
	var count int
	for _, value := range values {
		clock, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || clock <= 0 {
			continue
		}
		total += clock
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float64(count) / 1000
}

func scrapeCPUClockMHz() float64 {
	paths, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	if err != nil {
		return 0
	}
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		value, err := os.ReadFile(path)
		if err == nil {
			values = append(values, string(value))
		}
	}
	return averageClockKHz(values)
}

// scrapeCPUTimes reads aggregate CPU jiffies from /proc/stat. Deltas between
// polls give a standard busy/total CPU utilization ratio.
func scrapeCPUTimes() (total, idle float64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0
	}
	fields := strings.Fields(scanner.Text()) // "cpu" user nice system idle iowait irq softirq steal ...
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var vals []float64
	for _, f := range fields[1:] {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue
		}
		vals = append(vals, v)
		total += v
	}
	if len(vals) >= 4 {
		idle = vals[3] // idle
	}
	return
}

// scrapeMem reads /proc/meminfo. On this unified-memory system (GB10), host
// RAM usage is the best available proxy for overall memory pressure since
// nvidia-smi reports GPU memory as N/A.
func scrapeMem() (usedGB, totalGB, usedPct float64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()
	var totalKB, availKB float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseFloat(fields[1], 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	if totalKB == 0 {
		return 0, 0, 0
	}
	usedKB := totalKB - availKB
	totalGB = totalKB / 1024 / 1024
	usedGB = usedKB / 1024 / 1024
	usedPct = usedKB / totalKB * 100
	return
}

// scrapeNet sums rx/tx bytes across all non-loopback interfaces from /proc/net/dev.
func scrapeNet() (rxBytes, txBytes float64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // header lines
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseFloat(fields[0], 64)
		tx, _ := strconv.ParseFloat(fields[8], 64)
		rxBytes += rx
		txBytes += tx
	}
	return
}
