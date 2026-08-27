#!/bin/sh
# Interactive Linux installer and service console for Niulang protocol 2.
# POSIX sh is intentional: the same file works with dash and BusyBox ash.
set -eu

REPOSITORY=${NIULANG_REPOSITORY:-4fuu/niulang}
GITHUB_URL=${NIULANG_GITHUB_URL:-https://github.com}
GITHUB_API=${NIULANG_GITHUB_API:-https://api.github.com}
REQUESTED_VERSION=${NIULANG_VERSION:-latest}
SUPPLIED_BINARY=${NIULANG_BINARY:-}

SERVER_STATE=${NIULANG_STATE_DIR:-/var/lib/niulang/provider}
SERVER_USER=${NIULANG_SERVICE_USER:-niulang}
SERVER_BINARY=${NIULANG_INSTALLED_BINARY:-/usr/local/bin/niulangd}
SERVER_SERVICE=${NIULANG_SERVER_SERVICE:-niulangd}
CLIENT_SERVICE=${NIULANG_CLIENT_SERVICE:-niulang-client}
CLIENT_BINARY=${NIULANG_CLIENT_BINARY:-${HOME:?HOME must be set}/.niulang/bin/niulangd}

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

root_path() {
	printf '%s%s\n' "$test_root" "$1"
}

require_linux_systemd() {
	[ "$(uname -s)" = Linux ] || die "管理脚本仅支持 Linux；macOS 客户端请使用 deploy/install-client.sh"
	command -v systemctl >/dev/null 2>&1 || die "需要 systemd/systemctl"
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
	fetch_file "$GITHUB_API/repos/$REPOSITORY/releases/latest" "$TEMP_DIR/latest.json" >&2
	latest_tag=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		"$TEMP_DIR/latest.json" | sed -n '1p')
	validate_version "$latest_tag" || die "无法从 GitHub API 解析 $REPOSITORY 的最新稳定版本"
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
	[ -d "$state_path" ] || run_root test -d "$state_path"
}

install_server_interactive() {
	require_linux_systemd
	prepare_install_source
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
	require_linux_systemd
	[ "$(id -u)" -ne 0 ] || die "客户端必须由实际使用代理的登录用户安装，不能以 root 运行"
	prepare_install_source
	client_manifest=$(default_client_manifest)
	set -- "$INSTALL_DEPLOY/install-client.sh" --binary "$INSTALL_BINARY"
	if [ -f "$client_manifest" ]; then
		info "检测到现有 Niulang provider manifest，将保留 profile 并更新程序和服务"
	else
		client_invite=$(prompt_invitation)
		set -- "$@" --invite "$client_invite"
	fi
	client_local=$(prompt "外层连接绑定（auto、if:接口名或 IP）" auto)
	set -- "$@" --local-address "$client_local"
	"$@"
}

provider_run() {
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
	provider_run "$SERVER_BINARY" provider "$@" --state "$SERVER_STATE"
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
	server_state_exists "$SERVER_STATE" || die "找不到 provider state: $SERVER_STATE"
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
			[ -f "$legacy_system_path" ] && printf '    system service: %s\n' "$legacy_system_path"
			[ -f "$legacy_user_path" ] && printf '    user service:   %s\n' "$legacy_user_path"
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
		else
			if [ "$restore_enabled" = true ]; then
				systemctl --user enable "$restore_unit.service" >/dev/null 2>&1 || true
			fi
			if [ "$restore_active" = true ]; then
				systemctl --user start "$restore_unit.service" >/dev/null 2>&1 || true
			fi
		fi
	done <"$restore_file"
}

remove_legacy_role() {
	remove_role=$1
	stop_legacy_role "$remove_role"
	for remove_unit in $(legacy_units_for_role "$remove_role"); do
		remove_system_path=$(legacy_system_unit_path "$remove_unit")
		remove_user_path=$(legacy_user_unit_path "$remove_unit")
		if [ -f "$remove_system_path" ]; then
			run_root rm -f "$remove_system_path"
		fi
		if [ -f "$remove_user_path" ]; then
			rm -f "$remove_user_path"
		fi
	done
	run_root systemctl daemon-reload
	systemctl --user daemon-reload >/dev/null 2>&1 || true
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
	info "旧 Queqiao 数据已删除"
}

stop_new_role_after_failed_migration() {
	case $1 in
	server) run_root systemctl disable --now "$SERVER_SERVICE.service" >/dev/null 2>&1 || true ;;
	client) systemctl --user disable --now "$CLIENT_SERVICE.service" >/dev/null 2>&1 || true ;;
	esac
}

