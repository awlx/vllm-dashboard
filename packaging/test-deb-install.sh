#!/bin/sh
# Install, upgrade, and remove the deb on a host that runs systemd.
# Usage: sudo ./packaging/test-deb-install.sh <old.deb> <new.deb>
set -eu

old=$1
new=$2
config=/etc/vllm-dashboard/dashboard.json
port=19090

fail() {
	echo "deb install test failed: $*" >&2
	systemctl status vllm-dashboard --no-pager >&2 || true
	journalctl -u vllm-dashboard --no-pager -n 50 >&2 || true
	exit 1
}

wait_http() {
	p=${1:-$port}
	i=0
	until curl -fsS "http://127.0.0.1:$p/" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 30 ] || fail "dashboard does not answer on port $p"
		sleep 1
	done
}

export DEBIAN_FRONTEND=noninteractive

# Fresh install without configuration: enabled and started with defaults.
dpkg -i "$old"
getent passwd vllm-dashboard >/dev/null || fail "system user missing"
[ "$(getent passwd vllm-dashboard | cut -d: -f7)" = "/usr/sbin/nologin" ] ||
	fail "system user has a login shell"
test -x /usr/bin/vllm-dashboard || fail "binary missing"
test -f /usr/share/vllm-dashboard/config.example.json || fail "example missing"
test -d /etc/vllm-dashboard || fail "configuration directory missing"
test ! -e "$config" || fail "package created $config"
[ "$(stat -c '%U:%G %a' /etc/vllm-dashboard)" = "root:vllm-dashboard 750" ] ||
	fail "configuration directory owner or mode"
[ -z "$(dpkg-query -W -f='${Conffiles}' vllm-dashboard)" ] ||
	fail "package has conffiles"
[ -z "$(dpkg -V vllm-dashboard)" ] || fail "dpkg -V reports changes"
systemctl is-enabled --quiet vllm-dashboard || fail "service not enabled"
wait_http 9090

# Add a configuration and restart the service.
cat > "$config" <<JSON
{
  "listen": "127.0.0.1:$port",
  "node": {
    "key": "collector",
    "name": "Package Test",
    "models": []
  }
}
JSON
chown root:vllm-dashboard "$config"
chmod 0640 "$config"
before=$(sha256sum "$config")
systemctl restart vllm-dashboard
wait_http
[ "$(ps -o user= -p "$(systemctl show -p MainPID --value vllm-dashboard)")" = "vllm-dashboard" ] ||
	fail "service does not run as vllm-dashboard"
old_pid=$(systemctl show -p MainPID --value vllm-dashboard)

# Upgrade keeps the configuration and restarts the service.
dpkg -i "$new"
[ "$(sha256sum "$config")" = "$before" ] || fail "upgrade changed $config"
[ -z "$(dpkg -V vllm-dashboard)" ] || fail "dpkg -V reports changes after upgrade"
systemctl is-enabled --quiet vllm-dashboard || fail "upgrade disabled the service"
wait_http
new_pid=$(systemctl show -p MainPID --value vllm-dashboard)
[ "$new_pid" != "$old_pid" ] || fail "upgrade did not restart the service"

# Reinstall of the same version also keeps the service running.
dpkg -i "$new"
wait_http

# Removal stops and disables the service and keeps the configuration.
dpkg -r vllm-dashboard
! systemctl is-active --quiet vllm-dashboard || fail "service still active after removal"
test ! -e /usr/bin/vllm-dashboard || fail "binary still present after removal"
test ! -e /lib/systemd/system/vllm-dashboard.service || fail "unit still present after removal"
[ "$(sha256sum "$config")" = "$before" ] || fail "removal changed $config"
dpkg -P vllm-dashboard

# Install over an existing configuration starts the service immediately.
dpkg -i "$new"
wait_http
dpkg -P vllm-dashboard

echo "deb install test passed"
