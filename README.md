# vLLM Fleet Dashboard

A lightweight dashboard for monitoring one or more
[vLLM](https://github.com/vllm-project/vllm) servers. It combines live model
metrics with GPU, CPU, memory, power, temperature, network, process, and systemd
service information in a single embedded web interface.

The dashboard has no runtime package dependencies: build one Go binary, provide
a JSON configuration file, and run it on each compute node or as a fleet
collector.

## Features

- Multiple compute nodes and models in one fleet overview
- Globally scoped `node/model` identifiers
- Two-second live charts and one-minute 24-hour in-memory history
- Throughput, latency, request, KV-cache, prefix-cache, and speculative-decoding metrics
- GPU utilization, temperature, and power from `nvidia-smi`
- Host CPU, memory, and network utilization from Linux `/proc`
- vLLM command-line and systemd service metadata
- Peer aggregation with graceful handling of unavailable nodes
- Strict JSON configuration and no automatic network discovery
- Responsive, dependency-free UI embedded in the binary

## Requirements

- Linux on monitored compute nodes
- Go 1.26 or newer to build
- A vLLM server exposing its Prometheus-compatible `/metrics` endpoint
- `nvidia-smi` for GPU metrics
- `systemctl` for optional service metadata

The fleet collector can run without vLLM or an NVIDIA GPU when its local
`models` list is empty.

## Deployment Modes

### Single Node

Use one dashboard process when all vLLM servers run on the same machine. That
process scrapes every locally configured model and serves both the web UI and
JSON API. No collector or peer configuration is needed.

```text
Browser -> Dashboard -> vLLM /metrics
              -> nvidia-smi, /proc, systemd
```

One node can monitor multiple local vLLM servers. Add one entry to `node.models`
for each metrics endpoint, such as ports 8000 and 8001.

![Single-node dashboard showing one Qwen model](docs/images/single-node-dashboard.png)

_Single-node view with sample data._

### Multiple Nodes

Use an agent-and-collector layout when vLLM servers run on different machines:

1. Run one dashboard process on each compute node with its local models in
  `node.models`. These processes are the node agents.
2. Run one additional dashboard process as the fleet collector. Give it an
  empty `node.models` list and add every node agent under `peers`.
3. Open only the collector UI. It combines peer data and forwards
  model-specific API requests to the correct node.

```text
                 +-> Node A agent -> local vLLM server(s)
Browser -> Fleet collector
                 +-> Node B agent -> local vLLM server(s)
```

Node agents do not push data. The collector polls their HTTP APIs, so it must be
able to reach each configured peer URL. Keep node-agent endpoints on a trusted
network; expose only the collector through an authenticated reverse proxy.

![Multi-node dashboard showing Qwen and DeepSeek models across two nodes](docs/images/multi-node-dashboard.png)

_Fleet view with sample data._

## Quick Start

Build the same binary for either deployment mode:

```sh
git clone https://github.com/awlx/vllm-dashboard.git
cd vllm-dashboard
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o vllm-dashboard .
```

### Single-Node Quick Start

Create the node configuration, add all local vLLM metrics endpoints to its
`models` list, and start the dashboard:

```sh
cp config.node.example.json dashboard.json
VLLM_DASHBOARD_CONFIG=./dashboard.json ./vllm-dashboard
```

Open `http://<node-address>:9090`. If you run the browser on the same machine,
use `http://localhost:9090`.

### Multi-Node Quick Start

On each compute node, copy the binary and use a unique node configuration:

```sh
cp config.node.example.json dashboard.json
# Edit node.key, node.hostname, listen, and node.models for this machine.
VLLM_DASHBOARD_CONFIG=./dashboard.json ./vllm-dashboard
```

Verify each agent from the collector machine before continuing:

```sh
curl http://<node-a-address>:9090/api/overview
curl http://<node-b-address>:9090/api/overview
```

On the collector machine, configure the reachable agent URLs and start another
copy of the same binary:

```sh
cp config.example.json dashboard.json
# Edit peers so each URL points to a running node agent.
VLLM_DASHBOARD_CONFIG=./dashboard.json ./vllm-dashboard
```

Open `http://localhost:9090` on the collector machine, or use the collector's
reverse-proxied URL. You do not need to open the individual node dashboards.

## Configuration

Configuration is loaded from the path in `VLLM_DASHBOARD_CONFIG`. Unknown JSON
fields are rejected so misspelled settings do not silently pass.

### Compute Node

Run an instance beside each vLLM server. Start with
[`config.node.example.json`](config.node.example.json):

```json
{
  "listen": "0.0.0.0:9090",
  "node": {
    "key": "node-a",
    "hostname": "node-a.example.net",
    "models": [
      {
        "key": "model-a",
        "name": "Model A",
        "metrics_url": "http://127.0.0.1:8000/metrics",
        "port": "8000",
        "unit": "vllm-model-a.service"
      }
    ]
  }
}
```

`key` values must be stable, unique lowercase identifiers. `hostname` is
optional; when present, its first DNS label is used as the display name.

Set `user_unit` to `true` when the configured service is managed by
`systemctl --user`. The dashboard process must have permission to inspect the
service and process metadata you configure.

### Fleet Collector

Run another instance with an empty local model list and peer URLs for each
compute-node agent. Start with [`config.example.json`](config.example.json).
The example uses the documentation-only `192.0.2.0/24` network.

The collector forwards model-specific requests to the owning peer and combines
each peer's `all` series for fleet totals. Unavailable peers are omitted from
combined responses rather than taking down the dashboard.

## systemd

The included [`vllm-dashboard.service`](vllm-dashboard.service) provides a
hardened starting point:

```sh
sudo useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin vllm-dashboard
sudo install -m 0755 vllm-dashboard /usr/local/bin/vllm-dashboard
sudo install -d -o vllm-dashboard -g vllm-dashboard /etc/vllm-dashboard
sudo install -m 0640 -o vllm-dashboard -g vllm-dashboard \
  dashboard.json /etc/vllm-dashboard/dashboard.json
sudo install -m 0644 vllm-dashboard.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vllm-dashboard
```

The account-creation command is suitable for distributions that provide
`useradd`; adapt it and the unit for other systems as needed.

## HTTP API

The embedded UI uses these read-only JSON endpoints:

- `GET /api/overview` - current status for all models
- `GET /api/metrics?model=<key>` - five-minute history
- `GET /api/metrics/long?model=<key>` - 24-hour history
- `GET /api/info?model=<key>` - model and service metadata
- `GET /api/tokens?model=<key>` - one-hour and 24-hour token usage

Use `model=all` for aggregate metrics. Fleet model keys use the
`node/model` form.

## Data Retention

History is held in bounded memory and resets when the process restarts. This is
intentional: durable retention should live in Prometheus, VictoriaMetrics, or
another time-series database rather than in the dashboard process.

## Security

The dashboard has no built-in authentication. Node agents and peer APIs should
remain on a private management network. If users access a collector through a
reverse proxy, terminate TLS and enforce authentication there.

The model information endpoint can expose vLLM command-line parameters and
service metadata. Review those values before granting access. Production
configuration files can reveal network topology and are ignored by the supplied
`.gitignore`; do not commit them.

## Development

```sh
go test ./...
go vet ./...
gofmt -w main.go main_test.go
```

The project deliberately uses only the Go standard library and plain HTML,
CSS, and JavaScript.

## License

Licensed under the [Apache License 2.0](LICENSE).
