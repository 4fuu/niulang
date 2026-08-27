#!/bin/sh
# Interactive Linux installer and service console for Niulang protocol 2.
# POSIX sh is intentional: the same file works with dash and BusyBox ash.
set -eu

OFFICIAL_REPOSITORY=4fuu/niulang
REPOSITORY=${NIULANG_REPOSITORY:-$OFFICIAL_REPOSITORY}
RELEASE_INCLUDE_PRERELEASE=${NIULANG_INCLUDE_PRERELEASE:-false}
RELEASE_SOURCE_EXPLICIT=false
if [ "${NIULANG_REPOSITORY+x}" = x ] || [ "${NIULANG_INCLUDE_PRERELEASE+x}" = x ]; then
	RELEASE_SOURCE_EXPLICIT=true
fi
GITHUB_URL=${NIULANG_GITHUB_URL:-https://github.com}
GITHUB_API=${NIULANG_GITHUB_API:-https://api.github.com}
REQUESTED_VERSION=${NIULANG_VERSION:-latest}
SUPPLIED_BINARY=${NIULANG_BINARY:-}

SERVER_STATE=${NIULANG_STATE_DIR:-/var/lib/niulang/provider}
SERVER_USER=${NIULANG_SERVICE_USER:-niulang}
SERVER_GROUP=${NIULANG_SERVICE_GROUP:-$SERVER_USER}
SERVER_BINARY=${NIULANG_INSTALLED_BINARY:-/usr/local/bin/niulangd}
SERVER_SERVICE=${NIULANG_SERVER_SERVICE:-niulangd}
CLIENT_SERVICE=${NIULANG_CLIENT_SERVICE:-niulang-client}
CLIENT_BINARY=${NIULANG_CLIENT_BINARY:-${HOME:?HOME must be set}/.niulang/bin/niulangd}

INIT_SYSTEM=${NIULANG_INIT_SYSTEM:-}
CONFIG_DIR=${NIULANG_CONFIG_DIR:-/etc/niulang}
DATA_DIR=${NIULANG_DATA_DIR:-/var/lib/niulang}
LOG_DIR=${NIULANG_LOG_DIR:-/var/log/niulang}
ROLLBACK_DIR=${NIULANG_ROLLBACK_DIR:-/usr/local/lib/niulang/rollback}
PROFILE_PATH=${NIULANG_PROFILE_PATH:-$DATA_DIR/client.json}
PROVIDERS_PATH=${NIULANG_PROVIDERS_PATH:-$DATA_DIR/providers.json}
RELEASE_SOURCE_FILE=
NATIVE_BINARY=
RUNTIME_USER=$SERVER_USER
OPENWRT_NETWORK=${NIULANG_OPENWRT_NETWORK:-wan}

SERVER_PORT=443
SERVER_TRANSPORT=auto
SERVER_MAX_SESSIONS=4096
SERVER_METRICS_PORT=19090
SERVER_ALLOW_PRIVATE=false
CLIENT_PORT=12080
CLIENT_METRICS_PORT=12090
CLIENT_MAX_SESSIONS=2048
CLIENT_LOCAL_ADDRESS=auto
CLIENT_MODE=single

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
test_root=${NIULANG_MANAGER_ROOT:-}
if [ -n "$test_root" ] && [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
	echo "manage.sh: NIULANG_MANAGER_ROOT is reserved for tests" >&2
	exit 1
fi

TEMP_DIR=
INSTALL_BINARY=
INSTALL_DEPLOY=

info() {
	printf '\n[+] %s\n' "$*" >&2
}

warn() {
	printf '\n[!] %s\n' "$*" >&2
}

die() {
	printf '\n[错误] %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [ -n "${TEMP_DIR:-}" ] && [ -d "$TEMP_DIR" ]; then
		rm -rf "$TEMP_DIR"
	fi
}

ensure_temp() {
	if [ -z "$TEMP_DIR" ]; then
		TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/niulang-manager.XXXXXX") ||
			die "无法创建临时目录"
	fi
}

prompt() {
	prompt_text=$1
	prompt_default=${2:-}
	if [ -n "$prompt_default" ]; then
		printf '%s [%s]: ' "$prompt_text" "$prompt_default" >&2
	else
		printf '%s: ' "$prompt_text" >&2
	fi
	IFS= read -r prompt_answer || die "输入已中断"
	if [ -z "$prompt_answer" ]; then
		prompt_answer=$prompt_default
	fi
	printf '%s\n' "$prompt_answer"
}

confirm() {
	confirm_text=$1
	confirm_default=${2:-yes}
	while :; do
		if [ "$confirm_default" = yes ]; then
			printf '%s [Y/n]: ' "$confirm_text" >&2
		else
			printf '%s [y/N]: ' "$confirm_text" >&2
		fi
		IFS= read -r confirm_answer || die "输入已中断"
		if [ -z "$confirm_answer" ]; then
			confirm_answer=$confirm_default
		fi
		case $confirm_answer in
		y | Y | yes | YES | Yes) return 0 ;;
		n | N | no | NO | No) return 1 ;;
		esac
		printf '请输入 y 或 n。\n' >&2
	done
}

prompt_invitation() {
	prompt_hide=false
	if [ -t 0 ] && command -v stty >/dev/null 2>&1; then
		prompt_hide=true
	fi
	printf '粘贴新的 niulang://enroll/ 邀请链接: ' >&2
	if [ "$prompt_hide" = true ]; then
		prompt_stty=$(stty -g)
		trap 'stty "$prompt_stty" 2>/dev/null || true; printf "\n" >&2; exit 130' HUP INT TERM
		stty -echo
		if ! IFS= read -r prompt_answer; then
			stty "$prompt_stty" 2>/dev/null || true
			trap - HUP INT TERM
			die "输入已中断"
		fi
		stty "$prompt_stty"
		trap - HUP INT TERM
		printf '\n' >&2
	else
		IFS= read -r prompt_answer || die "输入已中断"
	fi
	valid_invitation "$prompt_answer" || die "必须使用新的 niulang://enroll/... 邀请链接"
	printf '%s\n' "$prompt_answer"
}

valid_invitation() {
	case $1 in
	niulang://enroll/?*) return 0 ;;
	*) return 1 ;;
	esac
}

valid_port() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ "$1" -ge 1 ] 2>/dev/null && [ "$1" -le 65535 ] 2>/dev/null
}

ask_port() {
	port_label=$1
	port_default=$2
	while :; do
		port_value=$(prompt "$port_label" "$port_default")
		if valid_port "$port_value"; then
			printf '%s\n' "$port_value"
			return
		fi
		warn "端口必须是 1-65535 的整数"
	done
}

ask_nonnegative_integer() {
	integer_label=$1
	integer_default=$2
	integer_max=$3
	while :; do
		integer_value=$(prompt "$integer_label" "$integer_default")
		case $integer_value in
		'' | *[!0-9]*) ;;
		*)
			if [ "$integer_value" -le "$integer_max" ] 2>/dev/null; then
				printf '%s\n' "$integer_value"
				return
			fi
			;;
		esac
		warn "请输入 0-$integer_max 的整数"
	done
}

ask_positive_integer() {
	integer_label=$1
	integer_default=$2
	integer_max=$3
	while :; do
		integer_value=$(prompt "$integer_label" "$integer_default")
		case $integer_value in
		'' | *[!0-9]*) ;;
		*)
			if [ "$integer_value" -ge 1 ] 2>/dev/null && [ "$integer_value" -le "$integer_max" ] 2>/dev/null; then
				printf '%s\n' "$integer_value"
				return
			fi
			;;
		esac
		warn "请输入 1-$integer_max 的整数"
	done
}

valid_token() {
	case $1 in
	'' | *[!A-Za-z0-9_.:-]*) return 1 ;;
	*) return 0 ;;
	esac
}

valid_local_address() {
	case $1 in
	'' | *[!A-Za-z0-9_.:%-]*) return 1 ;;
	esac
	case $1 in
	auto | if:?*) return 0 ;;
	if:*) return 1 ;;
	*.* | *:*) return 0 ;;
	*) return 1 ;;
	esac
}

root_path() {
	printf '%s%s\n' "$test_root" "$1"
}

detect_init_system() {
	if [ -z "$INIT_SYSTEM" ]; then
		if [ -f "$(root_path /etc/openwrt_release)" ] && [ -x "$(root_path /sbin/procd)" ]; then
			INIT_SYSTEM=openwrt
		elif command -v systemctl >/dev/null 2>&1 && [ -d "$(root_path /run/systemd/system)" ]; then
			INIT_SYSTEM=systemd
		elif command -v rc-service >/dev/null 2>&1 && command -v openrc-run >/dev/null 2>&1; then
			INIT_SYSTEM=openrc
		else
			INIT_SYSTEM=unknown
		fi
	fi

	case $INIT_SYSTEM in
	openwrt)
		CONFIG_DIR=${NIULANG_CONFIG_DIR:-/etc/niulang}
		DATA_DIR=${NIULANG_DATA_DIR:-$CONFIG_DIR}
		LOG_DIR=${NIULANG_LOG_DIR:-/tmp/niulang}
		ROLLBACK_DIR=${NIULANG_ROLLBACK_DIR:-/usr/lib/niulang/rollback}
		NATIVE_BINARY=${NIULANG_INSTALLED_BINARY:-/usr/bin/niulangd}
		RUNTIME_USER=${NIULANG_SERVICE_USER:-root}
		SERVER_USER=$RUNTIME_USER
		SERVER_GROUP=${NIULANG_SERVICE_GROUP:-$RUNTIME_USER}
		;;
	openrc)
		NATIVE_BINARY=${NIULANG_INSTALLED_BINARY:-/usr/local/bin/niulangd}
		RUNTIME_USER=${NIULANG_SERVICE_USER:-niulang}
		SERVER_USER=$RUNTIME_USER
		SERVER_GROUP=${NIULANG_SERVICE_GROUP:-$RUNTIME_USER}
		;;
	systemd) ;;
	unknown) ;;
	*) die "无效的 NIULANG_INIT_SYSTEM: $INIT_SYSTEM" ;;
	esac

	if [ "$INIT_SYSTEM" = openrc ] || [ "$INIT_SYSTEM" = openwrt ]; then
		SERVER_STATE=${NIULANG_STATE_DIR:-$DATA_DIR/provider}
		PROFILE_PATH=${NIULANG_PROFILE_PATH:-$DATA_DIR/client.json}
		PROVIDERS_PATH=${NIULANG_PROVIDERS_PATH:-$DATA_DIR/providers.json}
		SERVER_BINARY=$NATIVE_BINARY
		CLIENT_BINARY=$NATIVE_BINARY
	fi
	RELEASE_SOURCE_FILE=${NIULANG_RELEASE_SOURCE_FILE:-$CONFIG_DIR/release-source}
}

require_supported_init() {
	[ "$(uname -s)" = Linux ] || die "管理脚本仅支持 Linux；macOS 客户端请使用 deploy/install-client.sh"
	map_arch "$(uname -m)" >/dev/null || die "Linux 管理器仅支持 amd64 和 arm64"
	detect_init_system
	case $INIT_SYSTEM in
	systemd | openrc | openwrt) ;;
	*) die "无法识别服务管理器；支持 systemd、OpenRC 和 OpenWrt procd" ;;
	esac
}

run_root() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	elif command -v sudo >/dev/null 2>&1; then
		sudo -- "$@"
	else
		die "该操作需要 root 权限，请安装 sudo 或以 root 运行"
	fi
}

