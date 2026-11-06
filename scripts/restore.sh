#!/usr/bin/env bash
# ==============================================================================
# Telegram NAS Backup - File Restore Script
# Restores files from encrypted Telegram cloud back to local NAS
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

RCLONE_CONF="$ROOT_DIR/rclone/rclone.conf"
REMOTE="${1:-teldrive-crypt:nas-backup}"
DEST="${2:-$ROOT_DIR/restore_output}"

echo "================================================================="
echo "  📥 RESTORE FILES FROM TELEGRAM"
echo "================================================================="
echo "  Remote Source:      $REMOTE"
echo "  Local Destination:  $DEST"
echo "================================================================="

if [ ! -f "$RCLONE_CONF" ]; then
    echo "❌ Không tìm thấy file $RCLONE_CONF"
    exit 1
fi

mkdir -p "$DEST"

echo "Bắt đầu tải về và giải mã tệp..."
rclone copy "$REMOTE" "$DEST" \
    --config "$RCLONE_CONF" \
    --progress \
    --transfers 4 \
    --checkers 8

echo ""
echo "✅ Tải về hoàn tất! Tệp tin đã được lưu tại: $DEST"
