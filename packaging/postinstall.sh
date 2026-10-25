#!/bin/sh
set -e

config_dir=/etc/vllm-dashboard
config_file=$config_dir/dashboard.json

# The configuration is not a package conffile. The package never creates or
# replaces it, so installs and upgrades never prompt and keep local settings.
if [ ! -d "$config_dir" ]; then
    install -d -m 0750 -o root -g vllm-dashboard "$config_dir"
fi

# amd-smi needs the GPU device nodes, which these groups usually own.
# nvidia-smi works without extra groups.
for group in video render; do
    if getent group "$group" >/dev/null &&
        ! id -nG vllm-dashboard | tr ' ' '\n' | grep -qx "$group"; then
        usermod -a -G "$group" vllm-dashboard
    fi
done

# Always enable and (re)start the service. The old package's prerm stops it
# during upgrade. The configuration file is optional.
if command -v systemctl >/dev/null 2>&1; then
    systemctl enable vllm-dashboard >/dev/null 2>&1 || true
    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload || true
        systemctl restart vllm-dashboard 2>/dev/null || true
    fi
fi

if [ ! -e "$config_file" ]; then
    echo ""
    echo "vllm-dashboard installed. It runs with the defaults and listens on"
    echo "port 9090 on all interfaces. To change the settings, create"
    echo "$config_file, for example from:"
    echo "  /usr/share/vllm-dashboard/config.node.example.json (compute node)"
    echo "  /usr/share/vllm-dashboard/config.example.json (fleet collector)"
    echo "Then run: sudo systemctl restart vllm-dashboard"
    echo ""
fi
