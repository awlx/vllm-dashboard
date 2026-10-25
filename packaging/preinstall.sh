#!/bin/sh
set -e

# The service runs as a dedicated unprivileged account. Keep an existing
# account, for example one created by a manual installation.
if ! getent group vllm-dashboard >/dev/null; then
    groupadd --system vllm-dashboard
fi
if ! getent passwd vllm-dashboard >/dev/null; then
    useradd --system --gid vllm-dashboard --home-dir /nonexistent \
        --no-create-home --shell /usr/sbin/nologin \
        --comment "vLLM Fleet Dashboard" vllm-dashboard
fi
