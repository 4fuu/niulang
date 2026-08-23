#!/bin/sh
set -eu

# One-command Queqiao desktop client install for macOS and Linux. It enrolls
# one or more invitations, writes the multi-provider manifest, installs a
# per-user service that starts at login (launchd on macOS, a systemd --user
# unit with lingering on Linux), and verifies each SOCKS5 listener.
#
# Run it as the account that will use the tunnel, not as root: the profile is a
# private key file owned by that user, and the service is a user agent.
#
# Adding a provider later is the same command with only the new invitation.
# Existing manifest entries and their listener ports are preserved.

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)

home=${HOME:?HOME must be set}
prefix=$home/.queqiao
config_dir=$home/.config/queqiao
base_port=12080
metrics_listen=127.0.0.1:12090
local_address=auto
device_name=
log_level=info
label=me.01.queqiao.client
service_name=queqiao-client
binary_source=
verify=true
egress_check=true
start_service=true
dry_run=false

tab=$(printf '\t')
work_dir=
staged_binary=
binary_origin=

usage() {
	cat <<'EOF'
Usage: deploy/install-client.sh --invite 'queqiao://enroll/...' [--invite ...] [options]

Enroll one or more provider invitations, install queqiaod as a per-user
service that starts at login, and verify the resulting SOCKS5 listeners.

Invitations:
  --invite URI             a single-use invitation; repeat for several providers
  --invite NAME=URI        the same, with an explicit manifest name
  URI                      an invitation may also be given positionally

Options:
  --base-port PORT         first loopback SOCKS5 port (default 12080, matching
                           deploy/clash-queqiao.yaml); later providers take the
                           next free ports
  --local-address VALUE    outer source for enrollment and traffic: auto, an IP,
                           or if:NAME (default auto). Use if:en0 when Clash TUN
                           owns the default route or two uplinks are active.
  --device-name NAME       device label shown to the provider (default hostname)
  --metrics-listen ADDR    loopback metrics address (default 127.0.0.1:12090)
  --log-level LEVEL        debug, info, warn, or error (default info)
  --config-dir DIR         profiles and manifest (default ~/.config/queqiao)
  --prefix DIR             binary install prefix (default ~/.queqiao)
  --label NAME             macOS LaunchAgent label (default me.01.queqiao.client)
  --service-name NAME      Linux systemd --user unit name (default queqiao-client)
  --binary PATH            install this queqiaod instead of building one
  --no-start               write the service definition without loading it
  --no-egress-check        skip the outbound request through the tunnel
  --no-verify              skip listener and egress verification entirely
  --dry-run                print the plan and exit without changing anything
  -h, --help               show this help

The binary is taken from --binary, then ./queqiaod in the repository root,
then a local `go build ./cmd/queqiaod`.
EOF
}

die() {
	echo "install-client.sh: $*" >&2
	exit 1
}

usage_error() {
	echo "install-client.sh: $*" >&2
	echo "Run deploy/install-client.sh --help for usage." >&2
	exit 2
}

next_value() {
	if [ "$1" -lt 2 ]; then
		usage_error "$2 requires a value"
	fi
}

work_dir=$(mktemp -d)
cleanup() {
	rm -rf "$work_dir"
}
trap cleanup 0 HUP INT TERM
pending=$work_dir/pending
entries=$work_dir/entries
: >"$pending"
: >"$entries"

# An invitation may be written as NAME=URI. Splitting on the first '=' is
# unambiguous because a name is restricted to slug characters here, while the
# URI always begins with its scheme.
#
# The URI is stored first because a tab is IFS whitespace: a leading empty
# field would be stripped on read and the invitation itself would be parsed as
# the name. An unnamed provider leaves the trailing field empty instead.
record_invite() {
	case $1 in
	queqiao://*)
		printf '%s\t\n' "$1" >>"$pending"
		;;
	[A-Za-z0-9]*=queqiao://*)
		invite_name=${1%%=*}
		case $invite_name in
		*[!A-Za-z0-9._-]*)
			die "a provider name may use letters, digits, dot, underscore, and dash only: $invite_name"
			;;
		esac
		printf '%s\t%s\n' "${1#*=}" "$invite_name" >>"$pending"
		;;
	*)
		die "not an invitation URI or NAME=URI pair: $1"
		;;
	esac
}

