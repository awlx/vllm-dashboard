#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    if [ -d /run/systemd/system ]; then
        systemctl stop vllm-dashboard 2>/dev/null || true
    fi
    # Only disable on removal, not on upgrade.
    if [ "$1" != "upgrade" ]; then
        systemctl disable vllm-dashboard >/dev/null 2>&1 || true
    fi
fi
