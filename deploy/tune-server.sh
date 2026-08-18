#!/bin/sh
set -eu

config_path=${QUEQIAO_SYSCTL_CONFIG:-/etc/sysctl.d/90-queqiao-performance.conf}
service_name=${QUEQIAO_SERVICE:-queqiaod.service}
restart_service=true
dry_run=false

usage() {
	cat <<'EOF'
Usage: deploy/tune-server.sh [--dry-run] [--no-restart] [--service NAME]

Persist and apply the Linux socket and backlog limits recommended for a
Queqiao provider. If the selected systemd service is active, it is restarted
so its QUIC socket can request the larger buffer.

Environment:
  QUEQIAO_SYSCTL_CONFIG  sysctl file to install
  QUEQIAO_SERVICE        systemd service name
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--dry-run)
		dry_run=true
		;;
	--no-restart)
		restart_service=false
		;;
	--service)
		shift
		if [ "$#" -eq 0 ]; then
			echo "--service requires a name" >&2
			exit 2
		fi
		service_name=$1
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

settings() {
	cat <<'EOF'
# Queqiao provider socket queues.
#
# quic-go requests an 8 MiB UDP buffer. Linux caps SO_RCVBUF and SO_SNDBUF at
# rmem_max and wmem_max, so leave headroom above that request. The backlog
# limits absorb a synchronized connection/retry wave before userspace accepts
# it. Keep this file on provider hosts that listen on both QUIC and TLS/TCP.
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.netdev_max_backlog = 16384
net.core.somaxconn = 8192
net.ipv4.tcp_max_syn_backlog = 8192
EOF
}

if [ "$dry_run" = true ]; then
	echo "Would install $config_path with:"
	settings
	if [ "$restart_service" = true ]; then
		echo "Would restart $service_name if it is active."
	fi
	exit 0
fi

if [ "$(id -u)" -ne 0 ]; then
	echo "this script must run as root; use: sudo $0" >&2
	exit 1
fi

if [ "$(uname -s)" != Linux ]; then
	echo "this script supports Linux provider hosts only" >&2
	exit 1
fi
if ! command -v sysctl >/dev/null 2>&1; then
	echo "sysctl is required" >&2
	exit 1
fi

config_dir=$(dirname "$config_path")
install -d -m 0755 "$config_dir"
temporary=$(mktemp "${config_path}.tmp.XXXXXX")
trap 'rm -f "$temporary"' 0 HUP INT TERM
settings >"$temporary"
install -o root -g root -m 0644 "$temporary" "$config_path"
rm -f "$temporary"
trap - 0 HUP INT TERM

sysctl -p "$config_path"

verify_minimum() {
	key=$1
	want=$2
	got=$(sysctl -n "$key")
	if [ "$got" -lt "$want" ]; then
		echo "$key is $got after tuning; expected at least $want" >&2
		exit 1
	fi
}

verify_minimum net.core.rmem_max 16777216
verify_minimum net.core.wmem_max 16777216
verify_minimum net.core.netdev_max_backlog 16384
verify_minimum net.core.somaxconn 8192
verify_minimum net.ipv4.tcp_max_syn_backlog 8192

if [ "$restart_service" = true ] && command -v systemctl >/dev/null 2>&1; then
	if systemctl is-active --quiet "$service_name"; then
		systemctl restart "$service_name"
		systemctl is-active --quiet "$service_name"
		echo "Restarted active service $service_name."
	else
		echo "$service_name is not active; no restart was needed."
	fi
fi

echo "Queqiao server networking limits are installed and active."
