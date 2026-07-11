#!/bin/sh
set -eu

SERVICE_NAME="codex-quota-router.service"
WEB_URL="http://127.0.0.1:4000/"
HEALTH_URL="http://127.0.0.1:4000/healthz"
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_BIN="$SCRIPT_DIR/codex-quota-router"
INSTALL_DIR="$HOME/.local/lib/codex-quota-router"
INSTALL_BIN="$INSTALL_DIR/codex-quota-router"
CONFIG_HOME=${XDG_CONFIG_HOME:-"$HOME/.config"}
CONFIG_DIR="$CONFIG_HOME/codex-quota-router"
UNIT_DIR="$CONFIG_HOME/systemd/user"
UNIT_FILE="$UNIT_DIR/$SERVICE_NAME"

require_user_systemd() {
    if ! command -v systemctl >/dev/null 2>&1 || ! systemctl --user show-environment >/dev/null 2>&1; then
        echo "当前登录会话没有可用的 systemd 用户服务。" >&2
        exit 1
    fi
}

require_health_client() {
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        echo "需要 curl 或 wget 执行本机健康检查。" >&2
        exit 1
    fi
}

open_web() {
    if command -v xdg-open >/dev/null 2>&1; then
        xdg-open "$WEB_URL" >/dev/null 2>&1 &
    elif command -v gio >/dev/null 2>&1; then
        gio open "$WEB_URL" >/dev/null 2>&1 &
    elif command -v wslview >/dev/null 2>&1; then
        wslview "$WEB_URL" >/dev/null 2>&1 &
    elif command -v powershell.exe >/dev/null 2>&1; then
        powershell.exe -NoLogo -NoProfile -NonInteractive -Command "Start-Process '$WEB_URL'" >/dev/null 2>&1
    else
        echo "请打开 $WEB_URL"
    fi
}

health_ok() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS --max-time 2 "$HEALTH_URL" | grep -Eq '"service"[[:space:]]*:[[:space:]]*"codex-quota-router"'
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T 2 -O - "$HEALTH_URL" | grep -Eq '"service"[[:space:]]*:[[:space:]]*"codex-quota-router"'
    else
        return 2
    fi
}

wait_for_health() {
    attempts=0
    while [ "$attempts" -lt 20 ]; do
        if ! systemctl --user is-active --quiet "$SERVICE_NAME"; then
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

install_router() {
    require_user_systemd
    require_health_client
    if [ ! -f "$SOURCE_BIN" ]; then
        echo "未找到 $SOURCE_BIN" >&2
        echo "请先将 Linux 可执行文件放在 router.sh 同一目录。" >&2
        exit 1
    fi

    if [ -f "$UNIT_FILE" ]; then
        systemctl --user stop "$SERVICE_NAME" >/dev/null
    fi
    install -d -m 0700 "$INSTALL_DIR" "$UNIT_DIR"
    install -m 0755 "$SOURCE_BIN" "$INSTALL_BIN"

    umask 077
    cat >"$UNIT_FILE" <<'EOF'
[Unit]
Description=Codex quota router
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/lib/codex-quota-router/codex-quota-router
Restart=on-failure
RestartSec=2
UMask=0077
StandardOutput=null
StandardError=null

[Install]
WantedBy=default.target
EOF

    systemctl --user daemon-reload
    if ! systemctl --user enable --now "$SERVICE_NAME"; then
        systemctl --user disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
        exit 1
    fi
    if ! wait_for_health; then
        systemctl --user disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
        exit 1
    fi
    open_web
    echo "已安装并启动。管理页：$WEB_URL"
}

start_router() {
    require_user_systemd
    require_health_client
    if [ ! -f "$UNIT_FILE" ]; then
        echo "尚未安装，请先运行 install。" >&2
        exit 1
    fi
    systemctl --user start "$SERVICE_NAME"
    if ! wait_for_health; then
        systemctl --user stop "$SERVICE_NAME" >/dev/null 2>&1 || true
        exit 1
    fi
    open_web
    echo "已启动。管理页：$WEB_URL"
}

stop_router() {
    require_user_systemd
    systemctl --user stop "$SERVICE_NAME"
    echo "已停止。"
}

status_router() {
    require_user_systemd
    require_health_client
    if ! systemctl --user is-active --quiet "$SERVICE_NAME"; then
        echo "未运行。"
        exit 1
    fi

    if health_ok; then
        echo "运行中。管理页：$WEB_URL"
        return
    else
        health_status=$?
    fi

    if [ "$health_status" -eq 2 ]; then
        echo "服务运行中；未找到 curl 或 wget，未执行 HTTP 健康检查。"
        return
    fi
    echo "服务已启动，但健康检查失败。" >&2
    exit 1
}

uninstall_router() {
    require_user_systemd
    if [ -f "$UNIT_FILE" ]; then
        systemctl --user disable --now "$SERVICE_NAME" >/dev/null
    fi
    rm -f -- "$UNIT_FILE" "$INSTALL_BIN"
    rmdir "$INSTALL_DIR" >/dev/null 2>&1 || true
    systemctl --user daemon-reload
    systemctl --user reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
}

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