while [ "$#" -gt 0 ]; do
	case $1 in
	--invite)
		next_value "$#" "$1"
		record_invite "$2"
		shift
		;;
	--base-port)
		next_value "$#" "$1"
		base_port=$2
		shift
		;;
	--local-address)
		next_value "$#" "$1"
		local_address=$2
		shift
		;;
	--device-name)
		next_value "$#" "$1"
		device_name=$2
		shift
		;;
	--metrics-listen)
		next_value "$#" "$1"
		metrics_listen=$2
		shift
		;;
	--log-level)
		next_value "$#" "$1"
		log_level=$2
		shift
		;;
	--config-dir)
		next_value "$#" "$1"
		config_dir=$2
		shift
		;;
	--prefix)
		next_value "$#" "$1"
		prefix=$2
		shift
		;;
	--label)
		next_value "$#" "$1"
		label=$2
		shift
		;;
	--service-name)
		next_value "$#" "$1"
		service_name=$2
		shift
		;;
	--binary)
		next_value "$#" "$1"
		binary_source=$2
		shift
		;;
	--no-start)
		start_service=false
		;;
	--no-egress-check)
		egress_check=false
		;;
	--no-verify)
		verify=false
		;;
	--dry-run)
		dry_run=true
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		usage_error "unknown argument: $1"
		;;
	*)
		record_invite "$1"
		;;
	esac
	shift
done

case $(uname -s) in
Darwin) platform=macos ;;
Linux) platform=linux ;;
*) die "the client installer supports macOS and Linux; use the manual steps in docs/DEPLOYING.md" ;;
esac

if [ "$(id -u)" -eq 0 ]; then
	die "run this as the account that will use the tunnel, not with sudo.
The profile is that user's private key and the service is a per-user agent."
fi

case $base_port in
'' | *[!0-9]*) usage_error "--base-port must be numeric" ;;
esac
if [ "$base_port" -lt 1 ] || [ "$base_port" -gt 65535 ]; then
	usage_error "--base-port $base_port is out of range"
fi

# Paths are emitted verbatim into JSON and, on macOS, into plist XML. Rather
# than escaping every metacharacter in two syntaxes, accept only characters
# that need no escaping in either.
for path_value in "$prefix" "$config_dir"; do
	case $path_value in
	*[!A-Za-z0-9/._@+:~\ -]*)
		die "path contains a character this installer will not quote safely: $path_value"
		;;
	esac
done

binary_path=$prefix/bin/queqiaod
manifest=$config_dir/providers.json
if [ -z "$device_name" ]; then
	device_name=$(hostname 2>/dev/null || echo device)
fi

name_taken() {
	cut -f1 "$entries" | grep -Fqx "$1"
}

port_taken() {
	cut -f3 "$entries" | grep -Fqx "127.0.0.1:$1"
}

allocate_port() {
	candidate=$base_port
	while port_taken "$candidate"; do
		candidate=$((candidate + 1))
		[ "$candidate" -le 65535 ] || die "no free loopback port at or above $base_port"
	done
	printf '%s' "$candidate"
}

slugify() {
	printf '%s' "$1" | tr '[:upper:]' '[:lower:]' |
		sed -e 's/[^a-z0-9]\{1,\}/-/g' -e 's/^-*//' -e 's/-*$//' -e 's/^\(.\{1,40\}\).*$/\1/'
}

