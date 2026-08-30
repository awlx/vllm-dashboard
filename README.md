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

## Quick Start

```sh
git clone https://github.com/awlx/vllm-dashboard.git
cd vllm-dashboard
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o vllm-dashboard .
cp config.node.example.json dashboard.json
VLLM_DASHBOARD_CONFIG=./dashboard.json ./vllm-dashboard
```

Open `http://localhost:9090` after adjusting `listen` in `dashboard.json` if
necessary.

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
