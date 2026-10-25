package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

//go:embed static/index.html
var staticFiles embed.FS

func main() {
	cfg, err := loadConfig(os.Getenv("VLLM_DASHBOARD_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	if value := os.Getenv(pollIntervalEnv); value != "" {
		if _, err := parsePollInterval(value); err != nil {
			log.Fatalf("%s: %v", pollIntervalEnv, err)
		}
		cfg.PollInterval = value
	}
	configureRuntime(cfg)
	sampleGPU()
	if out, err := exec.Command("vllm", "--version").Output(); err == nil {
		vllmVersion = strings.TrimSpace(string(out))
	}
	kernelVersion = readKernelVersion()
	for _, model := range models {
		modelStates[model.Key].CounterStartedAt = modelProcessStartedAt(model)
	}

	go runGPUSampler(context.Background())
	go runPollLoop()

	mux := newServeMux()

	log.Printf("vllm-dashboard node %q listening on %s, polling every %s", displayName(config.Node.Name, config.Node.Hostname), config.Listen, pollInterval)
	log.Fatal(http.ListenAndServe(config.Listen, mux))
}
