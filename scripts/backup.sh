#!/usr/bin/env bash
# ==============================================================================
# Host-level Rclone Backup Runner
# Run directly on Synology DSM Task Scheduler, TrueNAS, or Linux cron
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

# Load environment configuration if available
if [ -f "$ROOT_DIR/.env" ]; then
    set -a
    source "$ROOT_DIR/.env"
    set +a
fi

LOGS_DIR="$ROOT_DIR/logs"
mkdir -p "$LOGS_DIR"

TIMESTAMP=$(date "+%Y-%m-%d_%H-%M-%S")
LOG_FILE="$LOGS_DIR/backup_${TIMESTAMP}.log"
LOCK_FILE="/tmp/telegram_nas_backup.lock"

# 1. Prevent concurrent runs using lock file
exec 200>"$LOCK_FILE"
if ! flock -n 200; then
    echo "⚠️ Another backup instance is already running. Exiting."
    exit 1
fi

START_TIME=$(date +%s)
echo "=================================================================" | tee -a "$LOG_FILE"
echo "  🚀 Starting Telegram NAS Backup at $(date)" | tee -a "$LOG_FILE"
echo "  Source:      ${NAS_SOURCE_PATH:-/volume1/data}" | tee -a "$LOG_FILE"
echo "  Destination: ${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}" | tee -a "$LOG_FILE"
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
    echo "Vui lòng copy 'rclone/rclone.example.conf' sang 'rclone/rclone.conf' và cấu hình." | tee -a "$LOG_FILE"
    exit 1
fi

set +e
rclone sync "${NAS_SOURCE_PATH:-/volume1/data}" "${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}" \
    --config "$RCLONE_CONF" \
    --fast-list \
    --transfers "${RCLONE_TRANSFERS:-4}" \
    --checkers 8 \
    --bwlimit "${RCLONE_BWLIMIT:-15M}" \
    --exclude "@eaDir/**" \
    --exclude ".DS_Store" \
    --exclude "Thumbs.db" \
    --exclude "@SynoResource/**" \
    --exclude "*.tmp" \
    --retries 3 \
    --stats 1m \
    --log-file "$LOG_FILE" \
    --log-level INFO
EXIT_CODE=$?
set -e

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

if [ $EXIT_CODE -eq 0 ]; then
    SUCCESS_MSG="✅ *NAS Backup to Telegram SUCCESS*%0A- Thời gian chạy: ${DURATION}s%0A- Đích: ${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}%0A- Log: backup_${TIMESTAMP}.log"
    echo "Backup completed successfully in ${DURATION}s." | tee -a "$LOG_FILE"
    send_telegram_alert "$SUCCESS_MSG"
else
    FAIL_MSG="❌ *NAS Backup to Telegram FAILED*%0A- Exit code: ${EXIT_CODE}%0A- Thời gian chạy: ${DURATION}s%0A- Kiểm tra log: backup_${TIMESTAMP}.log"
    echo "Backup failed with exit code ${EXIT_CODE}." | tee -a "$LOG_FILE"
    send_telegram_alert "$FAIL_MSG"
fi

# Rotate log files older than 30 days
find "$LOGS_DIR" -name "backup_*.log" -type f -mtime +30 -delete 2>/dev/null || true

exit $EXIT_CODE