migrate_legacy_role() {
	migrate_role=$1
	if [ "$migrate_role" = client ] && [ "$(id -u)" -eq 0 ]; then
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
	require_linux_systemd
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
		if [ "$(id -u)" -eq 0 ]; then
			warn "检测到旧客户端。请以实际使用代理的登录用户重新运行 $0 migrate；root 不应持有客户端 profile。"
		elif confirm "迁移旧客户端？需要新的 Niulang 邀请。" yes; then
			migrate_legacy_role client
		fi
	fi
}

show_status() {
	require_linux_systemd
	printf 'Niulang repository: %s\n' "$REPOSITORY"
	if [ -x "$SERVER_BINARY" ]; then
		"$SERVER_BINARY" version || true
	else
		printf 'Server binary: 未安装（%s）\n' "$SERVER_BINARY"
	fi
	if [ -x "$CLIENT_BINARY" ]; then
		"$CLIENT_BINARY" version || true
	else
		printf 'Client binary: 未安装（%s）\n' "$CLIENT_BINARY"
	fi
	if systemctl is-active --quiet "$SERVER_SERVICE.service" 2>/dev/null; then
		printf 'Server service: active\n'
	else
		printf 'Server service: inactive/not installed\n'
	fi
	if [ "$(id -u)" -ne 0 ] && systemctl --user is-active --quiet "$CLIENT_SERVICE.service" 2>/dev/null; then
		printf 'Client service: active\n'
	else
		printf 'Client service: inactive/not installed for current user\n'
	fi
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
		cat >&2 <<'EOF'

Niulang Linux 管理工具
  1) 安装/更新服务端
  2) 安装/更新当前用户客户端
  3) 管理服务端用户、邀请和设备
  4) 查看 Niulang 和旧 Queqiao 状态
  5) 迁移旧 Queqiao 服务
  6) 停止并禁用旧 Queqiao 服务
  7) 删除旧 Queqiao 服务定义/二进制
  8) 永久删除旧 Queqiao state/profile/配置/日志
  0) 退出
EOF
		menu_choice=$(prompt "选择" 0)
		case $menu_choice in
		1) install_server_interactive ;;
		2) install_client_interactive ;;
		3) manage_provider ;;
		4) show_status ;;
		5) migrate_legacy ;;
		6) stop_legacy_interactive ;;
		7) remove_legacy_interactive ;;
		8) purge_legacy_data ;;
		0) return ;;
		*) warn "未知选项" ;;
		esac
	done
}

usage() {
	cat <<'EOF'
用法: deploy/manage.sh [server|client|provider|status|migrate|legacy-stop|legacy-remove|legacy-purge]

不带参数时进入交互菜单。管理器会下载 4fuu/niulang 最新稳定 Release、
校验 SHA256SUMS，并使用 Release 中同版本的安装脚本和二进制。

  server         安装或更新 systemd 服务端
  client         安装或更新当前登录用户的 systemd user 客户端
  provider       管理用户、邀请、设备和限制
  status         查看 Niulang 与旧 Queqiao 状态
  migrate        停止旧服务并迁移角色；state/profile 不复制
  legacy-stop    停止并禁用旧 Queqiao 服务，保留全部文件
  legacy-remove  删除旧服务定义，并可删除旧二进制，保留数据
  legacy-purge   强确认后永久删除旧 state/profile/配置和日志

环境变量：
  NIULANG_VERSION=latest       稳定 Release；也可指定 vYYYY.MDD.N
  NIULANG_BINARY=/path         使用本地 protocol 2 二进制
  NIULANG_REPOSITORY=owner/repo  Release 仓库（默认 4fuu/niulang）
  NIULANG_STATE_DIR=/path      Provider state 路径

旧 Queqiao 凭据无法转换。服务端迁移会创建新的 trust domain；客户端迁移
必须粘贴服务端新生成的 niulang://enroll/... 邀请链接。
EOF
}

main() {
	case ${1:-} in
	-h | --help | help)
		usage
		return
		;;
	esac
	require_linux_systemd
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
	provider) manage_provider ;;
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
