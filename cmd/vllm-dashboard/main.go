// Command vllm-dashboard serves the dashboard UI, the JSON API, and the
// Prometheus metrics for one node or for a fleet of nodes.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/awlx/vllm-dashboard/internal/api"
	"github.com/awlx/vllm-dashboard/internal/collect/gpu"
	"github.com/awlx/vllm-dashboard/internal/collect/host"
	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/metrics"
	"github.com/awlx/vllm-dashboard/internal/poller"
	"github.com/awlx/vllm-dashboard/internal/store"
	"github.com/awlx/vllm-dashboard/internal/web"
)

func main() {
	cfg, err := config.Load(os.Getenv("VLLM_DASHBOARD_CONFIG"))
	if err != nil {
		log.Fatal(err)
	}
	if value := os.Getenv(config.PollIntervalEnv); value != "" {
		if _, err := config.ParsePollInterval(value); err != nil {
			log.Fatalf("%s: %v", config.PollIntervalEnv, err)
		}
		cfg.PollInterval = value
	}
	st := store.New(cfg)
	gpus := gpu.NewSampler(cfg.Interval())
	gpus.Sample()
	if out, err := exec.Command("vllm", "--version").Output(); err == nil {
		st.VLLMVersion = strings.TrimSpace(string(out))
	}
	st.KernelVersion = host.KernelVersion()
	for _, model := range st.Models() {
		startedAt := process.StartedAt(model)
		st.Update(model.Key, func(state *store.ModelState) { state.CounterStartedAt = startedAt })
	}

	procs := process.NewCache(st.VLLMVersion, st.KernelVersion)

	go gpus.Run(context.Background())
	go poller.New(st, gpus, procs).Run()

	mux := http.NewServeMux()
	metrics.Register(mux, cfg, st)
	api.Register(mux, fleet.New(cfg, st, procs))
	ui, err := web.Handler()
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", ui)

	log.Printf("vllm-dashboard node %q listening on %s, polling every %s", config.DisplayName(cfg.Node.Name, cfg.Node.Hostname), cfg.Listen, st.Interval())
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
