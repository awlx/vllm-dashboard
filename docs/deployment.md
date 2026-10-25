# Deployment

This document gives details for the APT package, the container image, and a
manual systemd installation.

## APT package

Tagged releases publish `.deb` packages for `amd64` and `arm64`. The packages
are on the GitHub release page and in a signed APT repository. See the
[README](../README.md#debian-and-ubuntu-apt) for the installation steps.

The package installs these files:

- `/usr/bin/vllm-dashboard`: the binary with the embedded UI.
- `/lib/systemd/system/vllm-dashboard.service`: the systemd unit.
- `/usr/share/vllm-dashboard/config.node.example.json` and
  `config.example.json`: example configurations.

The package creates the `vllm-dashboard` system user. It adds the user to the
`video` and `render` groups if they exist.

The package enables and starts the service. An upgrade restarts it. The
configuration file `/etc/vllm-dashboard/dashboard.json` is optional. Without
it, the service uses the defaults and listens on port 9090 on all interfaces.

The package does not create or change the configuration file. Install,
upgrade, and removal do not ask about it and do not replace it. To use a
configuration, start from an example and restart the service:

```sh
sudo install -m 0640 -o root -g vllm-dashboard \
  /usr/share/vllm-dashboard/config.node.example.json \
  /etc/vllm-dashboard/dashboard.json
sudoedit /etc/vllm-dashboard/dashboard.json
sudo systemctl restart vllm-dashboard
```

The service runs as `vllm-dashboard`. `nvidia-smi`, `ps`, `pgrep`, and
`systemctl show` work without extra privileges. `amd-smi` needs the `video`
and `render` groups for the GPU device nodes.

To install a downloaded package file, run
`sudo apt install ./vllm-dashboard_<version>_<arch>.deb`.

### Migrate from a manual installation

A unit in `/etc/systemd/system` overrides the packaged unit. Remove the manual
files after you install the package. Keep the configuration.

```sh
sudo rm /etc/systemd/system/vllm-dashboard.service /usr/local/bin/vllm-dashboard
sudo systemctl daemon-reload
sudo systemctl restart vllm-dashboard
```

## Container image

The image is `ghcr.io/awlx/vllm-dashboard`. It runs on `linux/amd64` and
`linux/arm64`.

| Tag | Source |
| --- | --- |
| `X.Y.Z`, `X.Y` | Release tag `vX.Y.Z` |
| `latest` | Most recent release tag |
| `main`, `sha-<short>` | Latest commit on `main` (not released) |

Use an `X.Y.Z` tag for reproducible deployments. The image runs as user
65532. It reads the configuration from `/etc/vllm-dashboard/dashboard.json`.
The base image is `debian:bookworm-slim`, because a node needs `ps`, `pgrep`,
and the `nvidia-smi` of the NVIDIA Container Toolkit.

### Collector

A collector only calls the peer APIs. It needs no extra flags.

```sh
docker run -d --name vllm-dashboard --read-only --cap-drop ALL \
  -p 127.0.0.1:9090:9090 \
  -v "$PWD/dashboard.json:/etc/vllm-dashboard/dashboard.json:ro" \
  ghcr.io/awlx/vllm-dashboard:latest
```

[`docker-compose.example.yml`](../docker-compose.example.yml) has the same
setup for Docker Compose. The host data of a collector container is usually
not relevant.

```sh
docker compose -f docker-compose.example.yml up -d vllm-dashboard
```

### Node

A node needs no configuration file.

```sh
docker run -d --name vllm-dashboard --read-only --cap-drop ALL \
  --gpus all --pid host --network host \
  ghcr.io/awlx/vllm-dashboard:latest
```

- `--gpus all` gives GPU data. It needs the
  [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/).
  The toolkit adds `nvidia-smi` to the container.
- `--pid host` lets the dashboard see the vLLM processes. Discovery, the
  command line, and the uptime need it.
- `--network host` gives the host network counters. It also lets the
  dashboard reach vLLM on `127.0.0.1`.
- The image has no `systemctl`. Service data is not available.
- The image has no `vllm` command. The vLLM version is not available.

`--pid host` and `--network host` decrease the isolation of the container.
Use them only on trusted nodes. For complete data on a node, use the APT
package or the binary with systemd.

The `node-agent` service in `docker-compose.example.yml` is the same setup
without GPU data:

```sh
docker compose -f docker-compose.example.yml --profile node up -d node-agent
```

## Manual systemd installation

[`vllm-dashboard.service`](../vllm-dashboard.service) is a hardened unit. The
configuration file is optional.

```sh
sudo useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin vllm-dashboard
sudo install -m 0755 vllm-dashboard /usr/local/bin/vllm-dashboard
sudo install -d -o vllm-dashboard -g vllm-dashboard /etc/vllm-dashboard
sudo install -m 0644 vllm-dashboard.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vllm-dashboard
```

## Data retention

The dashboard keeps all history in memory. A restart deletes the history. Use
Prometheus or another time-series database for long-term data.

## Security

- The dashboard has no authentication.
- Keep nodes and peer APIs on a private network.
- Put the collector behind a reverse proxy with TLS and authentication.
- `/api/info` shows the vLLM command line and service data. Examine these
  values before you give access.
- Configuration files can show your network layout. The supplied `.gitignore`
  ignores them. Do not commit them.
