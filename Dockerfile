# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY static ./static
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/vllm-dashboard .

# Debian slim instead of distroless: node mode runs ps and pgrep (procps), and
# the NVIDIA Container Toolkit injects a glibc-linked nvidia-smi into the
# container. The fleet collector needs neither but uses the same image.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends procps \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 vllm-dashboard \
    && useradd --system --uid 65532 --gid 65532 --home-dir /nonexistent \
       --no-create-home --shell /usr/sbin/nologin vllm-dashboard \
    && install -d -m 0755 /etc/vllm-dashboard
COPY --from=build /out/vllm-dashboard /usr/local/bin/vllm-dashboard
ENV VLLM_DASHBOARD_CONFIG=/etc/vllm-dashboard/dashboard.json \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=utility
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/vllm-dashboard"]
