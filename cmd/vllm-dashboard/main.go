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
	"time"

	"github.com/awlx/vllm-dashboard/internal/api"
	"github.com/awlx/vllm-dashboard/internal/collect/gpu"
	"github.com/awlx/vllm-dashboard/internal/collect/host"
	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/collect/vllm"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/discovery"
	"github.com/awlx/vllm-dashboard/internal/fleet"
	"github.com/awlx/vllm-dashboard/internal/metrics"
	"github.com/awlx/vllm-dashboard/internal/poller"
	"github.com/awlx/vllm-dashboard/internal/store"
	"github.com/awlx/vllm-dashboard/internal/web"
)

func main() {
	cfg, err := config.Load(os.Getenv(config.EnvConfig))
	if err != nil {
		log.Fatal(err)
	}
	var discover func() []config.Model
	if cfg.Node.DiscoveryEnabled() {
		discover = func() []config.Model {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return discovery.Discover(ctx, "/proc")
		}
		cfg.Node.Models = discovery.Merge(cfg.Node.Models, discover())
	}
	cfg.Node.Models = discovery.NameModels(cfg.Node.Models, vllm.ServedModelName)
	for _, model := range cfg.Node.Models {
		log.Printf("model %s at %s", model.Key, model.MetricsURL)
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
	go poller.New(st, gpus, procs, discover).Run()

	f := fleet.New(cfg, st, procs)
	if len(cfg.Peers) > 0 {
		go f.RunResolver()
	}

	mux := http.NewServeMux()
	metrics.Register(mux, cfg, st, f.Peers)
	api.Register(mux, f)
	ui, err := web.Handler()
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", ui)

	log.Printf("vllm-dashboard node %q listening on %s, polling every %s", config.DisplayName(cfg.Node.Name, cfg.Node.Hostname), cfg.Listen, st.Interval())
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
