#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
	echo "packaging assertion failed: $*" >&2
	exit 1
}

assert_contains() {
	grep -F -- "$2" "$1" >/dev/null || fail "$1 does not contain $2"
}

assert_not_contains() {
	if grep -F -- "$2" "$1" >/dev/null; then
		fail "$1 unexpectedly contains $2"
	fi
}

unit=$repo/packaging/vllm-dashboard.service
assert_contains "$unit" "ExecStart=/usr/bin/vllm-dashboard"
assert_contains "$unit" "Environment=VLLM_DASHBOARD_CONFIG=/etc/vllm-dashboard/dashboard.json"
assert_not_contains "$unit" "ConditionPathExists="
assert_contains "$unit" "User=vllm-dashboard"
assert_not_contains "$unit" "/usr/local/"

# The packaged unit must match the manual-install unit except for the binary
# path.
grep -v -e '^ExecStart=' "$unit" > "$tmp/packaged"
grep -v -e '^ExecStart=' "$repo/vllm-dashboard.service" > "$tmp/manual"
cmp "$tmp/manual" "$tmp/packaged" ||
	fail "packaging/vllm-dashboard.service differs from vllm-dashboard.service"

nfpm=$repo/nfpm.yaml
assert_contains "$nfpm" "dst: /usr/bin/vllm-dashboard"
assert_contains "$nfpm" "dst: /lib/systemd/system/vllm-dashboard.service"
assert_contains "$nfpm" "dst: /usr/share/vllm-dashboard/config.example.json"
assert_contains "$nfpm" "dst: /usr/share/vllm-dashboard/config.node.example.json"
assert_not_contains "$nfpm" "dst: /etc/"

for script in preinstall postinstall preremove postremove package-version test-packaging test-deb-install; do
	file=$repo/packaging/$script.sh
	[ -x "$file" ] || fail "$file is not executable"
	sh -n "$file" || fail "$file has a syntax error"
done

version=$repo/packaging/package-version.sh
[ "$("$version" tag v1.2.3 1 1 abc)" = "1.2.3" ] || fail "tag version"
[ "$("$version" branch main 42 2 0123456789abcdef)" = "0.0.0~git42.2.g0123456789ab" ] ||
	fail "snapshot version"
if "$version" tag vfoo 1 1 abc 2>/dev/null; then
	fail "accepted tag without numeric version"
fi
if "$version" tag 'v1.0;x' 1 1 abc 2>/dev/null; then
	fail "accepted tag with invalid characters"
fi

assert_contains "$repo/packaging/package-repository-index.html" "vllm-dashboard.gpg.key"
echo "packaging checks passed"
