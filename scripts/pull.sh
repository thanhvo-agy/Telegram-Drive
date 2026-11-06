#!/usr/bin/env bash
# ==============================================================================
# SynoTele Vault - PULL / REPLICA MODE RUNNER
# Run on Secondary NAS (Chi nhánh, Nhà riêng, Offsite Backup)
# Chỉ nhận kéo dữ liệu từ Telegram Cloud về NAS cục bộ
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

# Load environment configuration
if [ -f "$ROOT_DIR/.env" ]; then
    set -a
    source "$ROOT_DIR/.env"
    set +a
fi

LOGS_DIR="$ROOT_DIR/logs"
mkdir -p "$LOGS_DIR"

NODE_NAME="${NODE_NAME:-Synology-Replica-NAS}"
REMOTE_SOURCE="${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}"
LOCAL_DEST="${NAS_REPLICA_PATH:-/volume1/replica_data}"
PULL_STRATEGY="${PULL_STRATEGY:-SYNC}" # SYNC (Mirror) hoặc COPY (Append-Only)

TIMESTAMP=$(date "+%Y-%m-%d_%H-%M-%S")
LOG_FILE="$LOGS_DIR/pull_${TIMESTAMP}.log"
LOCK_FILE="/tmp/telegram_nas_pull.lock"

# 1. Prevent concurrent runs using lock file
exec 201>"$LOCK_FILE"
if ! flock -n 201; then
    echo "⚠️ Another PULL instance is already running on this NAS. Exiting."
    exit 1
fi

START_TIME=$(date +%s)
echo "=================================================================" | tee -a "$LOG_FILE"
echo "  📥 Starting SynoTele Vault PULL REPLICATION at $(date)" | tee -a "$LOG_FILE"
echo "  Node Identity: $NODE_NAME (MODE: PULL / REPLICA)" | tee -a "$LOG_FILE"
echo "  Remote Source: $REMOTE_SOURCE" | tee -a "$LOG_FILE"
echo "  Local Dest:    $LOCAL_DEST" | tee -a "$LOG_FILE"
echo "  Pull Strategy: $PULL_STRATEGY" | tee -a "$LOG_FILE"
echo "=================================================================" | tee -a "$LOG_FILE"

send_telegram_alert() {
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${TELEGRAM_CHAT_ID:-}" ]; then
        local MSG="$1"
        curl -s -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
            -d "chat_id=${TELEGRAM_CHAT_ID}" \
            -d "text=${MSG}" \
            -d "parse_mode=Markdown" >/dev/null 2>&1 || true
    fi
}

RCLONE_CONF="$ROOT_DIR/rclone/rclone.conf"
if [ ! -f "$RCLONE_CONF" ]; then
    echo "❌ Lỗi: Không tìm thấy file cấu hình Rclone tại $RCLONE_CONF" | tee -a "$LOG_FILE"
    exit 1
fi

mkdir -p "$LOCAL_DEST"

# Setup Low Priority for Synology DSM
PRIORITY_PREFIX=""
if command -v nice >/dev/null 2>&1; then
    PRIORITY_PREFIX="nice -n 19"
fi
if command -v ionice >/dev/null 2>&1; then
    PRIORITY_PREFIX="$PRIORITY_PREFIX ionice -c 3"
fi

BWLIMIT_CONFIG="${RCLONE_BWLIMIT:-07:00,5M 23:00,18M}"

# Choose between sync (mirror) or copy (keep old files even if deleted upstream)
RCLONE_ACTION="sync"
if [ "$PULL_STRATEGY" = "COPY" ]; then
    RCLONE_ACTION="copy"
fi

set +e
$PRIORITY_PREFIX rclone $RCLONE_ACTION "$REMOTE_SOURCE" "$LOCAL_DEST" \
    --config "$RCLONE_CONF" \
    --fast-list \
    --transfers "${RCLONE_TRANSFERS:-2}" \
    --checkers 4 \
    --bwlimit "$BWLIMIT_CONFIG" \
    --tpslimit 4 \
    --tpslimit-burst 6 \
    --exclude "@eaDir/**" \
    --exclude "#recycle/**" \
    --exclude ".DS_Store" \
    --retries 5 \
    --stats 1m \
    --log-file "$LOG_FILE" \
    --log-level INFO
EXIT_CODE=$?
set -e

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

if [ $EXIT_CODE -eq 0 ]; then
    SUCCESS_MSG="📥 *[${NODE_NAME}] PULL Hoàn Tất*%0A- Kéo từ Telegram Vault về NAS thành công!%0A- Thời gian: ${DURATION}s%0A- Thư mục đích: ${LOCAL_DEST}%0A- Log: pull_${TIMESTAMP}.log"
    echo "PULL completed successfully in ${DURATION}s." | tee -a "$LOG_FILE"
    send_telegram_alert "$SUCCESS_MSG"
else
    FAIL_MSG="❌ *[${NODE_NAME}] PULL Thất Bại*%0A- Exit code: ${EXIT_CODE}%0A- Thời gian: ${DURATION}s%0A- Kiểm tra log: pull_${TIMESTAMP}.log"
    echo "PULL failed with exit code ${EXIT_CODE}." | tee -a "$LOG_FILE"
    send_telegram_alert "$FAIL_MSG"
fi

# Rotate log files older than 30 days
find "$LOGS_DIR" -name "pull_*.log" -type f -mtime +30 -delete 2>/dev/null || true

exit $EXIT_CODE