valid_repository() {
	repository_value=$1
	case $repository_value in
	'' | /* | */ | *[!A-Za-z0-9._/-]* | *//* ) return 1 ;;
	esac
	repository_owner=${repository_value%%/*}
	repository_name=${repository_value#*/}
	[ "$repository_name" != "$repository_value" ] || return 1
	[ -n "$repository_owner" ] && [ -n "$repository_name" ] || return 1
	case $repository_name in
	*/*) return 1 ;;
	esac
	return 0
}

normalize_boolean() {
	case $1 in
	1 | true | TRUE | yes | YES) printf 'true\n' ;;
	0 | false | FALSE | no | NO) printf 'false\n' ;;
	*) return 1 ;;
	esac
}

set_release_source() {
	release_repository=$1
	release_prerelease=$2
	valid_repository "$release_repository" || return 1
	release_prerelease=$(normalize_boolean "$release_prerelease") || return 1
	REPOSITORY=$release_repository
	RELEASE_INCLUDE_PRERELEASE=$release_prerelease
}

release_source_description() {
	if [ "$RELEASE_INCLUDE_PRERELEASE" = true ]; then
		printf '%s（含预发行版）\n' "$REPOSITORY"
	else
		printf '%s（仅稳定版）\n' "$REPOSITORY"
	fi
}

load_saved_release_source() {
	valid_repository "$REPOSITORY" || die "无效 Release 仓库: $REPOSITORY"
	RELEASE_INCLUDE_PRERELEASE=$(normalize_boolean "$RELEASE_INCLUDE_PRERELEASE") ||
		die "NIULANG_INCLUDE_PRERELEASE 必须是 true/false"
	[ "$RELEASE_SOURCE_EXPLICIT" = false ] || return 0
	release_source_path=$(root_path "$RELEASE_SOURCE_FILE")
	[ -r "$release_source_path" ] || return 0
	saved_repository=$(sed -n 's/^repository=//p' "$release_source_path" | sed -n '1p')
	saved_prerelease=$(sed -n 's/^include_prerelease=//p' "$release_source_path" | sed -n '1p')
	if ! set_release_source "$saved_repository" "$saved_prerelease"; then
		warn "忽略无效的来源配置 $release_source_path"
		set_release_source "$OFFICIAL_REPOSITORY" false
	fi
}

save_release_source() {
	ensure_temp
	release_source_path=$(root_path "$RELEASE_SOURCE_FILE")
	release_source_temp=$TEMP_DIR/release-source
	{
		printf 'repository=%s\n' "$REPOSITORY"
		printf 'include_prerelease=%s\n' "$RELEASE_INCLUDE_PRERELEASE"
	} >"$release_source_temp"
	chmod 0644 "$release_source_temp"
	run_root mkdir -p "$(dirname "$release_source_path")"
	run_root cp "$release_source_temp" "$release_source_path.new.$$"
	run_root chmod 0644 "$release_source_path.new.$$"
	run_root mv "$release_source_path.new.$$" "$release_source_path"
}

choose_release_source() {
	cat >&2 <<EOF

当前 Release 来源: $(release_source_description)
  1) 官方 $OFFICIAL_REPOSITORY（仅稳定版）
  2) 官方 $OFFICIAL_REPOSITORY（包含预发行版）
  3) 自定义 Niulang owner/repository
  0) 保持当前来源
EOF
	release_choice=$(prompt "选择下载来源" 0)
	case $release_choice in
	1) set_release_source "$OFFICIAL_REPOSITORY" false ;;
	2) set_release_source "$OFFICIAL_REPOSITORY" true ;;
	3)
		release_custom=$(prompt "GitHub 仓库（owner/repository）" "")
		valid_repository "$release_custom" || die "无效 GitHub 仓库: $release_custom"
		if confirm "latest 是否允许选择预发行版？" no; then
			release_custom_prerelease=true
		else
			release_custom_prerelease=false
		fi
		set_release_source "$release_custom" "$release_custom_prerelease"
		;;
	0) return 0 ;;
	*) die "未知的下载来源选项: $release_choice" ;;
	esac
	save_release_source
	info "Release 来源已切换为 $(release_source_description)"
}

map_arch() {
	case $1 in
	x86_64 | amd64) printf 'amd64\n' ;;
	aarch64 | arm64) printf 'arm64\n' ;;
	*) return 1 ;;
	esac
}

validate_version() {
	case $1 in
	v[0-9]*) ;;
	*) return 1 ;;
	esac
	case $1 in
	*[!A-Za-z0-9._-]*) return 1 ;;
	esac
	return 0
}

release_asset() {
	release_asset_version=$1
	release_asset_arch=$2
	printf 'niulangd_%s_linux_%s.tar.gz\n' "$release_asset_version" "$release_asset_arch"
}

fetch_file() {
	fetch_url=$1
	fetch_output=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fL --retry 3 --connect-timeout 15 -o "$fetch_output" "$fetch_url"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$fetch_output" "$fetch_url"
	else
		die "需要 curl 或 wget 下载 Release"
	fi
}

sha256_file() {
	sha_path=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$sha_path" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$sha_path" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$sha_path" | sed 's/^.*= //'
	else
		die "需要 sha256sum、shasum 或 openssl 验证 Release"
	fi
}

latest_version() {
	ensure_temp
	if [ "$RELEASE_INCLUDE_PRERELEASE" = true ]; then
		latest_endpoint="$GITHUB_API/repos/$REPOSITORY/releases?per_page=1"
	else
		latest_endpoint="$GITHUB_API/repos/$REPOSITORY/releases/latest"
	fi
	fetch_file "$latest_endpoint" "$TEMP_DIR/latest.json" >&2
	latest_tag=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		"$TEMP_DIR/latest.json" | sed -n '1p')
	validate_version "$latest_tag" || die "无法从 GitHub API 解析 $REPOSITORY 的最新可选版本"
	printf '%s\n' "$latest_tag"
}

validate_protocol_binary() {
	protocol_binary=$1
	[ -x "$protocol_binary" ] || die "找不到可执行文件 $protocol_binary"
	protocol_version=$($protocol_binary version) || die "$protocol_binary 无法在本机运行"
	case $protocol_version in
	*wire=2*) ;;
	*) die "拒绝安装非 Niulang protocol 2 二进制: $protocol_version" ;;
	esac
	printf '%s\n' "$protocol_version" >&2
}

download_release() {
	ensure_temp
	command -v tar >/dev/null 2>&1 || die "需要 tar 解压 Linux Release"
	download_arch=$(map_arch "$(uname -m)") ||
		die "Linux Release 仅提供 amd64 和 arm64，当前架构为 $(uname -m)"
	download_version=$REQUESTED_VERSION
	if [ "$download_version" = latest ]; then
		download_version=$(latest_version)
	fi
	validate_version "$download_version" || die "无效 Release 版本: $download_version"
	download_asset=$(release_asset "$download_version" "$download_arch")
	download_base="$GITHUB_URL/$REPOSITORY/releases/download/$download_version"

	info "下载并验证 $REPOSITORY $download_version"
	fetch_file "$download_base/SHA256SUMS" "$TEMP_DIR/SHA256SUMS"
	fetch_file "$download_base/$download_asset" "$TEMP_DIR/$download_asset"
	download_expected=$(awk -v name="$download_asset" '$2 == name || $2 == "*" name { print $1; exit }' \
		"$TEMP_DIR/SHA256SUMS")
	case $download_expected in
	????????????????????????????????????????????????????????????????) ;;
	*) die "SHA256SUMS 中没有 $download_asset" ;;
	esac
	download_actual=$(sha256_file "$TEMP_DIR/$download_asset")
	[ "$download_actual" = "$download_expected" ] || die "Release SHA-256 校验失败"

	download_root=${download_asset%.tar.gz}
	tar -xzf "$TEMP_DIR/$download_asset" -C "$TEMP_DIR" \
		"$download_root/niulangd" \
		"$download_root/deploy/install-client.sh" \
		"$download_root/deploy/install-server.sh" \
		"$download_root/deploy/niulangd.service" \
		"$download_root/deploy/tune-server.sh"
	INSTALL_BINARY=$TEMP_DIR/$download_root/niulangd
	INSTALL_DEPLOY=$TEMP_DIR/$download_root/deploy
	validate_protocol_binary "$INSTALL_BINARY"
}

prepare_install_source() {
	if [ -n "$INSTALL_BINARY" ]; then
		return
	fi
	if [ -n "$SUPPLIED_BINARY" ]; then
		INSTALL_BINARY=$SUPPLIED_BINARY
		INSTALL_DEPLOY=$script_dir
	elif [ -x "$repo_root/niulangd" ]; then
		INSTALL_BINARY=$repo_root/niulangd
		INSTALL_DEPLOY=$script_dir
	else
		download_release
		return
	fi
	[ -x "$INSTALL_DEPLOY/install-client.sh" ] || die "缺少 install-client.sh"
	[ -x "$INSTALL_DEPLOY/install-server.sh" ] || die "缺少 install-server.sh"
	[ -f "$INSTALL_DEPLOY/niulangd.service" ] || die "缺少 niulangd.service"
	validate_protocol_binary "$INSTALL_BINARY"
}

argument_value() {
	argument_text=$1
	argument_name=$2
	printf '%s\n' "$argument_text" | awk -v wanted="--$argument_name" '
		{ for (i = 1; i < NF; i++) if ($i == wanted) { print $(i + 1); exit } }
	'
}

existing_server_arguments() {
	env_path=$(root_path /etc/niulang/niulangd.env)
	if [ -r "$env_path" ]; then
		cat "$env_path"
	else
		run_root cat "$env_path" 2>/dev/null
	fi | sed -n 's/^NIULANGD_ARGS=//p' | sed -n '1p'
}

server_state_exists() {
	state_path=$1
	case $state_path in
	"$test_root"/*) state_full=$state_path ;;
	*) state_full=$(root_path "$state_path") ;;
	esac
	[ -d "$state_full" ] || run_root test -d "$state_full"
}

native_service_name() {
	printf 'niulang-%s\n' "$1"
}

native_service_path() {
	printf '%s/etc/init.d/%s\n' "$test_root" "$(native_service_name "$1")"
}

native_service_installed() {
	[ -f "$(native_service_path "$1")" ]
}

safe_native_path() {
	case $1 in
	/*) ;;
	*) return 1 ;;
	esac
	case $1 in
	*[!A-Za-z0-9/._@+:-]*) return 1 ;;
	esac
	return 0
}

validate_native_paths() {
	for native_path in "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" "$ROLLBACK_DIR" \
		"$SERVER_STATE" "$PROFILE_PATH" "$PROVIDERS_PATH" "$NATIVE_BINARY"; do
		safe_native_path "$native_path" || die "OpenRC/OpenWrt 路径必须是无空格的安全绝对路径: $native_path"
	done
}

shell_quote() {
	printf "'"
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}

run_as_runtime() {
	if [ "$RUNTIME_USER" = root ]; then
		run_root "$@"
	elif [ "$(id -u)" -eq 0 ] && command -v runuser >/dev/null 2>&1; then
		runuser -u "$RUNTIME_USER" -- "$@"
	elif command -v su-exec >/dev/null 2>&1; then
		su-exec "$RUNTIME_USER:$SERVER_GROUP" "$@"
	elif command -v sudo >/dev/null 2>&1; then
		sudo -u "$RUNTIME_USER" -- "$@"
	elif [ "$(id -u)" -eq 0 ] && command -v su >/dev/null 2>&1; then
		runtime_command=
		for runtime_argument in "$@"; do
			runtime_quoted=$(shell_quote "$runtime_argument")
			runtime_command="$runtime_command $runtime_quoted"
		done
		su -s /bin/sh "$RUNTIME_USER" -c "exec$runtime_command"
	else
		die "需要 runuser、su-exec、sudo 或 su 以 $RUNTIME_USER 身份运行 provider 命令"
	fi
}

create_native_runtime() {
	validate_native_paths
	if [ "$RUNTIME_USER" != root ] && ! id "$RUNTIME_USER" >/dev/null 2>&1; then
		if [ "${NIULANG_MANAGER_TESTING:-0}" = 1 ]; then
			:
		elif command -v useradd >/dev/null 2>&1; then
			run_root useradd --system --user-group --home-dir "$DATA_DIR" --shell /sbin/nologin "$RUNTIME_USER"
		elif command -v adduser >/dev/null 2>&1; then
			run_root adduser -S -D -H -h "$DATA_DIR" -s /sbin/nologin "$RUNTIME_USER"
		else
			die "无法创建 $RUNTIME_USER 服务账号：缺少 useradd/adduser"
		fi
	fi
	for native_dir in "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"; do
		run_root mkdir -p "$(root_path "$native_dir")"
	done
	run_root chmod 0750 "$(root_path "$CONFIG_DIR")" "$(root_path "$LOG_DIR")"
	run_root chmod 0700 "$(root_path "$DATA_DIR")"
	if [ "$RUNTIME_USER" != root ] && [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
		run_root chown -R "$RUNTIME_USER:$SERVER_GROUP" "$(root_path "$DATA_DIR")" "$(root_path "$LOG_DIR")"
	fi
}

install_native_binary() {
	prepare_install_source
	validate_native_paths
	ensure_temp
	native_binary_path=$(root_path "$NATIVE_BINARY")
	native_new_sum=$(sha256_file "$INSTALL_BINARY")
	NATIVE_BINARY_CHANGED=false
	NATIVE_BINARY_BACKUP=
	if [ -x "$native_binary_path" ]; then
		native_old_sum=$(sha256_file "$native_binary_path")
		if [ "$native_old_sum" = "$native_new_sum" ]; then
			info "已安装相同的 Niulang 二进制"
			return
		fi
		native_stamp=$(date -u +%Y%m%dT%H%M%SZ)
		NATIVE_BINARY_BACKUP=$(root_path "$ROLLBACK_DIR/niulangd-$native_stamp-$$")
		run_root mkdir -p "$(dirname "$NATIVE_BINARY_BACKUP")"
		run_root cp -p "$native_binary_path" "$NATIVE_BINARY_BACKUP"
	fi
	run_root mkdir -p "$(dirname "$native_binary_path")"
	run_root cp "$INSTALL_BINARY" "$native_binary_path.new.$$"
	run_root chmod 0755 "$native_binary_path.new.$$"
	run_root mv "$native_binary_path.new.$$" "$native_binary_path"
	NATIVE_BINARY_CHANGED=true
	info "已安装 $NATIVE_BINARY"
}

restore_native_binary() {
	[ "${NATIVE_BINARY_CHANGED:-false}" = true ] || return 0
	native_binary_path=$(root_path "$NATIVE_BINARY")
	if [ -n "${NATIVE_BINARY_BACKUP:-}" ] && [ -f "$NATIVE_BINARY_BACKUP" ]; then
		run_root cp -p "$NATIVE_BINARY_BACKUP" "$native_binary_path.restore.$$"
		run_root mv "$native_binary_path.restore.$$" "$native_binary_path"
	else
		run_root rm -f "$native_binary_path"
	fi
}

native_service_is_active() {
	native_active_name=$(native_service_name "$1")
	case $INIT_SYSTEM in
	openrc) rc-service "$native_active_name" status >/dev/null 2>&1 ;;
	openwrt)
		ubus call service list "{\"name\":\"$native_active_name\"}" 2>/dev/null |
			grep -q '"running"[[:space:]]*:[[:space:]]*true'
		;;
	esac
}

native_stop_service() {
	native_stop_role=$1
	native_stop_name=$(native_service_name "$native_stop_role")
	native_stop_path=$(native_service_path "$native_stop_role")
	case $INIT_SYSTEM in
	openrc) rc-service "$native_stop_name" stop >/dev/null 2>&1 || true ;;
	openwrt) [ ! -x "$native_stop_path" ] || "$native_stop_path" stop >/dev/null 2>&1 || true ;;
	esac
}

native_start_service() {
	native_start_role=$1
	native_start_name=$(native_service_name "$native_start_role")
	native_start_path=$(native_service_path "$native_start_role")
	case $INIT_SYSTEM in
	openrc)
		rc-update add "$native_start_name" default >/dev/null
		if rc-service "$native_start_name" status >/dev/null 2>&1; then
			rc-service "$native_start_name" restart
		else
			rc-service "$native_start_name" start
		fi
		;;
	openwrt)
		"$native_start_path" enable
		"$native_start_path" restart
		;;
	esac
}

ensure_native_server_capability() {
	[ "$INIT_SYSTEM" = openrc ] && [ "$SERVER_PORT" -lt 1024 ] || return 0
	[ "${NIULANG_MANAGER_TESTING:-0}" != 1 ] || return 0
	command -v setcap >/dev/null 2>&1 ||
		die "OpenRC 监听低端口需要 setcap/libcap，或改用 1024 以上端口"
	run_root setcap cap_net_bind_service=+ep "$(root_path "$NATIVE_BINARY")"
}

openwrt_ipv4() {
	openwrt_logical=$1
	openwrt_network_script=$(root_path /lib/functions/network.sh)
	[ -r "$openwrt_network_script" ] || return 1
	__NETWORK_CACHE=${__NETWORK_CACHE:-}
	# This script and network_get_ipaddr are provided by OpenWrt.
	# shellcheck disable=SC1090
	. "$openwrt_network_script"
	openwrt_address=
	network_get_ipaddr openwrt_address "$openwrt_logical" || return 1
	[ -n "$openwrt_address" ] || return 1
	printf '%s\n' "$openwrt_address"
}

verify_native_service() {
	verify_native_role=$1
	if [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
		sleep 1
	fi
	if native_service_is_active "$verify_native_role"; then
		return 0
	fi
	if [ "$INIT_SYSTEM" = openwrt ] && [ "$verify_native_role" = client ]; then
		case $CLIENT_LOCAL_ADDRESS in
		openwrt:*)
			if ! openwrt_ipv4 "${CLIENT_LOCAL_ADDRESS#openwrt:}" >/dev/null 2>&1; then
				warn "WAN 当前没有 IPv4；procd 会在接口获得地址后启动客户端"
				return 0
			fi
			;;
		esac
	fi
	warn "$(native_service_name "$verify_native_role") 启动后未保持运行"
	return 1
}

native_server_arguments() {
	native_server_log=$LOG_DIR/server.log
	[ "$INIT_SYSTEM" != openwrt ] || native_server_log=none
	SERVER_ARGS="--state $SERVER_STATE --listen :$SERVER_PORT --transport $SERVER_TRANSPORT --max-sessions $SERVER_MAX_SESSIONS --metrics-listen 127.0.0.1:$SERVER_METRICS_PORT --log-level info --log-format json --log-file $native_server_log --telemetry-log-interval 5s"
	if [ "$SERVER_ALLOW_PRIVATE" = true ]; then
		SERVER_ARGS="$SERVER_ARGS --allow-private-destinations"
	fi
	printf '%s\n' "$SERVER_ARGS"
}

native_client_arguments() {
	native_client_log=$LOG_DIR/client.log
	[ "$INIT_SYSTEM" != openwrt ] || native_client_log=none
	if [ "$CLIENT_MODE" = multi ]; then
		CLIENT_ARGS="--providers $PROVIDERS_PATH --local-address $CLIENT_LOCAL_ADDRESS --max-sessions $CLIENT_MAX_SESSIONS --metrics-listen 127.0.0.1:$CLIENT_METRICS_PORT"
	else
		CLIENT_ARGS="--profile $PROFILE_PATH --listen 127.0.0.1:$CLIENT_PORT --local-address $CLIENT_LOCAL_ADDRESS --max-sessions $CLIENT_MAX_SESSIONS --metrics-listen 127.0.0.1:$CLIENT_METRICS_PORT"
	fi
	CLIENT_ARGS="$CLIENT_ARGS --log-level info --log-format json --log-file $native_client_log --telemetry-log-interval 5s"
	printf '%s\n' "$CLIENT_ARGS"
}

render_openrc_service() {
	render_native_role=$1
	render_native_args=$2
	cat <<EOF
#!/sbin/openrc-run
name="Niulang $render_native_role"
description="Niulang $render_native_role service"
command="$NATIVE_BINARY"
command_args="$render_native_role $render_native_args"
command_user="$RUNTIME_USER:$SERVER_GROUP"
supervisor=supervise-daemon
respawn_delay=2
respawn_max=0

depend() {
	need net
	after firewall
}
EOF
}

render_openwrt_service() {
	render_native_role=$1
	cat <<EOF
#!/bin/sh /etc/rc.common

START=95
STOP=10
USE_PROCD=1
PROG=$NATIVE_BINARY

start_service() {
EOF
	if [ "$render_native_role" = client ]; then
		cat <<EOF
	local local_address="$CLIENT_LOCAL_ADDRESS"
	if [ "\$local_address" = "openwrt:$OPENWRT_NETWORK" ]; then
		. /lib/functions/network.sh
		if ! network_get_ipaddr local_address "$OPENWRT_NETWORK"; then
			logger -t niulang "logical interface $OPENWRT_NETWORK has no IPv4 address; waiting for interface trigger"
			return 0
		fi
	fi
EOF
	fi
	cat <<EOF
	procd_open_instance
	procd_set_param command "\$PROG" "$render_native_role"
EOF
	if [ "$render_native_role" = server ]; then
		cat <<EOF
	procd_append_param command --state "$SERVER_STATE"
	procd_append_param command --listen ":$SERVER_PORT"
	procd_append_param command --transport "$SERVER_TRANSPORT"
	procd_append_param command --max-sessions "$SERVER_MAX_SESSIONS"
	procd_append_param command --metrics-listen "127.0.0.1:$SERVER_METRICS_PORT"
EOF
		if [ "$SERVER_ALLOW_PRIVATE" = true ]; then
			printf '\tprocd_append_param command --allow-private-destinations\n'
		fi
	else
		if [ "$CLIENT_MODE" = multi ]; then
			cat <<EOF
	procd_append_param command --providers "$PROVIDERS_PATH"
EOF
		else
			cat <<EOF
	procd_append_param command --profile "$PROFILE_PATH"
	procd_append_param command --listen "127.0.0.1:$CLIENT_PORT"
EOF
		fi
		cat <<EOF
	procd_append_param command --local-address "\$local_address"
	procd_append_param command --max-sessions "$CLIENT_MAX_SESSIONS"
	procd_append_param command --metrics-listen "127.0.0.1:$CLIENT_METRICS_PORT"
EOF
	fi
	cat <<'EOF'
	procd_append_param command --log-level info
	procd_append_param command --log-format json
	procd_append_param command --log-file none
	procd_append_param command --log-stderr=true
	procd_append_param command --telemetry-log-interval 5s
	procd_set_param respawn
	procd_set_param limits nofile="65536 65536"
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
EOF
	if [ "$render_native_role" = client ] && [ "$CLIENT_LOCAL_ADDRESS" = "openwrt:$OPENWRT_NETWORK" ]; then
		cat <<EOF

service_triggers() {
	procd_add_interface_trigger "interface.*" "$OPENWRT_NETWORK" /etc/init.d/niulang-client restart
}
EOF
	fi
}

write_native_service() {
	write_native_role=$1
	write_native_args=$2
	ensure_temp
	write_native_path=$(native_service_path "$write_native_role")
	write_native_stage=$TEMP_DIR/$(native_service_name "$write_native_role")
	case $INIT_SYSTEM in
	openrc) render_openrc_service "$write_native_role" "$write_native_args" >"$write_native_stage" ;;
	openwrt) render_openwrt_service "$write_native_role" >"$write_native_stage" ;;
	esac
	chmod 0755 "$write_native_stage"
	if [ "$write_native_role" = server ]; then
		ensure_native_server_capability
	fi
	write_native_had_service=false
	if [ -f "$write_native_path" ]; then
		write_native_had_service=true
		cp -p "$write_native_path" "$write_native_stage.backup"
		native_stop_service "$write_native_role"
	fi
	run_root mkdir -p "$(dirname "$write_native_path")"
	run_root cp "$write_native_stage" "$write_native_path.new.$$"
	run_root chmod 0755 "$write_native_path.new.$$"
	run_root mv "$write_native_path.new.$$" "$write_native_path"
	if native_start_service "$write_native_role" && verify_native_service "$write_native_role"; then
		info "已写入并启动 $(native_service_name "$write_native_role")（$INIT_SYSTEM）"
		return 0
	fi
	warn "新服务未通过验证，正在恢复旧服务定义"
	native_stop_service "$write_native_role"
	if [ "$write_native_had_service" = true ]; then
		run_root cp -p "$write_native_stage.backup" "$write_native_path"
		native_start_service "$write_native_role" >/dev/null 2>&1 || true
	else
		run_root rm -f "$write_native_path"
	fi
	return 1
}

route_hint() {
	command -v ip >/dev/null 2>&1 || return 1
	route_line=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n '1p')
	[ -n "$route_line" ] || return 1
	route_device=$(printf '%s\n' "$route_line" | awk '{ for (i=1; i<=NF; i++) if ($i == "dev") { print $(i+1); exit } }')
	route_source=$(printf '%s\n' "$route_line" | awk '{ for (i=1; i<=NF; i++) if ($i == "src") { print $(i+1); exit } }')
	[ -n "$route_device" ] && [ -n "$route_source" ] || return 1
	printf '%s %s\n' "$route_device" "$route_source"
}

choose_native_client_binding() {
	if [ "$INIT_SYSTEM" = openwrt ]; then
		while :; do
			OPENWRT_NETWORK=$(prompt "OpenWrt 逻辑出站接口" "$OPENWRT_NETWORK")
			valid_token "$OPENWRT_NETWORK" && break
			warn "接口名只能包含字母、数字、点、下划线、冒号和连字符"
		done
		CLIENT_LOCAL_ADDRESS="openwrt:$OPENWRT_NETWORK"
		if enrollment_address=$(openwrt_ipv4 "$OPENWRT_NETWORK"); then
			info "$OPENWRT_NETWORK 当前 IPv4 为 $enrollment_address；地址变化时 procd 会重启客户端"
		else
			warn "$OPENWRT_NETWORK 当前没有 IPv4"
			enrollment_address=$(prompt "本次注册的 --local-address" auto)
		fi
	else
		if route_value=$(route_hint); then
			info "当前 IPv4 路由提示：接口 ${route_value%% *}，源地址 ${route_value#* }"
		fi
		while :; do
			CLIENT_LOCAL_ADDRESS=$(prompt "外层连接绑定（auto、if:接口名或 IP）" "$CLIENT_LOCAL_ADDRESS")
			valid_local_address "$CLIENT_LOCAL_ADDRESS" && break
			warn "请输入 auto、if:eth0 或字面量 IP"
		done
		enrollment_address=$CLIENT_LOCAL_ADDRESS
	fi
}

native_provider_endpoint() {
	native_provider_file=$(root_path "$SERVER_STATE/provider.json")
	[ -r "$native_provider_file" ] || return 1
	sed -n 's/^.*"endpoint"[[:space:]]*:[[:space:]]*"\([^"]*\)".*$/\1/p' \
		"$native_provider_file" | sed -n '1p'
}

native_provider_endpoint_port() {
	native_endpoint=$(native_provider_endpoint) || return 1
	native_endpoint_port=${native_endpoint##*:}
	valid_port "$native_endpoint_port" || return 1
	printf '%s\n' "$native_endpoint_port"
}

native_provider_command() {
	run_as_runtime "$(root_path "$NATIVE_BINARY")" provider "$@" --state "$SERVER_STATE"
}

openwrt_service_parameter() {
	openwrt_role=$1
	openwrt_parameter=$2
	sed -n "s|^[[:space:]]*procd_append_param command --$openwrt_parameter \"\([^\"]*\)\".*$|\1|p" \
		"$(native_service_path "$openwrt_role")" 2>/dev/null | sed -n '1p'
}

openrc_service_arguments() {
	openrc_role=$1
	sed -n "s|^command_args=\"$openrc_role \\(.*\\)\"$|\\1|p" \
		"$(native_service_path "$openrc_role")" 2>/dev/null | sed -n '1p'
}

load_native_server_configuration() {
	if [ "$INIT_SYSTEM" = openwrt ]; then
		native_server_listen=$(openwrt_service_parameter server listen || true)
		native_server_transport=$(openwrt_service_parameter server transport || true)
		native_server_max=$(openwrt_service_parameter server max-sessions || true)
		native_server_metrics=$(openwrt_service_parameter server metrics-listen || true)
		grep -q -- '--allow-private-destinations' "$(native_service_path server)" 2>/dev/null &&
			SERVER_ALLOW_PRIVATE=true || SERVER_ALLOW_PRIVATE=false
	else
		native_server_current=$(openrc_service_arguments server || true)
		native_server_listen=$(argument_value "$native_server_current" listen)
		native_server_transport=$(argument_value "$native_server_current" transport)
		native_server_max=$(argument_value "$native_server_current" max-sessions)
		native_server_metrics=$(argument_value "$native_server_current" metrics-listen)
		case " $native_server_current " in
		*' --allow-private-destinations '*) SERVER_ALLOW_PRIVATE=true ;;
		*) SERVER_ALLOW_PRIVATE=false ;;
		esac
	fi
	if [ -n "$native_server_listen" ]; then
		native_server_port=${native_server_listen##*:}
		valid_port "$native_server_port" && SERVER_PORT=$native_server_port
	fi
	case $native_server_transport in
	auto | quic | tcp) SERVER_TRANSPORT=$native_server_transport ;;
	esac
	case $native_server_max in
	'' | *[!0-9]*) ;;
	*) SERVER_MAX_SESSIONS=$native_server_max ;;
	esac
	if [ -n "$native_server_metrics" ]; then
		native_server_metrics_port=${native_server_metrics##*:}
		valid_port "$native_server_metrics_port" && SERVER_METRICS_PORT=$native_server_metrics_port
	fi
}

load_native_client_configuration() {
	CLIENT_MODE=single
	if [ "$INIT_SYSTEM" = openwrt ]; then
		existing_providers=$(openwrt_service_parameter client providers || true)
		existing_profile=$(openwrt_service_parameter client profile || true)
		existing_listen=$(openwrt_service_parameter client listen || true)
		existing_local=$(sed -n 's/^[[:space:]]*local local_address="\([^"]*\)".*$/\1/p' \
			"$(native_service_path client)" 2>/dev/null | sed -n '1p')
		existing_max=$(openwrt_service_parameter client max-sessions || true)
		existing_metrics=$(openwrt_service_parameter client metrics-listen || true)
	else
		existing_arguments=$(openrc_service_arguments client || true)
		existing_providers=$(argument_value "$existing_arguments" providers)
		existing_profile=$(argument_value "$existing_arguments" profile)
		existing_listen=$(argument_value "$existing_arguments" listen)
		existing_local=$(argument_value "$existing_arguments" local-address)
		existing_max=$(argument_value "$existing_arguments" max-sessions)
		existing_metrics=$(argument_value "$existing_arguments" metrics-listen)
	fi
	if [ -n "$existing_providers" ]; then
		CLIENT_MODE=multi
		PROVIDERS_PATH=$existing_providers
	elif [ -n "$existing_profile" ]; then
		PROFILE_PATH=$existing_profile
	fi
	if [ -n "$existing_listen" ]; then
		existing_port=${existing_listen##*:}
		valid_port "$existing_port" && CLIENT_PORT=$existing_port
	fi
	case ${existing_local:-} in
	openwrt:?*) CLIENT_LOCAL_ADDRESS=$existing_local; OPENWRT_NETWORK=${existing_local#openwrt:} ;;
	*) valid_local_address "${existing_local:-}" && CLIENT_LOCAL_ADDRESS=$existing_local || true ;;
	esac
	case ${existing_max:-} in
	'' | *[!0-9]*) ;;
	*) CLIENT_MAX_SESSIONS=$existing_max ;;
	esac
	if [ -n "${existing_metrics:-}" ]; then
		existing_metrics_port=${existing_metrics##*:}
		valid_port "$existing_metrics_port" && CLIENT_METRICS_PORT=$existing_metrics_port
	fi
}

tune_native_server() {
	command -v sysctl >/dev/null 2>&1 || {
		warn "未找到 sysctl，跳过服务器网络队列调优"
		return 0
	}
	ensure_temp
	native_sysctl=$(root_path /etc/sysctl.d/90-niulang-performance.conf)
	native_sysctl_stage=$TEMP_DIR/90-niulang-performance.conf
	cat >"$native_sysctl_stage" <<'EOF'
# Niulang provider socket queues.
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.netdev_max_backlog = 16384
net.core.somaxconn = 8192
net.ipv4.tcp_max_syn_backlog = 8192
EOF
	run_root mkdir -p "$(dirname "$native_sysctl")"
	run_root cp "$native_sysctl_stage" "$native_sysctl.new.$$"
	run_root chmod 0644 "$native_sysctl.new.$$"
	run_root mv "$native_sysctl.new.$$" "$native_sysctl"
	if [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ] && ! run_root sysctl -p "$native_sysctl"; then
		warn "部分 sysctl 参数不受当前 OpenWrt/OpenRC 内核支持，请检查上面的输出"
	fi
}

configure_native_firewall() {
	if [ "$INIT_SYSTEM" = openwrt ] && command -v uci >/dev/null 2>&1; then
		uci -q delete firewall.niulang || true
		uci set firewall.niulang=rule
		uci set firewall.niulang.name='Allow-Niulang'
		uci set firewall.niulang.src='wan'
		uci set firewall.niulang.proto='tcp udp'
		uci set firewall.niulang.dest_port="$SERVER_PORT"
		uci set firewall.niulang.target='ACCEPT'
		uci commit firewall
		"$(root_path /etc/init.d/firewall)" reload
		info "已通过 UCI 放行 WAN TCP/UDP $SERVER_PORT"
	elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		run_root ufw allow "$SERVER_PORT/tcp"
		run_root ufw allow "$SERVER_PORT/udp"
		info "已通过 UFW 放行 TCP/UDP $SERVER_PORT"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		run_root firewall-cmd --permanent --add-port="$SERVER_PORT/tcp"
		run_root firewall-cmd --permanent --add-port="$SERVER_PORT/udp"
		run_root firewall-cmd --reload
		info "已通过 firewalld 放行 TCP/UDP $SERVER_PORT"
	else
		warn "未检测到可安全修改的防火墙；请手动同时放行 TCP/UDP $SERVER_PORT"
	fi
}

install_native_server() {
	[ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 服务端安装必须以 root 运行"
	install_native_binary
	create_native_runtime
	native_new_provider=false
	if server_state_exists "$(root_path "$SERVER_STATE")"; then
		if native_service_installed server; then
			load_native_server_configuration
			if confirm "保留现有 protocol 2 provider 配置，仅更新二进制并重启？" yes; then
				if native_start_service server && verify_native_service server; then
					info "服务端已更新"
					return 0
				fi
				restore_native_binary
				native_start_service server >/dev/null 2>&1 || true
				die "服务端更新失败，已恢复旧二进制"
			fi
		fi
		SERVER_PORT=$(native_provider_endpoint_port) || die "无法从 provider.json 读取公网端口"
		info "复用 provider trust root：$(native_provider_endpoint)"
	else
		native_provider_host=$(prompt "公网域名或 IP（不带端口）" "")
		valid_token "$native_provider_host" || die "公网域名/IP 包含不支持的字符"
		SERVER_PORT=$(ask_port "公网监听端口（TCP 和 UDP 共用）" 443)
		native_provider_name=$(prompt "服务名称" "My Niulang")
		[ -n "$native_provider_name" ] || die "服务名称不能为空"
		case $native_provider_host in
		*:*) native_endpoint="[$native_provider_host]:$SERVER_PORT" ;;
		*) native_endpoint="$native_provider_host:$SERVER_PORT" ;;
		esac
		native_provider_command init --name "$native_provider_name" --endpoint "$native_endpoint"
		native_new_provider=true
		native_first_user=$(prompt "首个用户名" "")
		[ -n "$native_first_user" ] || die "用户名不能为空"
		native_first_clients=$(ask_nonnegative_integer "该用户最大活跃设备数（0 不限制）" 8 65536)
		native_first_flows=$(ask_nonnegative_integer "该用户最大并发流（0 使用全局限制）" 1024 65536)
		native_provider_command add-user --name "$native_first_user" \
			--max-clients "$native_first_clients" --max-flows "$native_first_flows"
		native_invite_ttl=$(prompt "首个邀请有效期（最长 7d）" 24h)
	fi
	while :; do
		SERVER_TRANSPORT=$(prompt "传输模式（auto/quic/tcp）" "$SERVER_TRANSPORT")
		case $SERVER_TRANSPORT in auto | quic | tcp) break ;; esac
		warn "传输模式只能是 auto、quic 或 tcp"
	done
	SERVER_MAX_SESSIONS=$(ask_positive_integer "服务端全局最大并发流" "$SERVER_MAX_SESSIONS" 65536)
	SERVER_METRICS_PORT=$(ask_port "本机 metrics 端口" "$SERVER_METRICS_PORT")
	if confirm "允许代理访问服务端所在私网/本地地址？" no; then
		SERVER_ALLOW_PRIVATE=true
	else
		SERVER_ALLOW_PRIVATE=false
	fi
	native_server_args=$(native_server_arguments)
	if confirm "应用推荐的 Linux socket/backlog 参数？" yes; then
		tune_native_server
	fi
	if ! write_native_service server "$native_server_args"; then
		restore_native_binary
		native_start_service server >/dev/null 2>&1 || true
		die "服务端安装失败"
	fi
	if confirm "自动配置检测到的本机防火墙？" yes; then
		configure_native_firewall
	else
		warn "请手动同时放行 TCP/UDP $SERVER_PORT"
	fi
	if [ "$native_new_provider" = true ]; then
		warn "下面是一次性 bearer 凭据，只能通过可信私密渠道发送"
		native_provider_command invite --user "$native_first_user" --expires-in "$native_invite_ttl"
	fi
}

install_native_client() {
	[ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 客户端安装必须以 root 运行"
	install_native_binary
	create_native_runtime
	if native_service_installed client; then
		load_native_client_configuration
		if [ "$CLIENT_MODE" = multi ]; then
			[ -f "$(root_path "$PROVIDERS_PATH")" ] || die "客户端 manifest 不存在: $PROVIDERS_PATH"
			if native_start_service client && verify_native_service client; then
				info "已保留多 provider manifest 并更新客户端"
				return 0
			fi
			restore_native_binary
			native_start_service client >/dev/null 2>&1 || true
			die "客户端更新失败，已恢复旧二进制"
		fi
	fi
	choose_native_client_binding
	native_profile_full=$(root_path "$PROFILE_PATH")
	if [ -f "$native_profile_full" ]; then
		confirm "检测到 $PROFILE_PATH，继续使用该 profile？" yes ||
			die "脚本不会覆盖已消费邀请生成的 profile"
	else
		native_invitation=$(prompt_invitation)
		native_device=$(prompt "设备名称" "$(hostname 2>/dev/null || printf device)")
		if ! run_as_runtime "$(root_path "$NATIVE_BINARY")" enroll "$native_invitation" \
			--profile "$PROFILE_PATH" --device-name "$native_device" --local-address "$enrollment_address"; then
			warn "$PROFILE_PATH.enrolling 可能是可恢复草稿；请使用相同邀请、路径和设备名重试"
			return 1
		fi
	fi
	run_root chmod 0600 "$native_profile_full"
	if [ "$RUNTIME_USER" != root ] && [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
		run_root chown "$RUNTIME_USER:$SERVER_GROUP" "$native_profile_full"
	fi
	CLIENT_PORT=$(ask_port "本地 SOCKS5 端口" "$CLIENT_PORT")
	CLIENT_METRICS_PORT=$(ask_port "本机 metrics 端口" "$CLIENT_METRICS_PORT")
	CLIENT_MAX_SESSIONS=$(ask_positive_integer "客户端共享最大并发流" "$CLIENT_MAX_SESSIONS" 65536)
	CLIENT_MODE=single
	native_client_args=$(native_client_arguments)
	if ! write_native_service client "$native_client_args"; then
		restore_native_binary
		native_start_service client >/dev/null 2>&1 || true
		die "客户端安装失败"
	fi
	printf 'SOCKS5: 127.0.0.1:%s\nMetrics: http://127.0.0.1:%s/metrics\n' \
		"$CLIENT_PORT" "$CLIENT_METRICS_PORT" >&2
}

client_binary_supports_providers() {
	client_capability_binary=$1
	"$client_capability_binary" client --help 2>&1 |
		grep -E -q -e '(^|[[:space:]])--?providers([[:space:]=]|$)'
}

resolve_native_client_enrollment_address() {
	case $CLIENT_LOCAL_ADDRESS in
	openwrt:*)
		OPENWRT_NETWORK=${CLIENT_LOCAL_ADDRESS#openwrt:}
		if enrollment_address=$(openwrt_ipv4 "$OPENWRT_NETWORK"); then
			info "$OPENWRT_NETWORK 当前 IPv4 为 $enrollment_address"
		else
			warn "$OPENWRT_NETWORK 当前没有 IPv4，无法安全复用现有客户端出站绑定"
			enrollment_address=$(prompt "本次注册的 --local-address" auto)
		fi
		;;
	*) enrollment_address=$CLIENT_LOCAL_ADDRESS ;;
	esac
	valid_local_address "$enrollment_address" || die "现有客户端的外层连接绑定无效: $enrollment_address"
}

manifest_is_version_one() {
	manifest_file=$(root_path "$PROVIDERS_PATH")
	[ -r "$manifest_file" ] || return 1
	tr -d '[:space:]' <"$manifest_file" | grep -q '"version":1[,}]' &&
		tr -d '[:space:]' <"$manifest_file" | grep -Fq '"providers":['
}

manifest_has_name() {
	manifest_name=$1
	manifest_file=$(root_path "$PROVIDERS_PATH")
	[ -r "$manifest_file" ] || return 1
	tr -d '[:space:]' <"$manifest_file" | grep -Fq "\"name\":\"$manifest_name\""
}

manifest_has_port() {
	manifest_port=$1
	manifest_file=$(root_path "$PROVIDERS_PATH")
	[ -r "$manifest_file" ] || return 1
	tr -d '[:space:]' <"$manifest_file" | grep -Fq "\"listen\":\"127.0.0.1:$manifest_port\""
}

json_escape() {
	case $1 in
	*'
'*) return 1 ;;
	esac
	printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

manifest_has_profile() {
	manifest_profile=$(json_escape "$1") || return 1
	manifest_file=$(root_path "$PROVIDERS_PATH")
	[ -r "$manifest_file" ] || return 1
	tr -d '[:space:]' <"$manifest_file" | grep -Fq "\"profile\":\"$manifest_profile\""
}

port_is_listening() {
	listening_hex=$(printf '%04X' "$1")
	for listening_table in /proc/net/tcp /proc/net/tcp6; do
		[ -r "$listening_table" ] || continue
		if awk -v suffix=":$listening_hex" '$2 ~ suffix "$" && $4 == "0A" { found = 1 } END { exit !found }' \
			"$listening_table"; then
			return 0
		fi
	done
	return 1
}

suggest_provider_port() {
	suggested_port=$((CLIENT_PORT + 1))
	while [ "$suggested_port" -le 65535 ]; do
		if ! manifest_has_port "$suggested_port" && ! port_is_listening "$suggested_port"; then
			printf '%s\n' "$suggested_port"
			return
		fi
		suggested_port=$((suggested_port + 1))
	done
	return 1
}

create_provider_manifest() {
	manifest_output=$1
	manifest_existing_name=$(json_escape "$2") || return 1
	manifest_existing_profile=$(json_escape "$3") || return 1
	manifest_existing_port=$4
	manifest_new_name=$(json_escape "$5") || return 1
	manifest_new_profile=$(json_escape "$6") || return 1
	manifest_new_port=$7
	cat >"$manifest_output" <<EOF
{
  "version": 1,
  "providers": [
    {"name": "$manifest_existing_name", "profile": "$manifest_existing_profile", "listen": "127.0.0.1:$manifest_existing_port"},
    {"name": "$manifest_new_name", "profile": "$manifest_new_profile", "listen": "127.0.0.1:$manifest_new_port"}
  ]
}
EOF
}

append_provider_manifest() {
	manifest_input=$1
	manifest_output=$2
	manifest_append_name=$(json_escape "$3") || return 1
	manifest_append_profile=$(json_escape "$4") || return 1
	manifest_append_port=$5
	ensure_temp
	manifest_entry=$TEMP_DIR/provider-entry.json
	cat >"$manifest_entry" <<EOF
    {"name": "$manifest_append_name", "profile": "$manifest_append_profile", "listen": "127.0.0.1:$manifest_append_port"}
EOF
	awk '
		NR == FNR { entry = entry $0 ORS; next }
		{ data = data $0 ORS }
		END {
			sub(/\n$/, "", entry)
			position = 0
			for (i = 1; i <= length(data); i++) {
				if (substr(data, i, 1) == "]") position = i
			}
			if (position == 0) exit 1
			prefix = substr(data, 1, position - 1)
			suffix = substr(data, position)
			trimmed = prefix
			sub(/[[:space:]]+$/, "", trimmed)
			separator = substr(trimmed, length(trimmed), 1) == "[" ? "" : ","
			printf "%s%s\n%s\n%s", prefix, separator, entry, suffix
		}
	' "$manifest_entry" "$manifest_input" >"$manifest_output"
}

add_native_client_provider() {
	[ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 客户端管理必须以 root 运行"
	native_client_binary=$(root_path "$NATIVE_BINARY")
	[ -x "$native_client_binary" ] || die "尚未安装 $NATIVE_BINARY；请先部署客户端"
	native_service_installed client || die "找不到已有 niulang-client 服务"
	client_binary_supports_providers "$native_client_binary" || die "当前二进制不支持 --providers"
	load_native_client_configuration
	if [ "$CLIENT_MODE" = multi ]; then
		[ -f "$(root_path "$PROVIDERS_PATH")" ] || die "客户端 manifest 不存在: $PROVIDERS_PATH"
		manifest_is_version_one || die "只支持更新 version 1 provider manifest: $PROVIDERS_PATH"
	else
		[ -f "$(root_path "$PROFILE_PATH")" ] || die "找不到现有客户端 profile: $PROFILE_PATH"
		[ ! -e "$(root_path "$PROVIDERS_PATH")" ] ||
			die "现有服务仍使用 --profile，但目标 manifest 已存在: $PROVIDERS_PATH"
	fi
	resolve_native_client_enrollment_address
	while :; do
		new_provider_name=$(prompt "新 provider 标识（用于 Clash/mihomo 区分）" provider-2)
		valid_token "$new_provider_name" || {
			warn "标识只能包含字母、数字、点、下划线、冒号和连字符"
			continue
		}
		manifest_has_name "$new_provider_name" && {
			warn "manifest 中已存在标识 $new_provider_name"
			continue
		}
		break
	done
	if [ "$CLIENT_MODE" = single ]; then
		while :; do
			existing_provider_name=$(prompt "现有 provider 标识" primary)
			if valid_token "$existing_provider_name" && [ "$existing_provider_name" != "$new_provider_name" ]; then
				break
			fi
			warn "现有和新增 provider 必须使用两个不同的安全标识"
		done
	fi
	new_provider_default_port=$(suggest_provider_port) || die "找不到可用的 loopback SOCKS5 端口"
	while :; do
		new_provider_port=$(ask_port "新 provider 的本地 SOCKS5 端口" "$new_provider_default_port")
		if manifest_has_port "$new_provider_port" || port_is_listening "$new_provider_port"; then
			warn "端口 $new_provider_port 已被 manifest 或其他进程使用"
			continue
		fi
		break
	done
	new_profile_path="$DATA_DIR/client-$new_provider_name.json"
	manifest_has_profile "$new_profile_path" && die "profile 已在 manifest 中使用: $new_profile_path"
	new_profile_full=$(root_path "$new_profile_path")
	if [ -f "$new_profile_full" ]; then
		confirm "检测到未加入 manifest 的 $new_profile_path，直接复用？" yes ||
			die "请改用其他 provider 标识或先处理该 profile"
	else
		new_provider_invitation=$(prompt_invitation)
		new_provider_device=$(prompt "设备名称" "$(hostname 2>/dev/null || printf device)")
		if ! run_as_runtime "$native_client_binary" enroll "$new_provider_invitation" \
			--profile "$new_profile_path" --device-name "$new_provider_device" \
			--local-address "$enrollment_address"; then
			warn "$new_profile_path.enrolling 可能是可恢复草稿；请使用相同参数重试"
			return 1
		fi
	fi
	run_root chmod 0600 "$new_profile_full"
	if [ "$RUNTIME_USER" != root ] && [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
		run_root chown "$RUNTIME_USER:$SERVER_GROUP" "$new_profile_full"
	fi
	ensure_temp
	manifest_full=$(root_path "$PROVIDERS_PATH")
	run_root mkdir -p "$(dirname "$manifest_full")"
	manifest_stage=$TEMP_DIR/providers.json
	manifest_existed=false
	if [ -f "$manifest_full" ]; then
		manifest_existed=true
		cp -p "$manifest_full" "$TEMP_DIR/providers.backup"
		append_provider_manifest "$manifest_full" "$manifest_stage" "$new_provider_name" \
			"$new_profile_path" "$new_provider_port" || die "无法更新 provider manifest"
	else
		create_provider_manifest "$manifest_stage" "$existing_provider_name" "$PROFILE_PATH" "$CLIENT_PORT" \
			"$new_provider_name" "$new_profile_path" "$new_provider_port" ||
			die "无法创建 provider manifest"
	fi
	run_root cp "$manifest_stage" "$manifest_full.new.$$"
	run_root chmod 0600 "$manifest_full.new.$$"
	run_root mv "$manifest_full.new.$$" "$manifest_full"
	CLIENT_MODE=multi
	if ! write_native_service client "$(native_client_arguments)"; then
		warn "新配置未能启动，正在恢复原 manifest"
		if [ "$manifest_existed" = true ]; then
			run_root cp -p "$TEMP_DIR/providers.backup" "$manifest_full"
		else
			run_root rm -f "$manifest_full"
		fi
		native_start_service client >/dev/null 2>&1 || true
		warn "新 profile 保留在 $new_profile_path，可在修复后复用"
		return 1
	fi
	info "已添加 provider $new_provider_name"
	printf 'Manifest: %s\nProfile: %s\nSOCKS5: 127.0.0.1:%s\n' \
		"$PROVIDERS_PATH" "$new_profile_path" "$new_provider_port" >&2
}

add_client_provider() {
	require_supported_init
	if [ "$INIT_SYSTEM" != systemd ]; then
		add_native_client_provider
		return
	fi
	[ "$(id -u)" -ne 0 ] || die "systemd 客户端由登录用户管理，不能以 root 运行"
	[ -x "$CLIENT_BINARY" ] || die "尚未安装客户端: $CLIENT_BINARY"
	new_provider_name=$(prompt "新 provider 标识（留空使用服务端名称）" "")
	new_provider_invitation=$(prompt_invitation)
	new_provider_local=$(systemd_user_service_argument local-address || true)
	valid_local_address "$new_provider_local" || new_provider_local=auto
	new_provider_local=$(prompt "外层连接绑定（auto、if:接口名或 IP）" "$new_provider_local")
	valid_local_address "$new_provider_local" || die "无效的外层连接绑定"
	if [ -n "$new_provider_name" ]; then
		valid_token "$new_provider_name" || die "provider 标识包含不支持的字符"
		new_provider_invitation="$new_provider_name=$new_provider_invitation"
	fi
	"$script_dir/install-client.sh" --binary "$CLIENT_BINARY" \
		--invite "$new_provider_invitation" --local-address "$new_provider_local"
}

systemd_user_service_argument() {
	service_argument_name=$1
	service_argument_path=${HOME:?HOME must be set}/.config/systemd/user/$CLIENT_SERVICE.service
	[ -r "$service_argument_path" ] || return 1
	sed -n 's/^ExecStart=//p' "$service_argument_path" |
		grep -o '"[^"]*"' | sed -e 's/^"//' -e 's/"$//' |
		awk -v wanted="--$service_argument_name" 'previous == wanted { print; exit } { previous = $0 }'
}

install_systemd_client_max_sessions() {
	client_new_max=$1
	client_unit_path=${HOME:?HOME must be set}/.config/systemd/user/$CLIENT_SERVICE.service
	[ -f "$client_unit_path" ] || die "找不到已有 $CLIENT_SERVICE.service"
	client_current_max=$(systemd_user_service_argument max-sessions || true)
	case $client_current_max in
	'') client_current_max=2048 ;;
	*[!0-9]*) die "现有 --max-sessions 不是数字；不会覆盖其他客户端参数" ;;
	esac
	printf '当前客户端共享最大并发流: %s\n' "$client_current_max" >&2
	[ "$client_new_max" != "$client_current_max" ] || {
		info "并发上限未改变"
		return
	}
	ensure_temp
	client_unit_stage=$TEMP_DIR/$CLIENT_SERVICE.service
	if grep -q '"--max-sessions"' "$client_unit_path"; then
		sed "s/\"--max-sessions\" \"[0-9][0-9]*\"/\"--max-sessions\" \"$client_new_max\"/" \
			"$client_unit_path" >"$client_unit_stage"
	else
		sed "s/\"--log-level\"/\"--max-sessions\" \"$client_new_max\" \"--log-level\"/" \
			"$client_unit_path" >"$client_unit_stage"
	fi
	cmp -s "$client_unit_path" "$client_unit_stage" && die "未找到管理器生成的 --max-sessions 参数"
	cp -p "$client_unit_path" "$TEMP_DIR/client.service.backup"
	cp "$client_unit_stage" "$client_unit_path.new.$$"
	chmod 0644 "$client_unit_path.new.$$"
	mv "$client_unit_path.new.$$" "$client_unit_path"
	if systemctl --user daemon-reload && systemctl --user restart "$CLIENT_SERVICE.service"; then
		info "客户端共享最大并发流已修改为 $client_new_max"
		return
	fi
	warn "新并发配置未能启动，正在恢复原服务定义"
	cp -p "$TEMP_DIR/client.service.backup" "$client_unit_path"
	systemctl --user daemon-reload >/dev/null 2>&1 || true
	systemctl --user restart "$CLIENT_SERVICE.service" >/dev/null 2>&1 || true
	return 1
}

configure_client_sessions() {
	require_supported_init
	if [ "$INIT_SYSTEM" = systemd ]; then
		[ "$(id -u)" -ne 0 ] || die "systemd 客户端由登录用户管理，不能以 root 运行"
		[ -x "$CLIENT_BINARY" ] || die "尚未安装客户端: $CLIENT_BINARY"
		client_current_max=$(systemd_user_service_argument max-sessions || true)
		case $client_current_max in '' | *[!0-9]*) client_current_max=2048 ;; esac
		client_new_max=$(ask_positive_integer "新的客户端共享最大并发流" "$client_current_max" 65536)
		install_systemd_client_max_sessions "$client_new_max"
		return
	fi
	[ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 客户端管理必须以 root 运行"
	native_service_installed client || die "找不到已有 niulang-client 服务"
	load_native_client_configuration
	case ${existing_max:-} in
	'' | *[!0-9]*) die "无法从现有服务读取 --max-sessions；不会覆盖其他客户端参数" ;;
	esac
	printf '当前客户端共享最大并发流: %s\n' "$CLIENT_MAX_SESSIONS" >&2
	[ "$CLIENT_MODE" != multi ] || warn "这是所有 provider 共享的总上限，不是每个 provider 的独立上限"
	client_new_max=$(ask_positive_integer "新的客户端共享最大并发流" "$CLIENT_MAX_SESSIONS" 65536)
	[ "$client_new_max" != "$CLIENT_MAX_SESSIONS" ] || {
		info "并发上限未改变"
		return
	}
	CLIENT_MAX_SESSIONS=$client_new_max
	write_native_service client "$(native_client_arguments)" || die "客户端并发配置更新失败，已恢复旧服务定义"
	info "客户端共享最大并发流已修改为 $CLIENT_MAX_SESSIONS"
}

install_server_interactive() {
	require_supported_init
	prepare_install_source
	if [ "$INIT_SYSTEM" != systemd ]; then
		install_native_server
		return
	fi
	server_state=$(prompt "Provider state 路径" "$SERVER_STATE")
	server_existing=false
	if server_state_exists "$server_state"; then
		server_existing=true
	fi
	server_current=
	if [ "$server_existing" = true ]; then
		server_current=$(existing_server_arguments || true)
	fi
	server_listen=$(argument_value "$server_current" listen)
	server_transport=$(argument_value "$server_current" transport)
	server_max=$(argument_value "$server_current" max-sessions)
	server_metrics=$(argument_value "$server_current" metrics-listen)
	[ -n "$server_listen" ] || server_listen=:443
	[ -n "$server_transport" ] || server_transport=auto
	[ -n "$server_max" ] || server_max=4096
	[ -n "$server_metrics" ] || server_metrics=127.0.0.1:19090

	if [ "$server_existing" = true ]; then
		info "将保留现有 protocol 2 provider state；不会重新初始化或重新签发设备"
		server_listen=$(prompt "监听地址" "$server_listen")
		server_transport=$(prompt "传输模式（auto/quic/tcp）" "$server_transport")
		server_max=$(prompt "服务端最大并发流" "$server_max")
		server_metrics=$(prompt "Metrics 地址" "$server_metrics")
		set -- "$INSTALL_DEPLOY/install-server.sh" --binary "$INSTALL_BINARY" \
			--no-provider-init --state "$server_state" --listen "$server_listen" \
			--transport "$server_transport" --max-sessions "$server_max" \
			--metrics-listen "$server_metrics"
	else
		server_name=$(prompt "服务名称" "My Niulang")
		server_endpoint=$(prompt "公网域名或 IP:端口" "")
		[ -n "$server_endpoint" ] || die "公网端点不能为空"
		server_first_user=$(prompt "首个用户名" "")
		[ -n "$server_first_user" ] || die "用户名不能为空"
		server_clients=$(ask_nonnegative_integer "该用户最大活跃设备数（0 不限制）" 8 65536)
		server_flows=$(ask_nonnegative_integer "该用户最大并发流（0 使用全局限制）" 1024 65536)
		server_invite_ttl=$(prompt "首个邀请有效期（最长 7d）" 24h)
		server_port=${server_endpoint##*:}
		valid_port "$server_port" || die "公网端点必须以有效数字端口结尾"
		server_listen=$(prompt "监听地址" ":$server_port")
		server_transport=$(prompt "传输模式（auto/quic/tcp）" auto)
		server_max=$(prompt "服务端最大并发流" 4096)
		server_metrics=$(prompt "Metrics 地址" 127.0.0.1:19090)
		set -- "$INSTALL_DEPLOY/install-server.sh" --binary "$INSTALL_BINARY" \
			--name "$server_name" --endpoint "$server_endpoint" --user "$server_first_user" \
			--state "$server_state" --user-max-clients "$server_clients" \
			--user-max-flows "$server_flows" --invite-expires-in "$server_invite_ttl" \
			--listen "$server_listen" --transport "$server_transport" \
			--max-sessions "$server_max" --metrics-listen "$server_metrics"
	fi
	if confirm "应用推荐的 Linux socket/backlog 参数？" yes; then
		set -- "$@" --tune
	fi
	run_root "$@"
}

default_client_manifest() {
	printf '%s/niulang/providers.json\n' "${XDG_CONFIG_HOME:-${HOME:?HOME must be set}/.config}"
}

install_client_interactive() {
	require_supported_init
	prepare_install_source
	if [ "$INIT_SYSTEM" != systemd ]; then
		install_native_client
		return
	fi
	[ "$(id -u)" -ne 0 ] || die "客户端必须由实际使用代理的登录用户安装，不能以 root 运行"
	client_manifest=$(default_client_manifest)
	set -- "$INSTALL_DEPLOY/install-client.sh" --binary "$INSTALL_BINARY"
	if [ -f "$client_manifest" ]; then
		info "检测到现有 Niulang provider manifest，将保留 profile 并更新程序和服务"
		client_local=$(systemd_user_service_argument local-address || true)
		valid_local_address "$client_local" || client_local=auto
	else
		client_invite=$(prompt_invitation)
		set -- "$@" --invite "$client_invite"
		client_local=auto
	fi
	client_local=$(prompt "外层连接绑定（auto、if:接口名或 IP）" "$client_local")
	valid_local_address "$client_local" || die "无效的外层连接绑定"
	set -- "$@" --local-address "$client_local"
	"$@"
}

provider_run() {
	if [ "$INIT_SYSTEM" != systemd ]; then
		run_as_runtime "$@"
		return
	fi
	[ -x "$SERVER_BINARY" ] || die "找不到已安装的 $SERVER_BINARY"
	if [ "$(id -u)" -eq 0 ]; then
		if command -v runuser >/dev/null 2>&1; then
			runuser -u "$SERVER_USER" -- "$@"
		elif command -v sudo >/dev/null 2>&1; then
			sudo -u "$SERVER_USER" -- "$@"
		else
			die "需要 runuser 或 sudo 以 $SERVER_USER 身份管理 provider"
		fi
	elif command -v sudo >/dev/null 2>&1; then
		sudo -u "$SERVER_USER" -- "$@"
	else
		die "管理 provider 需要 sudo 或 root"
	fi
}

provider_command() {
	if [ "$INIT_SYSTEM" = systemd ]; then
		provider_run "$SERVER_BINARY" provider "$@" --state "$SERVER_STATE"
	else
		provider_run "$(root_path "$NATIVE_BINARY")" provider "$@" --state "$SERVER_STATE"
	fi
}

create_user_and_invite() {
	new_user=$(prompt "用户名" "")
	[ -n "$new_user" ] || die "用户名不能为空"
	new_clients=$(ask_nonnegative_integer "最大活跃设备数（0 不限制）" 8 65536)
	new_flows=$(ask_nonnegative_integer "最大并发流（0 使用全局限制）" 1024 65536)
	new_lifetime=$(prompt "账号有效期（0 永不过期，例如 720h）" 0)
	set -- add-user --name "$new_user" --max-clients "$new_clients" --max-flows "$new_flows"
	if [ "$new_lifetime" != 0 ]; then
		set -- "$@" --expires-in "$new_lifetime"
	fi
	provider_command "$@"
	new_invite_lifetime=$(prompt "邀请有效期（最长 7d）" 1h)
	warn "下面是一次性 bearer 凭据，只能通过可信私密渠道发送"
	provider_command invite --user "$new_user" --expires-in "$new_invite_lifetime"
}

set_user_limits() {
	limit_user=$(prompt "用户名或用户 ID" "")
	[ -n "$limit_user" ] || die "用户名不能为空"
	limit_flows=$(ask_nonnegative_integer "最大并发流（0 使用全局限制）" 1024 65536)
	limit_clients=$(ask_nonnegative_integer "最大活跃设备数（0 不限制）" 8 65536)
	provider_command set-user-limits --user "$limit_user" \
		--max-flows "$limit_flows" --max-clients "$limit_clients"
}

manage_provider() {
	if [ "$INIT_SYSTEM" = systemd ]; then
		manage_state=$SERVER_STATE
	else
		manage_state=$(root_path "$SERVER_STATE")
		[ -x "$(root_path "$NATIVE_BINARY")" ] || die "找不到已安装的 $NATIVE_BINARY"
	fi
	server_state_exists "$manage_state" || die "找不到 provider state: $SERVER_STATE"
	while :; do
		cat >&2 <<'EOF'

Niulang 用户、邀请和设备管理
  1) 列出用户
  2) 新建用户并生成邀请
  3) 为已有用户生成邀请
  4) 列出未使用邀请
  5) 撤销邀请
  6) 列出设备
  7) 撤销设备
  8) 启用用户
  9) 禁用用户
 10) 修改用户限制
  0) 返回
EOF
		manage_choice=$(prompt "选择" 0)
		case $manage_choice in
		1) provider_command list-users ;;
		2) create_user_and_invite ;;
		3)
			manage_user=$(prompt "用户名或用户 ID" "")
			manage_expiry=$(prompt "邀请有效期（最长 7d）" 1h)
			provider_command invite --user "$manage_user" --expires-in "$manage_expiry"
			;;
		4)
			manage_user=$(prompt "用户名/ID（留空列出全部）" "")
			if [ -n "$manage_user" ]; then
				provider_command list-invites --user "$manage_user"
			else
				provider_command list-invites
			fi
			;;
		5)
			manage_id=$(prompt "邀请 ID" "")
			confirm "确认撤销邀请 $manage_id？" no && provider_command revoke-invite --invite "$manage_id"
			;;
		6)
			manage_user=$(prompt "用户名/ID（留空列出全部）" "")
			if [ -n "$manage_user" ]; then
				provider_command list-devices --user "$manage_user"
			else
				provider_command list-devices
			fi
			;;
		7)
			manage_id=$(prompt "设备 ID" "")
			confirm "确认撤销设备 $manage_id？活动连接会关闭。" no &&
				provider_command revoke-device --device "$manage_id"
			;;
		8 | 9)
			manage_user=$(prompt "用户名或用户 ID" "")
			if [ "$manage_choice" = 8 ]; then
				provider_command enable-user --user "$manage_user"
			else
				confirm "确认禁用用户 $manage_user？活动连接会关闭。" no &&
					provider_command disable-user --user "$manage_user"
			fi
			;;
		10) set_user_limits ;;
		0) return ;;
		*) warn "未知选项" ;;
		esac
	done
}

legacy_system_unit_path() {
	printf '%s/etc/systemd/system/%s.service\n' "$test_root" "$1"
}

legacy_user_unit_path() {
	printf '%s/.config/systemd/user/%s.service\n' "${HOME:?HOME must be set}" "$1"
}

legacy_native_unit_path() {
	printf '%s/etc/init.d/%s\n' "$test_root" "$1"
}

legacy_units_for_role() {
	case $1 in
	server) printf '%s\n' queqiao-server queqiaod ;;
	client) printf '%s\n' queqiao-client ;;
	*) return 1 ;;
	esac
}

legacy_service_role_found() {
	legacy_service_role=$1
	for legacy_service_unit in $(legacy_units_for_role "$legacy_service_role"); do
		[ -f "$(legacy_system_unit_path "$legacy_service_unit")" ] && return 0
		[ -f "$(legacy_user_unit_path "$legacy_service_unit")" ] && return 0
		[ -f "$(legacy_native_unit_path "$legacy_service_unit")" ] && return 0
	done
	return 1
}

legacy_service_found() {
	legacy_service_role_found server || legacy_service_role_found client
}

legacy_role_found() {
	legacy_role=$1
	legacy_service_role_found "$legacy_role" && return 0
	case $legacy_role in
	server) [ -d "$(root_path /var/lib/queqiao/provider)" ] ;;
	client)
		[ -f "$(root_path /var/lib/queqiao/client.json)" ] ||
			[ -f "$HOME/.config/queqiao/client.json" ] ||
			[ -f "$HOME/.config/queqiao/providers.json" ]
		;;
	esac
}

legacy_found() {
	legacy_role_found server || legacy_role_found client
}

show_legacy_status() {
	if ! legacy_found; then
		printf '未检测到旧 Queqiao 服务或默认数据目录。\n'
		return
	fi
	printf '检测到的旧 Queqiao 项目：\n'
	for legacy_role in server client; do
		legacy_role_found "$legacy_role" || continue
		printf '  角色: %s\n' "$legacy_role"
		for legacy_unit in $(legacy_units_for_role "$legacy_role"); do
			legacy_system_path=$(legacy_system_unit_path "$legacy_unit")
			legacy_user_path=$(legacy_user_unit_path "$legacy_unit")
			legacy_native_path=$(legacy_native_unit_path "$legacy_unit")
			[ -f "$legacy_system_path" ] && printf '    system service: %s\n' "$legacy_system_path"
			[ -f "$legacy_user_path" ] && printf '    user service:   %s\n' "$legacy_user_path"
			[ -f "$legacy_native_path" ] && printf '    %s service: %s\n' "$INIT_SYSTEM" "$legacy_native_path"
		done
	done
	for legacy_path in \
		/var/lib/queqiao /etc/queqiao /var/log/queqiao \
		/usr/local/bin/queqiaod /usr/bin/queqiaod; do
		legacy_full=$(root_path "$legacy_path")
		[ -e "$legacy_full" ] && printf '  %s\n' "$legacy_full"
	done
	for legacy_path in "$HOME/.config/queqiao" "$HOME/.local/bin/queqiaod"; do
		[ -e "$legacy_path" ] && printf '  %s\n' "$legacy_path"
	done
	# A missing optional path is expected and must not become this function's
	# status: callers run under set -e and still need to reach the migration
	# confirmation after displaying the detected files.
	return 0
}

legacy_native_enabled() {
	legacy_native_name=$1
	legacy_native_path=$(legacy_native_unit_path "$legacy_native_name")
	case $INIT_SYSTEM in
	openrc) rc-update show default 2>/dev/null | grep -q "[[:space:]]$legacy_native_name[[:space:]]" ;;
	openwrt) "$legacy_native_path" enabled >/dev/null 2>&1 ;;
	*) return 1 ;;
	esac
}

legacy_native_active() {
	legacy_native_name=$1
	case $INIT_SYSTEM in
	openrc) rc-service "$legacy_native_name" status >/dev/null 2>&1 ;;
	openwrt)
		ubus call service list "{\"name\":\"$legacy_native_name\"}" 2>/dev/null |
			grep -q '"running"[[:space:]]*:[[:space:]]*true'
		;;
	*) return 1 ;;
	esac
}

record_legacy_state() {
	record_role=$1
	record_file=$2
	: >"$record_file"
	for record_unit in $(legacy_units_for_role "$record_role"); do
		record_system_path=$(legacy_system_unit_path "$record_unit")
		if [ -f "$record_system_path" ]; then
			record_enabled=false
			record_active=false
			systemctl is-enabled --quiet "$record_unit.service" 2>/dev/null && record_enabled=true
			systemctl is-active --quiet "$record_unit.service" 2>/dev/null && record_active=true
			printf 'system\t%s\t%s\t%s\n' "$record_unit" "$record_enabled" "$record_active" >>"$record_file"
		fi
		record_user_path=$(legacy_user_unit_path "$record_unit")
		if [ -f "$record_user_path" ]; then
			record_enabled=false
			record_active=false
			systemctl --user is-enabled --quiet "$record_unit.service" 2>/dev/null && record_enabled=true
			systemctl --user is-active --quiet "$record_unit.service" 2>/dev/null && record_active=true
			printf 'user\t%s\t%s\t%s\n' "$record_unit" "$record_enabled" "$record_active" >>"$record_file"
		fi
		record_native_path=$(legacy_native_unit_path "$record_unit")
		if [ -f "$record_native_path" ]; then
			record_enabled=false
			record_active=false
			legacy_native_enabled "$record_unit" && record_enabled=true
			legacy_native_active "$record_unit" && record_active=true
			printf 'native\t%s\t%s\t%s\n' "$record_unit" "$record_enabled" "$record_active" >>"$record_file"
		fi
	done
}

stop_legacy_role() {
	stop_role=$1
	for stop_unit in $(legacy_units_for_role "$stop_role"); do
		if [ -f "$(legacy_system_unit_path "$stop_unit")" ]; then
			run_root systemctl disable --now "$stop_unit.service" >/dev/null 2>&1 || true
		fi
		if [ -f "$(legacy_user_unit_path "$stop_unit")" ]; then
			systemctl --user disable --now "$stop_unit.service" >/dev/null 2>&1 || true
		fi
		stop_native_path=$(legacy_native_unit_path "$stop_unit")
		if [ -f "$stop_native_path" ]; then
			case $INIT_SYSTEM in
			openrc)
				rc-service "$stop_unit" stop >/dev/null 2>&1 || true
				rc-update del "$stop_unit" default >/dev/null 2>&1 || true
				;;
			openwrt)
				"$stop_native_path" stop >/dev/null 2>&1 || true
				"$stop_native_path" disable >/dev/null 2>&1 || true
				;;
			esac
		fi
	done
	info "旧 Queqiao $stop_role 服务已停止并禁用；state/profile 未删除"
}

restore_legacy_state() {
	restore_file=$1
	[ -f "$restore_file" ] || return 0
	tab=$(printf '\t')
	while IFS="$tab" read -r restore_scope restore_unit restore_enabled restore_active; do
		if [ "$restore_scope" = system ]; then
			if [ "$restore_enabled" = true ]; then
				run_root systemctl enable "$restore_unit.service" >/dev/null 2>&1 || true
			fi
			if [ "$restore_active" = true ]; then
				run_root systemctl start "$restore_unit.service" >/dev/null 2>&1 || true
			fi
		elif [ "$restore_scope" = user ]; then
			if [ "$restore_enabled" = true ]; then
				systemctl --user enable "$restore_unit.service" >/dev/null 2>&1 || true
			fi
			if [ "$restore_active" = true ]; then
				systemctl --user start "$restore_unit.service" >/dev/null 2>&1 || true
			fi
		else
			restore_native_path=$(legacy_native_unit_path "$restore_unit")
			case $INIT_SYSTEM in
			openrc)
				[ "$restore_enabled" != true ] || rc-update add "$restore_unit" default >/dev/null 2>&1 || true
				[ "$restore_active" != true ] || rc-service "$restore_unit" start >/dev/null 2>&1 || true
				;;
			openwrt)
				[ "$restore_enabled" != true ] || "$restore_native_path" enable >/dev/null 2>&1 || true
				[ "$restore_active" != true ] || "$restore_native_path" start >/dev/null 2>&1 || true
				;;
			esac
		fi
	done <"$restore_file"
}

remove_legacy_role() {
	remove_role=$1
	stop_legacy_role "$remove_role"
	for remove_unit in $(legacy_units_for_role "$remove_role"); do
		remove_system_path=$(legacy_system_unit_path "$remove_unit")
		remove_user_path=$(legacy_user_unit_path "$remove_unit")
		remove_native_path=$(legacy_native_unit_path "$remove_unit")
		if [ -f "$remove_system_path" ]; then
			run_root rm -f "$remove_system_path"
		fi
		if [ -f "$remove_user_path" ]; then
			rm -f "$remove_user_path"
		fi
		if [ -f "$remove_native_path" ]; then
			run_root rm -f "$remove_native_path"
		fi
	done
	if [ "$INIT_SYSTEM" = systemd ]; then
		run_root systemctl daemon-reload
		systemctl --user daemon-reload >/dev/null 2>&1 || true
	fi
	info "旧 Queqiao $remove_role 服务定义已删除；数据仍保留"
}

remove_legacy_binaries() {
	legacy_service_found && die "仍有旧 Queqiao 服务定义；先迁移或删除服务"
	for remove_binary in /usr/local/bin/queqiaod /usr/bin/queqiaod; do
		remove_binary_path=$(root_path "$remove_binary")
		if [ -f "$remove_binary_path" ] && confirm "删除 $remove_binary_path？" no; then
			run_root rm -f "$remove_binary_path"
		fi
	done
	if [ -f "$HOME/.local/bin/queqiaod" ] && confirm "删除 $HOME/.local/bin/queqiaod？" no; then
		rm -f "$HOME/.local/bin/queqiaod"
	fi
}

purge_legacy_data() {
	warn "此操作永久删除旧 Queqiao provider keys、客户端 profile、配置和日志；它们不能转换成 protocol 2"
	printf '如已完成备份并确认删除，请输入 DELETE QUEQIAO: ' >&2
	IFS= read -r purge_answer || die "输入已中断"
	[ "$purge_answer" = "DELETE QUEQIAO" ] || {
		info "未删除任何数据"
		return
	}
	legacy_service_found && die "仍检测到旧 Queqiao 服务；请先删除服务定义"
	for purge_path in \
		/var/lib/queqiao /etc/queqiao /var/log/queqiao \
		/usr/local/lib/queqiao /etc/sysctl.d/90-queqiao-performance.conf; do
		purge_full=$(root_path "$purge_path")
		[ ! -e "$purge_full" ] || run_root rm -rf "$purge_full"
	done
	for purge_path in "$HOME/.config/queqiao" "$HOME/.local/state/queqiao"; do
		[ ! -e "$purge_path" ] || rm -rf "$purge_path"
	done
	if [ "$INIT_SYSTEM" = openwrt ] && command -v uci >/dev/null 2>&1; then
		if uci -q get firewall.queqiao >/dev/null 2>&1; then
			uci -q delete firewall.queqiao || true
			uci commit firewall
			"$(root_path /etc/init.d/firewall)" reload >/dev/null 2>&1 || true
		fi
	fi
	info "旧 Queqiao 数据已删除"
}

stop_new_role_after_failed_migration() {
	failed_role=$1
	if [ "$INIT_SYSTEM" = systemd ]; then
		case $failed_role in
		server) run_root systemctl disable --now "$SERVER_SERVICE.service" >/dev/null 2>&1 || true ;;
		client) systemctl --user disable --now "$CLIENT_SERVICE.service" >/dev/null 2>&1 || true ;;
		esac
	else
		native_stop_service "$failed_role"
	fi
}

migrate_legacy_role() {
	migrate_role=$1
	if [ "$INIT_SYSTEM" = systemd ] && [ "$migrate_role" = client ] && [ "$(id -u)" -eq 0 ]; then
		die "客户端迁移必须由实际登录用户运行；请退出 root 后重新执行 $0 migrate"
	fi
	ensure_temp
	migrate_state=$TEMP_DIR/legacy-$migrate_role.tsv
	record_legacy_state "$migrate_role" "$migrate_state"
	stop_legacy_role "$migrate_role"

	if [ "$migrate_role" = server ]; then
		warn "旧 provider state、账号、设备和邀请不会迁移；现在会创建全新的 Niulang protocol 2 trust domain"
		if (install_server_interactive); then
			migrate_ok=true
		else
			migrate_ok=false
		fi
	else
		warn "旧 profile 和设备证书不会迁移；客户端必须使用服务端新生成的邀请链接"
		if (install_client_interactive); then
			migrate_ok=true
		else
			migrate_ok=false
		fi
	fi

	if [ "$migrate_ok" = true ]; then
		info "$migrate_role 已迁移到 Niulang；旧数据暂时保留用于人工回退"
		if confirm "现在删除旧 Queqiao $migrate_role 服务定义？" yes; then
			remove_legacy_role "$migrate_role"
		fi
		return 0
	fi

	warn "Niulang $migrate_role 安装未完成，正在恢复旧服务状态"
	stop_new_role_after_failed_migration "$migrate_role"
	restore_legacy_state "$migrate_state"
	return 1
}

migrate_legacy() {
	require_supported_init
	legacy_service_found || {
		info "未检测到旧 Queqiao 服务"
		return
	}
	show_legacy_status
	warn "Niulang protocol 2 不读取旧 state/profile，也不提供 wire downgrade"
	if legacy_service_role_found server && confirm "迁移旧服务端？" yes; then
		migrate_legacy_role server
	fi
	if legacy_service_role_found client; then
		if [ "$INIT_SYSTEM" = systemd ] && [ "$(id -u)" -eq 0 ]; then
			warn "检测到旧客户端。请以实际使用代理的登录用户重新运行 $0 migrate；root 不应持有客户端 profile。"
		elif confirm "迁移旧客户端？需要新的 Niulang 邀请。" yes; then
			migrate_legacy_role client
		fi
	fi
}

systemd_replace_binary() {
	replace_binary_path=$1
	replace_binary_scope=$2
	replace_binary_service=$3
	ensure_temp
	replace_binary_backup=$TEMP_DIR/$(basename "$replace_binary_path").$replace_binary_scope.backup
	replace_binary_existed=false
	if [ "$replace_binary_scope" = system ]; then
		if run_root test -x "$replace_binary_path"; then
			replace_binary_existed=true
			run_root cp -p "$replace_binary_path" "$replace_binary_backup"
		fi
	elif [ -x "$replace_binary_path" ]; then
		replace_binary_existed=true
		cp -p "$replace_binary_path" "$replace_binary_backup"
	fi
	if [ "$replace_binary_scope" = system ]; then
		run_root mkdir -p "$(dirname "$replace_binary_path")"
		run_root cp "$INSTALL_BINARY" "$replace_binary_path.new.$$"
		run_root chmod 0755 "$replace_binary_path.new.$$"
		run_root mv "$replace_binary_path.new.$$" "$replace_binary_path"
		if run_root systemctl restart "$replace_binary_service.service"; then
			return 0
		fi
		if [ "$replace_binary_existed" = true ]; then
			run_root cp -p "$replace_binary_backup" "$replace_binary_path"
		else
			run_root rm -f "$replace_binary_path"
		fi
		run_root systemctl restart "$replace_binary_service.service" >/dev/null 2>&1 || true
	else
		mkdir -p "$(dirname "$replace_binary_path")"
		cp "$INSTALL_BINARY" "$replace_binary_path.new.$$"
		chmod 0755 "$replace_binary_path.new.$$"
		mv "$replace_binary_path.new.$$" "$replace_binary_path"
		if systemctl --user restart "$replace_binary_service.service"; then
			return 0
		fi
		if [ "$replace_binary_existed" = true ]; then
			cp -p "$replace_binary_backup" "$replace_binary_path"
		else
			rm -f "$replace_binary_path"
		fi
		systemctl --user restart "$replace_binary_service.service" >/dev/null 2>&1 || true
	fi
	return 1
}

restore_systemd_binary_backup() {
	restore_binary_path=$1
	restore_binary_scope=$2
	restore_binary_service=$3
	restore_binary_backup=$TEMP_DIR/$(basename "$restore_binary_path").$restore_binary_scope.backup
	[ -f "$restore_binary_backup" ] || return 0
	if [ "$restore_binary_scope" = system ]; then
		run_root cp -p "$restore_binary_backup" "$restore_binary_path"
		run_root systemctl restart "$restore_binary_service.service" >/dev/null 2>&1 || true
	else
		cp -p "$restore_binary_backup" "$restore_binary_path"
		systemctl --user restart "$restore_binary_service.service" >/dev/null 2>&1 || true
	fi
}

update_binary_only() {
	require_supported_init
	prepare_install_source
	if [ "$INIT_SYSTEM" != systemd ]; then
		[ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 二进制更新必须以 root 运行"
		install_native_binary
		updated_any=false
		for update_role in server client; do
			if native_service_installed "$update_role"; then
				if [ "$update_role" = server ]; then
					load_native_server_configuration
					ensure_native_server_capability
				fi
				if ! native_start_service "$update_role" || ! verify_native_service "$update_role"; then
					restore_native_binary
					if native_service_installed server; then
						load_native_server_configuration
						ensure_native_server_capability
					fi
					for restore_role in server client; do
						native_service_installed "$restore_role" && native_start_service "$restore_role" >/dev/null 2>&1 || true
					done
					die "$update_role 未能使用新二进制启动；已恢复旧二进制"
				fi
				updated_any=true
			fi
		done
	else
		updated_any=false
		systemd_server_updated=false
		if [ -f "$(system_unit_path "$SERVER_SERVICE")" ]; then
			systemd_replace_binary "$SERVER_BINARY" system "$SERVER_SERVICE" ||
				die "服务端未能使用新二进制启动；已恢复旧二进制"
			updated_any=true
			systemd_server_updated=true
		fi
		client_unit=${HOME:?HOME must be set}/.config/systemd/user/$CLIENT_SERVICE.service
		if [ -f "$client_unit" ]; then
			if ! systemd_replace_binary "$CLIENT_BINARY" user "$CLIENT_SERVICE"; then
				if [ "$systemd_server_updated" = true ]; then
					restore_systemd_binary_backup "$SERVER_BINARY" system "$SERVER_SERVICE"
				fi
				die "客户端未能使用新二进制启动；已恢复本次更新的二进制"
			fi
			updated_any=true
		fi
	fi
	if [ "$updated_any" = true ]; then
		info "已原子更新二进制并重启本机 Niulang 服务"
	else
		warn "未找到由管理器识别的 Niulang 服务；二进制已安装，但没有服务可重启"
	fi
}

print_service_status() {
	status_role=$1
	if [ "$INIT_SYSTEM" = systemd ]; then
		if [ "$status_role" = server ]; then
			if [ ! -f "$(system_unit_path "$SERVER_SERVICE")" ]; then
				printf '%-8s 未安装\n' "$status_role"
			elif systemctl is-active --quiet "$SERVER_SERVICE.service" 2>/dev/null; then
				printf '%-8s active\n' "$status_role"
			else
				printf '%-8s inactive\n' "$status_role"
			fi
		else
			status_client_unit=${HOME:?HOME must be set}/.config/systemd/user/$CLIENT_SERVICE.service
			if [ ! -f "$status_client_unit" ]; then
				printf '%-8s 未安装（当前用户）\n' "$status_role"
			elif systemctl --user is-active --quiet "$CLIENT_SERVICE.service" 2>/dev/null; then
				printf '%-8s active\n' "$status_role"
			else
				printf '%-8s inactive\n' "$status_role"
			fi
		fi
	elif ! native_service_installed "$status_role"; then
		printf '%-8s 未安装\n' "$status_role"
	elif native_service_is_active "$status_role"; then
		printf '%-8s active\n' "$status_role"
	else
		printf '%-8s inactive\n' "$status_role"
	fi
}

show_status() {
	require_supported_init
	printf '平台: Linux/%s，服务管理器: %s\n' "$(uname -m)" "$INIT_SYSTEM"
	printf 'Release 来源: %s\n' "$(release_source_description)"
	if [ "$INIT_SYSTEM" = systemd ]; then
		if [ -x "$SERVER_BINARY" ]; then "$SERVER_BINARY" version || true; else printf 'Server binary: 未安装（%s）\n' "$SERVER_BINARY"; fi
		if [ -x "$CLIENT_BINARY" ]; then "$CLIENT_BINARY" version || true; else printf 'Client binary: 未安装（%s）\n' "$CLIENT_BINARY"; fi
	else
		if [ -x "$(root_path "$NATIVE_BINARY")" ]; then
			"$(root_path "$NATIVE_BINARY")" version || true
		else
			printf 'Binary: 未安装（%s）\n' "$NATIVE_BINARY"
		fi
	fi
	print_service_status server
	print_service_status client
	[ ! -d "$(root_path "$SERVER_STATE")" ] || printf 'Provider state: %s\n' "$SERVER_STATE"
	[ ! -f "$(root_path "$PROFILE_PATH")" ] || printf 'Client profile: %s\n' "$PROFILE_PATH"
	[ ! -f "$(root_path "$PROVIDERS_PATH")" ] || printf 'Client providers: %s\n' "$PROVIDERS_PATH"
	show_legacy_status
}

stop_legacy_interactive() {
	legacy_service_role_found server && confirm "停止并禁用旧 Queqiao server？" no && stop_legacy_role server
	legacy_service_role_found client && confirm "停止并禁用旧 Queqiao client？" no && stop_legacy_role client
	return 0
}

remove_legacy_interactive() {
	legacy_service_role_found server && confirm "删除旧 Queqiao server 服务定义？" no && remove_legacy_role server
	legacy_service_role_found client && confirm "删除旧 Queqiao client 服务定义？" no && remove_legacy_role client
	if ! legacy_service_found && confirm "继续检查并删除旧 queqiaod 二进制？" no; then
		remove_legacy_binaries
	fi
	return 0
}

offer_legacy_migration() {
	legacy_service_found || return 0
	warn "检测到旧 Queqiao 服务。Niulang 不兼容其 state/profile，客户端需要新的邀请链接。"
	if confirm "现在进入迁移流程？" yes; then
		migrate_legacy
	fi
}

interactive_menu() {
	offer_legacy_migration
	while :; do
		cat >&2 <<EOF

Niulang Linux 管理工具（检测到 $INIT_SYSTEM）
Release 来源：$(release_source_description)
  1) 安装/更新服务端
  2) 安装/更新客户端
  3) 向已有客户端添加 provider
  4) 修改客户端共享最大并发流
  5) 管理服务端用户、邀请和设备
  6) 仅更新二进制并重启已有服务
  7) 查看 Niulang 和旧 Queqiao 状态
  8) 切换并保存 Release 下载来源
  9) 迁移旧 Queqiao 服务
 10) 停止并禁用旧 Queqiao 服务
 11) 删除旧 Queqiao 服务定义/二进制
 12) 永久删除旧 Queqiao state/profile/配置/日志
  0) 退出
EOF
		menu_choice=$(prompt "选择" 0)
		case $menu_choice in
		1) install_server_interactive ;;
		2) install_client_interactive ;;
		3) add_client_provider ;;
		4) configure_client_sessions ;;
		5) manage_provider ;;
		6) update_binary_only ;;
		7) show_status ;;
		8) choose_release_source ;;
		9) migrate_legacy ;;
		10) stop_legacy_interactive ;;
		11) remove_legacy_interactive ;;
		12) purge_legacy_data ;;
		0) return ;;
		*) warn "未知选项" ;;
		esac
	done
}

usage() {
	cat <<'EOF'
用法: deploy/manage.sh [server|client|add-provider|client-config|provider|manage|update|source|status|migrate|legacy-stop|legacy-remove|legacy-purge]

不带参数时进入交互菜单。支持 systemd、OpenRC 和 OpenWrt procd。
管理器下载所选 Niulang Release、校验 SHA256SUMS，并拒绝非 protocol 2 二进制。

  server         安装或更新服务端
  client         安装或更新客户端
  add-provider   为已有客户端添加 provider 和 SOCKS5 监听
  client-config  修改所有 provider 共享的最大并发流
  provider/manage 管理用户、邀请、设备和限制
  update         原子更新二进制并重启已有服务；失败时回滚
  source         切换并保存官方/自定义 Release 来源及预发行策略
  status         查看 Niulang 与旧 Queqiao 状态
  migrate        停止旧服务并迁移角色；state/profile 不复制
  legacy-stop    停止并禁用旧 Queqiao 服务，保留全部文件
  legacy-remove  删除旧服务定义，并可删除旧二进制，保留数据
  legacy-purge   强确认后永久删除旧 state/profile/配置和日志

环境变量：
  NIULANG_VERSION=latest          latest 或指定 vYYYY.MDD.N
  NIULANG_BINARY=/path            使用本地 protocol 2 二进制
  NIULANG_REPOSITORY=owner/repo   Release 仓库（默认 4fuu/niulang）
  NIULANG_INCLUDE_PRERELEASE=1    latest 可选择预发行版
  NIULANG_INIT_SYSTEM=openwrt     覆盖服务管理器自动检测
  NIULANG_STATE_DIR=/path         Provider state 路径
  NIULANG_PROFILE_PATH=/path      单 provider 客户端 profile
  NIULANG_PROVIDERS_PATH=/path    多 provider manifest
  NIULANG_OPENWRT_NETWORK=wan     OpenWrt 逻辑出站接口

旧 Queqiao 凭据无法转换。服务端迁移会创建新的 trust domain；客户端迁移
必须粘贴服务端新生成的 niulang://enroll/... 邀请链接。
EOF
}

main() {
	detect_init_system
	load_saved_release_source
	case ${1:-} in
	-h | --help | help)
		usage
		return
		;;
	esac
	require_supported_init
	case $INIT_SYSTEM:${1:-} in
	openrc:status | openwrt:status | openrc:-h | openwrt:-h) ;;
	openrc:* | openwrt:*) [ "$(id -u)" -eq 0 ] || die "$INIT_SYSTEM 管理操作必须以 root 运行" ;;
	esac
	case ${1:-} in
	server)
		if legacy_service_role_found server && confirm "检测到旧 Queqiao server，现在迁移？" yes; then
			migrate_legacy_role server
		else
			install_server_interactive
		fi
		;;
	client)
		if legacy_service_role_found client && confirm "检测到旧 Queqiao client，现在迁移？" yes; then
			migrate_legacy_role client
		else
			install_client_interactive
		fi
		;;
	add-provider) add_client_provider ;;
	client-config) configure_client_sessions ;;
	provider) manage_provider ;;
	manage) manage_provider ;;
	update) update_binary_only ;;
	source) choose_release_source ;;
	status) show_status ;;
	migrate) migrate_legacy ;;
	legacy-stop) stop_legacy_interactive ;;
	legacy-remove) remove_legacy_interactive ;;
	legacy-purge) purge_legacy_data ;;
	'') interactive_menu ;;
	*)
		usage >&2
		exit 2
		;;
	esac
}

if [ "${NIULANG_MANAGER_TESTING:-0}" != 1 ]; then
	trap cleanup 0 HUP INT TERM
	main "$@"
fi
