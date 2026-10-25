# Architecture

This document describes how the vLLM dashboard collects, stores, and serves
data. It covers the single-node and the multi-node (fleet) deployment, the
HTTP API, the Prometheus `/metrics` endpoint, and data retention.

The dashboard is one Go binary. The same binary runs as a **node agent** (it
has local models in `node.models`) or as a **fleet collector** (it has
`peers`). A process can have both, but the usual fleet layout gives the
collector an empty `node.models` list.

## Contents

- [Components](#components)
- [Single-node deployment](#single-node-deployment)
- [Multi-node (fleet) deployment](#multi-node-fleet-deployment)
- [Poll cycle and browser refresh](#poll-cycle-and-browser-refresh)
- [Data retention](#data-retention)
- [Data scraped from vLLM](#data-scraped-from-vllm)
- [HTTP API](#http-api)
- [Prometheus `/metrics`](#prometheus-metrics)
- [Grafana](#grafana)

## Components

All Go code is in package `main` in the repository root. Each file has one
area of responsibility.

| Component | File | Main functions | Purpose |
| --- | --- | --- | --- |
| Entry point | `main.go` | `main` | Loads the configuration, applies `VLLM_DASHBOARD_POLL_INTERVAL`, reads the vLLM and kernel versions, starts the GPU sampler and the poll loop, and serves HTTP. Embeds `static/index.html`. |
| Configuration | `config.go` | `loadConfig`, `defaultConfig`, `parsePollInterval`, `displayName` | Reads strict JSON from `VLLM_DASHBOARD_CONFIG`. Unknown fields are rejected. Without the variable, built-in defaults are used. |
| Shared types | `types.go` | `Sample`, `TokenUsage`, `ModelOverview`, `ServerInfo` | JSON types of the HTTP API. |
| Runtime state | `state.go` | `ModelState`, `configureRuntime`, `historySize`, `onlineWindow`, `modelConfig` | Constants, the poll interval, package-level state, and the mutex `mu`. |
| Poll loop | `poll.go` | `runPollLoop`, `poll`, `pollModel` | Runs `poll()` on a ticker at the poll interval (default 1 s). Polls never overlap. Collects host telemetry once and then scrapes all local models in parallel. Calculates rates and averages and appends samples. |
| vLLM scraper | `vllm.go` | `scrapeVLLM`, `normalizeTensorFoldMetrics`, `discoverServedModelName` | Reads the Prometheus text from each model's `metrics_url`. Keeps `vllm:` series (and `tensorfold:` series) and sums label variants per name. |
| GPU sampler | `perf.go`, `gpu.go`, `amdgpu.go` | `runGPUSampler`, `latestGPU`, `scrapeGPU`, `parseNvidiaSMI`, `scrapeAMDGPU` | Runs in its own goroutine. Runs `nvidia-smi`. If it reports no values, runs `amd-smi`. The poll loop uses the last reading. |
| Host scraper | `host.go` | `scrapeCPUTimes`, `scrapeCPUClockMHz`, `scrapeMem`, `scrapeNet`, `readKernelVersion` | Reads `/proc/stat`, `/proc/meminfo`, `/proc/net/dev`, `/proc/sys/kernel/osrelease`, and CPU frequency files in sysfs. |
| Process and service metadata | `process.go`, `perf.go` | `buildModelIdentity`, `buildServerInfo`, `findVLLMPid`, `cachedServerInfo` | Finds the vLLM process with `pgrep`, reads `/proc/<pid>/cmdline`, and reads `systemctl show` properties. Caches the result. |
| History and token usage | `history.go` | `appendLongSample`, `aggregateSampleSets`, `samplesFor`, `tokensInWindow`, `usageForKey` | Builds the minute rollup, merges sample series, and calculates token usage windows. |
| Fleet aggregation | `fleet.go`, `perf.go` | `peerAPI`, `combinedSamples`, `combinedOverview`, `metricsForRequest`, `fanOutPeers` | Resolves model keys, fetches peer JSON APIs in parallel on request, and merges them with local data. |
| HTTP API | `api.go`, `perf.go` | `newServeMux`, `currentSettings`, `writeJSON`, `gzipJSON` | Handlers for `/api/metrics`, `/api/metrics/long`, `/api/info`, `/api/tokens`, `/api/settings`, and `/api/overview`. Optional gzip. |
| Prometheus exporter | `metrics.go` | `registerMetricsHandlers`, `collectTelemetry` | `GET /metrics` for Prometheus and `GET /api/telemetry` for collectors. |
| UI | `static/index.html` | — | Embedded single-page UI. Polls the JSON API. |

The graph shows which file calls functions or reads state in which other file.
All files read the shared state in `state.go`; those edges are left out.
`perf.go` holds the GPU sampler, the caches, the peer fan-out, and gzip.

```mermaid
flowchart TB
  main["main.go"] --> config["config.go"]
  main --> host["host.go"]
  main --> process["process.go"]
  main --> poll["poll.go"]
  main --> api["api.go"]
  main --> perf["perf.go"]
  api --> metrics["metrics.go"]
  poll --> perf
  perf --> gpu["gpu.go + amdgpu.go"]
  poll --> host
  poll --> vllm["vllm.go"]
  poll --> process
  poll --> history["history.go"]
  process --> vllm
  api --> fleet["fleet.go"]
  api --> history
  api --> perf
  fleet --> perf
  fleet --> history
  fleet --> process
  fleet --> config
  history --> fleet
  metrics --> config
  metrics --> host
  state[("state.go<br/>mu, config, models, modelStates")]
```

Issue #9 describes the planned move to `internal/` packages.

## Single-node deployment

Use one dashboard process when all vLLM servers run on the same machine. Add
one entry to `node.models` for each vLLM `/metrics` endpoint.

```mermaid
flowchart LR
  subgraph host["Compute node"]
    direction LR
    v1["vLLM server :8000<br/>/metrics, /v1/models"]
    v2["vLLM server :8001<br/>/metrics, /v1/models"]
    gpu["nvidia-smi or amd-smi"]
    sys["/proc, sysfs"]
    meta["pgrep, ps, /proc/pid/cmdline,<br/>systemctl show"]
    subgraph dash["vllm-dashboard :9090"]
      gpus["GPU sampler<br/>own goroutine"]
      poll["Poll loop<br/>every poll_interval (1 s)"]
      store[("In-memory store<br/>5 min live + 24 h history")]
      api["JSON API /api/*"]
      prom["/metrics exporter"]
      ui["Embedded UI /"]
    end
    v1 -- "HTTP GET" --> poll
    v2 -- "HTTP GET" --> poll
    gpu --> gpus
    gpus -- "last reading" --> poll
    sys --> poll
    poll --> store
    store --> api
    store --> prom
    meta -. "cached 10 s / 30 s" .-> api
  end
  browser["Browser"] -- "polls /api/*" --> api
  browser -- "loads" --> ui
  prometheus["Prometheus"] -- "scrapes /metrics" --> prom
  grafana["Grafana"] -- "PromQL" --> prometheus
```

Prometheus and Grafana are optional. The UI works without them.

## Multi-node (fleet) deployment

Run one node agent on each compute node. Run one more dashboard process as the
fleet collector. The collector has an empty `node.models` list and one `peers`
entry for each node agent. The collector does not store peer data. It calls the
peer APIs when a browser or Prometheus sends a request: `/api/overview`,
`/api/metrics`, `/api/metrics/long`, `/api/info`, and `/api/tokens` for the
UI, and `/api/telemetry` for `/metrics`. Agents do not push data.

```mermaid
flowchart LR
  subgraph nodeA["Compute node A"]
    va["vLLM server(s)"] --> agentA["vllm-dashboard<br/>node agent<br/>node.key = node-a"]
  end
  subgraph nodeB["Compute node B"]
    vb["vLLM server(s)"] --> agentB["vllm-dashboard<br/>node agent<br/>node.key = node-b"]
  end
  subgraph col["Collector host"]
    collector["vllm-dashboard<br/>fleet collector<br/>models: [], peers: [node-a, node-b]"]
  end
  collector -- "HTTP GET /api/*<br/>on each request" --> agentA
  collector -- "HTTP GET /api/*<br/>on each request" --> agentB
  browser["Browser"] --> proxy["Reverse proxy<br/>TLS + authentication"] --> collector
  prometheus["Prometheus"] -- "scrapes /metrics<br/>(collector only)" --> collector
  grafana["Grafana"] --> prometheus
```

### Model identifiers

Each model has a globally scoped key `<node.key>/<model.key>`, for example
`node-a/model-a`. The collector rewrites the keys from each peer to
`<peer.key>/<model key>`, so the peer `key` in the collector configuration is
the node identifier that the UI shows. Use the same value as the agent's
`node.key` to avoid confusion.

The node display name is the first DNS label of `hostname` when `hostname` is
set. Otherwise it is `name`. On the collector, the display name of a peer comes
from the `peers` entry, not from the agent.

### Request routing on the collector

| Request | Collector action |
| --- | --- |
| `model=all` or no `model` | Use local data and call every peer with `model=all`. Merge the results. |
| `model=<own node.key>/<model>` or `model=<model>` | Use local data. |
| `model=<peer.key>/<model>` | Forward the request to that peer with `model=<model>`. |
| Other key | `/api/metrics`: HTTP 502. `/api/info` and `/api/tokens`: HTTP 404. |

The collector calls all peers in parallel (up to 8 at the same time). All
peer calls of one request share one 4-second deadline. The collector does not
follow chains of peers in a defined way. Configure node
agents without `peers`.

### Merging of samples

For `model=all`, the dashboard aligns the sample lists by their **last** sample
(not by timestamp) and cuts all lists to the length of the shortest list. It
then merges each position:

- **Sum:** running and waiting requests, prompt, generation, and request rates,
  completed requests, preemptions, GPU power, network receive and transmit.
- **Maximum:** GPU utilization, temperature, and clock, CPU utilization and
  clock, memory use percent, KV-cache use, TTFT, inter-token latency, and
  end-to-end latency.
- **First list only:** all other fields, for example timestamp, prefix-cache
  hit rate, speculative-decoding values, and memory GB.

Token usage (`/api/tokens`) is the sum of the per-model values.

### Offline peers

A failed peer call does not stop the collector. The collector writes a log
line and continues.

| Endpoint | Effect of an unavailable peer |
| --- | --- |
| `/api/overview` | The models of that peer are not in the list. |
| `/api/metrics?model=all`, `/api/metrics/long?model=all` | The peer is not in the merged series. |
| `/api/tokens?model=all` | The peer is not in the sum. |
| `/api/metrics`, `/api/info`, `/api/tokens` with a key of that peer | HTTP 502. |
| `/metrics` | `vllm_dashboard_node_up{node_key="<peer>"} 0` and no other series for that node. |

A slow peer can delay a collector response by up to 4 seconds in total.

## Poll cycle and browser refresh

The poll loop runs in one goroutine. It starts one goroutine for each model
scrape and waits for all of them. The GPU sampler runs in a separate
goroutine. Browser requests and Prometheus scrapes run in the HTTP server
goroutines and read the store under a read lock.

```mermaid
sequenceDiagram
  autonumber
  participant G as GPU sampler
  participant L as Poll loop
  participant H as Host tools (GPU, /proc, sysfs, pgrep)
  participant V as vLLM (per model)
  participant S as In-memory store
  participant B as Browser
  participant A as Dashboard HTTP API
  participant P as Peer agents

  loop Every poll interval, or slower if the GPU tool is slow
    G->>H: nvidia-smi or amd-smi
    H-->>G: Latest GPU reading
  end
  loop Every poll interval (ticker, no overlap)
    L->>H: /proc/stat, /proc/meminfo, /proc/net/dev, cpufreq
    H-->>L: Host sample (CPU and network rates from deltas)
    L->>G: Read latest GPU reading
    L->>S: Store latest host sample
    par Each configured model, in parallel
      L->>V: GET metrics_url (timeout 1.5 s)
      V-->>L: Prometheus text (vllm:* series)
      L->>H: pgrep, /proc/pid/cmdline (cached 30 s)
      L->>L: Rates and averages from counter deltas
      L->>S: Append to live buffer and to the minute rollup
    end
  end

  B->>A: GET /api/settings (once, poll_interval_ms)
  par Every poll interval
    B->>A: GET /api/metrics?model=KEY
  and Every 10 s
    B->>A: GET /api/overview
    B->>A: GET /api/info?model=KEY
  and Every 15 s
    B->>A: GET /api/tokens?model=KEY
  and Every 60 s
    B->>A: GET /api/metrics/long?model=KEY
  end
  A->>S: Read local samples (read lock)
  opt Collector with peers
    A->>P: Same API call to all peers in parallel (one 4 s deadline)
    P-->>A: JSON
  end
  A-->>B: JSON
```

Details of one model poll:

- If the scrape fails, the dashboard still appends a sample. The sample has
  the host values, zero vLLM values, and the last known token totals.
- Rates (tokens per second, requests per second) and averages (TTFT,
  inter-token latency, end-to-end latency, speculative acceptance) come from
  the difference between two successful scrapes.
- A model is `online` in `/api/overview` when its last successful scrape is
  newer than `max(3 x poll interval, 6 s)`.
- The browser sends the next request of a type only after the previous
  response arrives. It stops all requests while the browser tab is hidden.
- A GPU reading older than `max(5 x poll interval, 10 s)` reads as zero.
- When the UI shows `all`, it also requests the history of each model to draw
  one line for each node or model.

## Data retention

All data is in process memory. A restart deletes all history. Use Prometheus
or another time-series database for long-term storage.

```mermaid
flowchart LR
  poll["Each model poll<br/>(every poll interval)"] --> live[("Live ring buffer<br/>5 min, 300 samples at 1 s<br/>/api/metrics")]
  poll --> rollup["Minute rollup<br/>average of polls"]
  rollup -- "once per 60 s" --> long[("24 h history<br/>1440 samples, 1 per min<br/>/api/metrics/long")]
  long --> tokens["Token usage<br/>last hour, last 24 h<br/>/api/tokens"]
  restart(["Process restart"]) -. "deletes" .-> live
  restart -. "deletes" .-> long
```

| Store | Size | Resolution | Used by |
| --- | --- | --- | --- |
| Live ring buffer (`History`) | 5 minutes per model: 300 samples at `1s` (default), 150 at `2s`, 600 at `500ms` | One sample per poll | `/api/metrics`, `/api/overview` latest sample, `/api/telemetry`, `/metrics` |
| 24-hour history (`LongHistory`) | 1440 samples per model | One sample per 60 s | `/api/metrics/long`, `/api/tokens` |

Each 24-hour sample is the average of all polls in that minute. Latency and
speculative-decoding values use only the polls with activity. Counters use the
last poll of the minute. The minute schedule stays fixed unless polling stops
for two minutes or more.

Token usage is the sum of the counter differences between history samples in
the window. A counter reset (engine restart) counts the new counter value. At
startup, the dashboard reads the start time of the vLLM process. If the
process started inside the window, the token totals in the first history
sample are also counted.

## Data scraped from vLLM

The dashboard reads the vLLM Prometheus text endpoint, normally
`http://<host>:<port>/metrics`. The vLLM OpenAI-compatible server exposes this
endpoint on its API port. The dashboard needs no special vLLM flags. Metrics
that vLLM does not export (for example speculative-decoding counters when
speculative decoding is off) read as zero.

| vLLM series | Dashboard use |
| --- | --- |
| `vllm:num_requests_running`, `vllm:num_requests_waiting` | Running and waiting requests |
| `vllm:kv_cache_usage_perc` | KV-cache use |
| `vllm:prompt_tokens_total`, `vllm:generation_tokens_total` | Token rates and token usage |
| `vllm:request_success_total` | Completed requests and request rate |
| `vllm:num_preemptions_total` | Preemptions |
| `vllm:prefix_cache_queries_total`, `vllm:prefix_cache_hits_total` | Prefix-cache hit rate (cumulative) |
| `vllm:time_to_first_token_seconds_sum` and `_count` | Average TTFT per poll interval |
| `vllm:inter_token_latency_seconds_sum` and `_count` | Average inter-token latency per poll interval |
| `vllm:e2e_request_latency_seconds_sum` and `_count` | Average end-to-end latency per poll interval |
| `vllm:spec_decode_num_accepted_tokens_total`, `vllm:spec_decode_num_draft_tokens_total` | Speculative acceptance rate and mean accepted tokens |

The dashboard sums all label variants of one series name. For example, it adds
the `finished_reason` variants of `vllm:request_success_total`.

TensorFold exporters are also supported:

- **Python exporter** (`vllm:` series and some `tensorfold:` series): the
  dashboard uses the `vllm:` series and also reads `GET <base>/health` for
  live token counters and prefix-cache counters.
- **Native exporter** (only `tensorfold:` series): the dashboard maps the
  `tensorfold:` series to the `vllm:` names above. The generation total is
  `tensorfold:generation_tokens_total` plus
  `tensorfold:generation_tokens_running`. These two series and
  `tensorfold:prompt_tokens_total` are required. The prefix-cache hit rate is
  not available.

A scrape fails when the response has no finite `vllm:` series and is not a
valid native TensorFold response.

Other calls to the same server:

- `GET <base>/v1/models` finds the served model name when the process command
  line has no `--served-model-name`.
- For the TensorFold Python exporter, `GET <base>/health` (see above).

`<base>` is `metrics_url` without the trailing `/metrics`.

## HTTP API

All endpoints are on the `listen` address. There is no authentication.

| Endpoint | Response | `model` parameter |
| --- | --- | --- |
| `GET /` | Embedded UI | — |
| `GET /api/overview` | List of models: key, name, node, `online`, latest sample, server info, token usage. Includes peers. | Not used |
| `GET /api/metrics` | Live samples (about 5 minutes) | `all` (default), local key, or `node/model` |
| `GET /api/metrics/long` | 24-hour samples at one-minute resolution | Same as `/api/metrics` |
| `GET /api/info` | Model and service metadata (command-line values, vLLM and kernel version, systemd uptime and restarts) | A model key is required |
| `GET /api/tokens` | Prompt, generation, and total tokens for the last hour and the last 24 hours | `all` (default), local key, or `node/model` |
| `GET /api/settings` | `poll_interval_ms`, `history_window_seconds`, `long_interval_seconds` of this process | Not used |
| `GET /api/telemetry` | Local-only snapshot (version 1) for collectors. GET and HEAD only. | Not used |
| `GET /metrics` | Prometheus text format. Local node and all peers. GET and HEAD only. | Not used |

`/api/overview` and `/api/info` use cached process and service data. The
cache keeps model identity for 30 seconds and server info for 10 seconds. A
model that comes back online clears its cache entries.

The `/api/*` JSON endpoints, except `/api/telemetry`, send gzip-compressed
responses when the request has `Accept-Encoding: gzip`.

## Prometheus `/metrics`

`GET /metrics` returns metrics in the Prometheus exposition format. The
endpoint is on the same port as the UI. It does not proxy the raw vLLM
metrics. It exports the values that the dashboard computed. Each request
builds a new registry, so the output has no Go runtime or process metrics.

### How the collector builds `/metrics`

```mermaid
sequenceDiagram
  participant Pr as Prometheus
  participant C as Collector /metrics
  participant S as Local store
  participant N as Peer /api/telemetry

  Pr->>C: GET /metrics
  C->>S: Copy local snapshot (read lock)
  par Up to 8 peers at the same time, one shared 4 s deadline
    C->>N: GET /api/telemetry
    N-->>C: Local snapshot of the peer (JSON, max 4 MiB)
  end
  C->>C: Validate snapshots, drop stale model series
  C-->>Pr: vllm_dashboard_* series for all nodes
```

`/api/telemetry` contains only the local data of a node. A collector does not
ask peers for their peers.

### Labels

| Label | Value |
| --- | --- |
| `node` | Node display name (short hostname or `name`; falls back to the key) |
| `node_key` | `node.key` for the local node, `peers[].key` for a peer |
| `model` | Served model name found at runtime (falls back to the configured name) |
| `model_key` | Model `key` from the node configuration (without the node prefix) |
| `kernel_version` | Only on `vllm_dashboard_node_info` |

A change of `node` or `model` value starts new series.

### Node metrics

Labels: `node`, `node_key`.

| Metric | Type | Description |
| --- | --- | --- |
| `vllm_dashboard_node_up` | gauge | 1 when local data or the peer telemetry call is available |
| `vllm_dashboard_node_info` | gauge | Always 1; has `kernel_version` |
| `vllm_dashboard_node_sample_timestamp_seconds` | gauge | Time of the latest host sample |
| `vllm_dashboard_node_cpu_utilization_ratio` | gauge | CPU use, 0 to 1 |
| `vllm_dashboard_node_cpu_clock_hertz` | gauge | Average CPU clock |
| `vllm_dashboard_node_gpu_utilization_ratio` | gauge | GPU use, 0 to 1 |
| `vllm_dashboard_node_gpu_temperature_celsius` | gauge | GPU temperature |
| `vllm_dashboard_node_gpu_power_watts` | gauge | GPU power |
| `vllm_dashboard_node_gpu_clock_hertz` | gauge | GPU clock |
| `vllm_dashboard_node_memory_used_ratio` | gauge | Memory use, 0 to 1 |
| `vllm_dashboard_node_memory_used_bytes` | gauge | Memory used |
| `vllm_dashboard_node_memory_total_bytes` | gauge | Memory total |
| `vllm_dashboard_node_network_receive_bytes_per_second` | gauge | Receive rate, all interfaces except `lo` |
| `vllm_dashboard_node_network_transmit_bytes_per_second` | gauge | Transmit rate, all interfaces except `lo` |

The host gauges are present only when the host sample is less than 30 seconds
old.

### Model metrics

Labels: `node`, `node_key`, `model`, `model_key`.

| Metric | Type | Description |
| --- | --- | --- |
| `vllm_dashboard_model_up` | gauge | 1 when the last poll succeeded and the data is less than 30 s old |
| `vllm_dashboard_model_last_success_timestamp_seconds` | gauge | Time of the last successful scrape |
| `vllm_dashboard_model_sample_timestamp_seconds` | gauge | Time of the latest sample, also after a failed poll |
| `vllm_dashboard_model_requests_running` | gauge | Running requests |
| `vllm_dashboard_model_requests_waiting` | gauge | Waiting requests |
| `vllm_dashboard_model_kv_cache_usage_ratio` | gauge | KV-cache use, 0 to 1 |
| `vllm_dashboard_model_prompt_tokens_per_second` | gauge | Prompt token rate over the last poll interval |
| `vllm_dashboard_model_generation_tokens_per_second` | gauge | Generation token rate over the last poll interval |
| `vllm_dashboard_model_requests_per_second` | gauge | Completed request rate over the last poll interval |
| `vllm_dashboard_model_ttft_average_seconds` | gauge | Average TTFT over the last poll interval |
| `vllm_dashboard_model_itl_average_seconds` | gauge | Average inter-token latency over the last poll interval |
| `vllm_dashboard_model_e2e_average_seconds` | gauge | Average end-to-end latency over the last poll interval |
| `vllm_dashboard_model_prefix_cache_hit_ratio` | gauge | Cumulative prefix-cache hit ratio |
| `vllm_dashboard_model_speculative_acceptance_ratio` | gauge | Speculative acceptance ratio over the last poll interval |
| `vllm_dashboard_model_prompt_tokens_total` | counter | Engine prompt tokens; resets when the engine restarts |
| `vllm_dashboard_model_generation_tokens_total` | counter | Engine generated tokens; resets when the engine restarts |
| `vllm_dashboard_model_requests_completed_total` | counter | Engine completed requests; resets when the engine restarts |
| `vllm_dashboard_model_preemptions_total` | counter | Engine preemptions; resets when the engine restarts |

When `model_up` is 0, only `model_up` and the two timestamp metrics are
present for that model. The other model series are left out; they are not set
to zero. The latency gauges are averages, not histograms or percentiles. For
percentiles, scrape vLLM `/metrics` directly.

### Scrape configuration

Each `/metrics` response contains the local node **and** all configured peers.
Select one of these layouts. Do not scrape the collector and the node agents
in the same job, because that counts each agent two times.

**Single node.** Scrape the dashboard:

```yaml
scrape_configs:
  - job_name: vllm-dashboard
    scrape_interval: 15s
    scrape_timeout: 10s
    metrics_path: /metrics
    static_configs:
      - targets: ['192.0.2.10:9090']
```

**Fleet, recommended.** Scrape only the collector. It fetches the agents in
parallel at scrape time:

```yaml
scrape_configs:
  - job_name: vllm-dashboard
    scrape_interval: 15s
    scrape_timeout: 10s   # larger than the 4 s peer deadline
    metrics_path: /metrics
    static_configs:
      - targets: ['192.0.2.1:9090']   # collector only
```

**Fleet, alternative.** Scrape each node agent and do not scrape the
collector. The agents must have no `peers`. Prometheus then sees each node as
a separate `instance`. The supplied Grafana dashboard selects one `instance`
at a time, so it shows one node at a time in this layout.

```yaml
scrape_configs:
  - job_name: vllm-dashboard
    scrape_interval: 15s
    metrics_path: /metrics
    static_configs:
      - targets: ['192.0.2.10:9090', '192.0.2.11:9090']   # agents, not the collector
```

The scrape interval does not change the dashboard poll interval. Gauges show
the most recent poll at scrape time. Use the `_total` counters with
`rate()` or `increase()` for exact totals over longer periods.

### Example queries

```promql
# Fleet generation throughput, tokens per second
sum(rate(vllm_dashboard_model_generation_tokens_total[5m]))

# Tokens generated per model in the last 24 hours
sum by (node, model) (increase(vllm_dashboard_model_generation_tokens_total[24h]))

# Models that are down
vllm_dashboard_model_up == 0

# Nodes with host data older than 60 seconds
time() - vllm_dashboard_node_sample_timestamp_seconds > 60
```

## Grafana

Grafana reads from Prometheus. It does not scrape the dashboard.

1. Configure Prometheus as shown in [Scrape configuration](#scrape-configuration).
2. In Grafana, open **Dashboards → New → Import**.
3. Upload [`grafana-dashboard.json`](grafana-dashboard.json).
4. Select your Prometheus data source for the `DS_PROMETHEUS` input.
5. In the dashboard, select the `job` and the collector `instance`. The `node`
   and `model` variables accept more than one value.

Counter rates need at least two Prometheus scrapes before the panels show data.
