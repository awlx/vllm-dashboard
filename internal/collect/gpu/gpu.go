// Package gpu reads GPU utilization, temperature, power, and clock from nvidia-smi or amd-smi.
package gpu

import (
	"strconv"
	"strings"
)

// Scrape returns the values of nvidia-smi, or of amd-smi if nvidia-smi reports nothing.
func Scrape() (util, temp, power, clock float64) {
	util, temp, power, clock = scrapeNvidia()
	if util != 0 || temp != 0 || power != 0 || clock != 0 {
		return util, temp, power, clock
	}
	return scrapeAMD()
}

func scrapeNvidia() (util, temp, power, clock float64) {
	out, err := commandOutput("nvidia-smi",
		"--query-gpu=utilization.gpu,temperature.gpu,power.draw,clocks.current.graphics",
		"--format=csv,noheader,nounits")
	if err != nil {
		return 0, 0, 0, 0
	}
	return parseNvidiaSMI(out)
}

// parseNvidiaSMI reads one CSV line per GPU. Utilization, temperature, and
// clock are averaged over the GPUs that report them. Power is summed. Values
// such as "[N/A]" are skipped.
func parseNvidiaSMI(out []byte) (util, temp, power, clock float64) {
	var sums [4]float64
	var counts [4]int
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 4 {
			continue
		}
		for index := range 4 {
			value, err := strconv.ParseFloat(strings.TrimSpace(fields[index]), 64)
			if err != nil || value != value || value < 0 {
				continue
			}
			sums[index] += value
			counts[index]++
		}
	}
	average := func(index int) float64 {
		if counts[index] == 0 {
			return 0
		}
		return sums[index] / float64(counts[index])
	}
	return average(0), average(1), sums[2], average(3)
}