# The manifest is rewritten from this script's own one-object-per-line shape.
# A hand-edited file is not re-serialized blindly: an unrecognized layout stops
# the install rather than dropping providers the operator added by hand.
load_existing_manifest() {
	[ -f "$manifest" ] || return 0
	declared=$(grep -c '"name"' "$manifest" || true)
	parsed=$(grep -o '{"name": "[^"]*", "profile": "[^"]*", "listen": "[^"]*"}' "$manifest" || true)
	found=0
	if [ -n "$parsed" ]; then
		found=$(printf '%s\n' "$parsed" | wc -l | tr -d ' ')
	fi
	if [ "$declared" -ne "$found" ]; then
		die "$manifest was edited into a shape this script cannot merge.
Add the new provider by hand, or move the file aside and re-enroll every
provider in one run."
	fi
	[ "$found" -gt 0 ] || return 0
	printf '%s\n' "$parsed" | while IFS= read -r object; do
		rest=${object#\{\"name\": \"}
		entry_name=${rest%%\"*}
		rest=${rest#*\", \"profile\": \"}
		entry_profile=${rest%%\"*}
		rest=${rest#*\", \"listen\": \"}
		entry_listen=${rest%%\"*}
		printf '%s\t%s\t%s\n' "$entry_name" "$entry_profile" "$entry_listen" >>"$entries"
	done
	echo "Keeping $found provider(s) already in $manifest."
}

if [ ! -s "$pending" ] && [ ! -f "$manifest" ]; then
	usage_error "at least one --invite is required for a first install"
fi

if [ "$dry_run" = true ]; then
	echo "Would install $binary_path and write $manifest."
	echo "Would enroll $(wc -l <"$pending" | tr -d ' ') invitation(s) with --local-address $local_address as device \"$device_name\"."
	echo "Would allocate loopback SOCKS5 ports from $base_port upward."
	if [ "$platform" = macos ]; then
		echo "Would install the LaunchAgent $home/Library/LaunchAgents/$label.plist."
	else
		echo "Would install the systemd user unit $home/.config/systemd/user/$service_name.service."
	fi
	exit 0
fi

if [ -n "$binary_source" ]; then
	[ -x "$binary_source" ] || die "--binary $binary_source is not an executable file"
	staged_binary=$binary_source
	binary_origin="supplied binary"
elif [ -x "$repo_root/queqiaod" ]; then
	staged_binary=$repo_root/queqiaod
	binary_origin="prebuilt $repo_root/queqiaod"
elif command -v go >/dev/null 2>&1; then
	echo "Building queqiaod from $repo_root ..."
	(cd "$repo_root" && go build -o "$work_dir/queqiaod" ./cmd/queqiaod) ||
		die "go build failed; build it separately and pass --binary PATH"
	staged_binary=$work_dir/queqiaod
	binary_origin="source build"
else
	die "no queqiaod binary found.
Pass --binary PATH, place a built binary at $repo_root/queqiaod, or install Go
and re-run: go build -o ./queqiaod ./cmd/queqiaod"
fi

version_line=$("$staged_binary" version) ||
	die "$staged_binary did not run on this host; check the build architecture"
case $version_line in
*wire=1*) ;;
*) die "refusing to install a binary that does not speak protocol 1: $version_line" ;;
esac
echo "Installing $version_line ($binary_origin)."

install -d -m 0755 "$prefix/bin"
install -d -m 0700 "$config_dir"

# Rename into place so a running client keeps its mapped image.
install -m 0755 "$staged_binary" "$binary_path.new"
mv -f "$binary_path.new" "$binary_path"

load_existing_manifest

index=0
while IFS="$tab" read -r invitation requested_name; do
	index=$((index + 1))
	entry_name=
	if [ -n "$requested_name" ]; then
		entry_name=$(slugify "$requested_name")
		[ -n "$entry_name" ] || die "provider name $requested_name has no usable characters"
	fi

	if [ -n "$entry_name" ]; then
		! name_taken "$entry_name" || die "provider $entry_name is already in $manifest"
		profile_path=$config_dir/$entry_name.json
	else
		profile_path=$config_dir/provider-$index.json
	fi
	[ ! -e "$profile_path" ] ||
		die "$profile_path already exists; give this invitation a distinct name with --invite NAME=URI"

	echo "Enrolling into $profile_path ..."
	enroll_output=$("$binary_path" enroll "$invitation" \
		--profile "$profile_path" \
		--device-name "$device_name" \
		--local-address "$local_address") || die "enrollment failed.
A one-time invitation is consumed on use; ask the provider for a new one unless
$profile_path.enrolling was left behind, which the same URI can safely retry.
When two physical uplinks are active, pass --local-address if:NAME."
	echo "$enroll_output"

	# Prefer the provider's own display name over provider-N once enrollment
	# has revealed it. The manifest name tags every runtime log record, so it
	# is worth making it recognizable.
	if [ -z "$entry_name" ]; then
		display_name=$(printf '%s\n' "$enroll_output" |
			sed -n 's/^Enrolled "\(.*\)" as device .*$/\1/p')
		candidate_name=$(slugify "$display_name")
		if [ -n "$candidate_name" ] && ! name_taken "$candidate_name" &&
			[ ! -e "$config_dir/$candidate_name.json" ]; then
			mv "$profile_path" "$config_dir/$candidate_name.json"
			rm -f "$profile_path.lock"
			profile_path=$config_dir/$candidate_name.json
			entry_name=$candidate_name
		else
			entry_name=provider-$index
		fi
	fi

	printf '%s\t%s\t127.0.0.1:%s\n' "$entry_name" "$profile_path" "$(allocate_port)" >>"$entries"
done <"$pending"

[ -s "$entries" ] || die "no providers to configure"

total=$(wc -l <"$entries" | tr -d ' ')
manifest_tmp=$work_dir/providers.json
{
	echo '{'
	echo '  "version": 1,'
	echo '  "providers": ['
	written=0
	while IFS="$tab" read -r entry_name entry_profile entry_listen; do
		written=$((written + 1))
		if [ "$written" -lt "$total" ]; then
			terminator=,
		else
			terminator=
		fi
		printf '    {"name": "%s", "profile": "%s", "listen": "%s"}%s\n' \
			"$entry_name" "$entry_profile" "$entry_listen" "$terminator"
	done <"$entries"
	echo '  ]'
	echo '}'
} >"$manifest_tmp"
install -m 0600 "$manifest_tmp" "$manifest"

# One argument list, rendered twice: as plist <string> elements and as a quoted
# systemd ExecStart. Keeping it in one place is what stops the two supervisors
# from drifting apart.
args_file=$work_dir/args
: >"$args_file"
add_arg() {
	printf '%s\n' "$1" >>"$args_file"
}
add_arg client
add_arg --providers
add_arg "$manifest"
add_arg --local-address
add_arg "$local_address"
if [ -n "$metrics_listen" ]; then
	add_arg --metrics-listen
	add_arg "$metrics_listen"
fi
add_arg --log-level
add_arg "$log_level"
add_arg --log-format
add_arg json
add_arg --telemetry-log-interval
add_arg 5s
if [ "$platform" = macos ]; then
	# launchd discards stderr unless a path is configured, so the rotating
	# JSON file is the only useful surface. journald keeps stderr on Linux.
	add_arg --log-stderr=false
fi

if [ "$platform" = macos ]; then
	agent_dir=$home/Library/LaunchAgents
	service_path=$agent_dir/$label.plist
	plist_tmp=$work_dir/agent.plist
	install -d -m 0755 "$agent_dir"
	{
		cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!-- Generated by deploy/install-client.sh. Re-run that script after changing
     providers; edits made here are overwritten. -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>

  <key>ProgramArguments</key>
  <array>
    <string>$binary_path</string>
EOF
		while IFS= read -r argument; do
			printf '    <string>%s</string>\n' "$argument"
		done <"$args_file"
		cat <<'EOF'
  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>

  <key>ProcessType</key>
  <string>Interactive</string>
</dict>
</plist>
EOF
	} >"$plist_tmp"
	install -m 0644 "$plist_tmp" "$service_path"

	if [ "$start_service" = true ]; then
		# bootout before bootstrap: kickstart restarts the definition launchd
		# already cached and would not re-read the arguments just written.
		launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
		launchctl bootstrap "gui/$(id -u)" "$service_path"
	fi
else
	unit_dir=$home/.config/systemd/user
	service_path=$unit_dir/$service_name.service
	unit_tmp=$work_dir/unit.service
	install -d -m 0755 "$unit_dir"

	exec_start="\"$binary_path\""
	while IFS= read -r argument; do
		exec_start="$exec_start \"$argument\""
	done <"$args_file"

	cat >"$unit_tmp" <<EOF
# Generated by deploy/install-client.sh. Re-run that script after changing
# providers; edits made here are overwritten.
[Unit]
Description=queqiao local SOCKS5 client
Documentation=https://github.com/bojieli/queqiao
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$exec_start
# The process exits when any provider's listener stops, so a partially working
# client never looks healthy. Restarting is what makes that safe.
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
PrivateTmp=true
LockPersonality=true
RestrictRealtime=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallArchitectures=native
LimitNOFILE=65536

[Install]
WantedBy=default.target
EOF
	install -m 0644 "$unit_tmp" "$service_path"

	if [ "$start_service" = true ]; then
		systemctl --user daemon-reload ||
			die "systemctl --user is unavailable in this session.
Log in on the desktop, or set XDG_RUNTIME_DIR for this account, then re-run."
		systemctl --user enable "$service_name" >/dev/null
		systemctl --user restart "$service_name"
		# Without lingering the user manager exists only while the account is
		# logged in, so the client would not come back after a reboot.
		if command -v loginctl >/dev/null 2>&1; then
			loginctl enable-linger "$(id -un)" >/dev/null 2>&1 ||
				echo "NOTE: could not enable lingering. Run 'sudo loginctl enable-linger $(id -un)' so the client starts at boot without a login." >&2
		fi
	fi
fi

have_nc=false
if command -v nc >/dev/null 2>&1; then
	have_nc=true
fi

if [ "$verify" = true ] && [ "$start_service" = true ]; then
	if [ "$have_nc" = false ]; then
		echo "NOTE: nc is unavailable; listener verification was skipped." >&2
	fi
	while IFS="$tab" read -r entry_name entry_profile entry_listen; do
		[ "$have_nc" = true ] || continue
		attempt=0
		listening=false
		while [ "$attempt" -lt 15 ]; do
			if nc -z "${entry_listen%:*}" "${entry_listen##*:}" >/dev/null 2>&1; then
				listening=true
				break
			fi
			sleep 1
			attempt=$((attempt + 1))
		done
		if [ "$listening" = false ]; then
			die "$entry_name never accepted a connection on $entry_listen.
Inspect the client log: $binary_path logs client"
		fi
		echo "$entry_name is listening on $entry_listen."

		if [ "$egress_check" = true ] && command -v curl >/dev/null 2>&1; then
			# NO_PROXY=* otherwise bypasses even an explicit --socks5-hostname
			# and produces a convincing but irrelevant result.
			echo "Checking egress for $entry_name through https://api.ipify.org ..."
			egress=$(env -u NO_PROXY -u no_proxy curl --noproxy '' -fsS --max-time 20 \
				--socks5-hostname "$entry_listen" https://api.ipify.org) ||
				die "$entry_name accepted the SOCKS connection but the request did not complete.
Skip this check with --no-egress-check, then read $binary_path logs client."
			echo "$entry_name egress address: $egress"
		fi
	done <"$entries"
fi

cat <<EOF

Queqiao client installed.
  binary    $binary_path
  manifest  $manifest
  service   $service_path
  logs      $binary_path logs client

Providers:
EOF
while IFS="$tab" read -r entry_name entry_profile entry_listen; do
	printf '  %-24s socks5h://%s\n' "$entry_name" "$entry_listen"
done <"$entries"

cat <<EOF

Point Clash or mihomo at the listeners above; deploy/clash-queqiao.yaml is a
starter profile. Add another provider later with:
  deploy/install-client.sh --invite 'queqiao://enroll/...'
EOF
