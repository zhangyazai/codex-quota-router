#!/bin/sh
set -eu

LABEL="com.codex-quota-router"
WEB_URL="http://127.0.0.1:4000/"
HEALTH_URL="http://127.0.0.1:4000/healthz"
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_BIN="$SCRIPT_DIR/codex-quota-router"
APP_DIR="$HOME/Applications/Codex Quota Router.app"
CONTENTS_DIR="$APP_DIR/Contents"
INSTALL_BIN="$CONTENTS_DIR/MacOS/codex-quota-router"
INFO_PLIST="$CONTENTS_DIR/Info.plist"
LAUNCH_AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST_FILE="$LAUNCH_AGENT_DIR/$LABEL.plist"
CONFIG_DIR="$HOME/Library/Application Support/codex-quota-router"
DOMAIN="gui/$(id -u)"
SERVICE_TARGET="$DOMAIN/$LABEL"

require_macos() {
	if [ "$(uname -s)" != "Darwin" ]; then
		echo "此脚本只能在 macOS 上运行。" >&2
		exit 1
	fi
}

require_health_client() {
	if ! command -v curl >/dev/null 2>&1; then
		echo "需要 curl 执行本机健康检查。" >&2
		exit 1
	fi
}

open_web() {
	open "$WEB_URL" >/dev/null 2>&1
}

health_ok() {
	curl -fsS --max-time 2 "$HEALTH_URL" | grep -Eq '"service"[[:space:]]*:[[:space:]]*"codex-quota-router"'
}

service_loaded() {
	launchctl print "$SERVICE_TARGET" >/dev/null 2>&1
}

service_running() {
	launchctl print "$SERVICE_TARGET" 2>/dev/null | grep -Eq '^[[:space:]]*state = running$'
}

stop_service() {
	if service_loaded; then
		launchctl bootout "$DOMAIN" "$PLIST_FILE" >/dev/null
	fi
}

wait_for_health() {
	attempts=0
	while [ "$attempts" -lt 20 ]; do
		if ! service_loaded; then
			echo "服务未保持运行。" >&2
			return 1
		fi
		if health_ok; then
			return 0
		fi
		attempts=$((attempts + 1))
		sleep 1
	done
	echo "服务启动后未通过健康检查。" >&2
	return 1
}

write_info_plist() {
	{
		printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>'
		printf '%s\n' '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
		printf '%s\n' '<plist version="1.0">'
		printf '%s\n' '<dict>'
		printf '%s\n' '  <key>CFBundleExecutable</key>'
		printf '%s\n' '  <string>codex-quota-router</string>'
		printf '%s\n' '  <key>CFBundleIdentifier</key>'
		printf '%s\n' '  <string>com.codex-quota-router</string>'
		printf '%s\n' '  <key>CFBundleInfoDictionaryVersion</key>'
		printf '%s\n' '  <string>6.0</string>'
		printf '%s\n' '  <key>CFBundleName</key>'
		printf '%s\n' '  <string>Codex Quota Router</string>'
		printf '%s\n' '  <key>CFBundlePackageType</key>'
		printf '%s\n' '  <string>APPL</string>'
		printf '%s\n' '  <key>LSUIElement</key>'
		printf '%s\n' '  <true/>'
		printf '%s\n' '  <key>NSHighResolutionCapable</key>'
		printf '%s\n' '  <true/>'
		printf '%s\n' '</dict>'
		printf '%s\n' '</plist>'
	} >"$INFO_PLIST"
	chmod 0644 "$INFO_PLIST"
}

write_launch_agent() {
	escaped_bin=$(printf '%s' "$INSTALL_BIN" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')
	umask 077
	{
		printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>'
		printf '%s\n' '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
		printf '%s\n' '<plist version="1.0">'
		printf '%s\n' '<dict>'
		printf '%s\n' '  <key>Label</key>'
		printf '  <string>%s</string>\n' "$LABEL"
		printf '%s\n' '  <key>ProgramArguments</key>'
		printf '%s\n' '  <array>'
		printf '    <string>%s</string>\n' "$escaped_bin"
		printf '%s\n' '  </array>'
		printf '%s\n' '  <key>LimitLoadToSessionType</key>'
		printf '%s\n' '  <string>Aqua</string>'
		printf '%s\n' '  <key>RunAtLoad</key>'
		printf '%s\n' '  <true/>'
		printf '%s\n' '  <key>KeepAlive</key>'
		printf '%s\n' '  <dict>'
		printf '%s\n' '    <key>SuccessfulExit</key>'
		printf '%s\n' '    <false/>'
		printf '%s\n' '  </dict>'
		printf '%s\n' '  <key>ThrottleInterval</key>'
		printf '%s\n' '  <integer>2</integer>'
		printf '%s\n' '</dict>'
		printf '%s\n' '</plist>'
	} >"$PLIST_FILE"
}

install_router() {
	require_health_client
	if [ ! -f "$SOURCE_BIN" ]; then
		echo "未找到 $SOURCE_BIN" >&2
		echo "请先将 macOS 可执行文件放在 router.sh 同一目录。" >&2
		exit 1
	fi

	stop_service
	install -d -m 0755 "$CONTENTS_DIR/MacOS" "$LAUNCH_AGENT_DIR"
	install -m 0755 "$SOURCE_BIN" "$INSTALL_BIN"
	write_info_plist
	write_launch_agent

	if ! launchctl bootstrap "$DOMAIN" "$PLIST_FILE"; then
		stop_service
		exit 1
	fi
	if ! wait_for_health; then
		stop_service
		exit 1
	fi
	open_web
	echo "已安装并启动。管理页：$WEB_URL"
}

start_router() {
	require_health_client
	if [ ! -f "$PLIST_FILE" ]; then
		echo "尚未安装，请先运行 install。" >&2
		exit 1
	fi
	if service_loaded; then
		launchctl kickstart "$SERVICE_TARGET" >/dev/null
	else
		launchctl bootstrap "$DOMAIN" "$PLIST_FILE"
	fi
	if ! wait_for_health; then
		stop_service
		exit 1
	fi
	open_web
	echo "已启动。管理页：$WEB_URL"
}

stop_router() {
	stop_service
	echo "已停止。"
}

status_router() {
	require_health_client
	if ! service_loaded || ! service_running; then
		echo "未运行。"
		exit 1
	fi
	if health_ok; then
		echo "运行中。管理页：$WEB_URL"
		return
	fi
	echo "服务已启动，但健康检查失败。" >&2
	exit 1
}

uninstall_router() {
	stop_service
	rm -f -- "$PLIST_FILE"
	rm -rf -- "$APP_DIR"
}

require_macos
command=${1:-install}
case "$command" in
	install)
		install_router
		;;
	start)
		start_router
		;;
	stop)
		stop_router
		;;
	status)
		status_router
		;;
	uninstall)
		uninstall_router
		echo "已卸载；配置保留在 $CONFIG_DIR"
		;;
	purge)
		uninstall_router
		rm -rf -- "$CONFIG_DIR"
		echo "已卸载并删除默认配置。"
		;;
	*)
		echo "用法：$0 [install|start|stop|status|uninstall|purge]" >&2
		exit 2
		;;
esac
