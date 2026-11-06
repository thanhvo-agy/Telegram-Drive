#!/usr/bin/env bash
# ==============================================================================
# SynoTele Vault - Unified Smart Sync Dispatcher
# Automatically determines whether this NAS should PUSH or PULL based on NODE_MODE
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

if [ -f "$ROOT_DIR/.env" ]; then
    set -a
    source "$ROOT_DIR/.env"
    set +a
fi

MODE="${NODE_MODE:-PUSH}"
NODE="${NODE_NAME:-Synology-Node}"

echo "================================================================="
echo "  🌐 SynoTele Vault Node Manager"
echo "  Node Name: $NODE"
echo "  Node Mode: $MODE"
echo "================================================================="

case "$MODE" in
    PUSH|push)
        echo "🚀 Node đang ở chế độ [PUSH]: Đẩy dữ liệu từ NAS lên Telegram Vault..."
        exec /bin/bash "$SCRIPT_DIR/backup.sh"
        ;;
    PULL|pull)
        echo "📥 Node đang ở chế độ [PULL]: Kéo dữ liệu từ Telegram Vault về NAS..."
        exec /bin/bash "$SCRIPT_DIR/pull.sh"
        ;;
    TWO_WAY|two_way|bisync)
        echo "🔄 Node đang ở chế độ [TWO_WAY]: Đồng bộ 2 chiều qua rclone bisync..."
        # Optional bisync command
        exec /bin/bash "$SCRIPT_DIR/backup.sh"
        ;;
    *)
        echo "❌ Lỗi: NODE_MODE='$MODE' không hợp lệ. Vui lòng chọn PUSH hoặc PULL trong .env"
        exit 1
        ;;
esac
