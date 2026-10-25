package main

func scrapeGPU() (util, temp, power, clock float64) {
	util, temp, power, clock = scrapeNvidiaGPU()
	if util != 0 || temp != 0 || power != 0 || clock != 0 {
		return util, temp, power, clock
	}
	return scrapeAMDGPU()
}

func scrapeNvidiaGPU() (util, temp, power, clock float64) {
	out, err := gpuCommandOutput("nvidia-smi",
		"--query-gpu=utilization.gpu,temperature.gpu,power.draw,clocks.current.graphics",
		"--format=csv,noheader,nounits")
	if err != nil {
		return 0, 0, 0, 0
	}
	return parseNvidiaSMI(out)
}
