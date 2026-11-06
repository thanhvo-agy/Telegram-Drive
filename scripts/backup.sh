#!/usr/bin/env bash
# ==============================================================================
# Host-level Rclone Backup Runner (Synology DSM / TrueNAS / Linux)
# Optimized for "Trickle Mode" (Chạy ngầm êm ái, nhường CPU & Disk cho NAS)
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
echo "  🚀 Starting Telegram NAS Backup (Trickle Mode) at $(date)" | tee -a "$LOG_FILE"
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

# 2. Setup Low Priority / Trickle execution for Synology DSM
# nice -n 19: Lowest CPU priority (yields to DSM, Plex, Photos)
# ionice -c 3: Idle I/O priority (only uses disk when system is idle)
PRIORITY_PREFIX=""
if command -v nice >/dev/null 2>&1; then
    PRIORITY_PREFIX="nice -n 19"
fi
if command -v ionice >/dev/null 2>&1; then
    PRIORITY_PREFIX="$PRIORITY_PREFIX ionice -c 3"
fi

# 3. Smart Bandwidth Throttling Timetable:
# Default: 5MB/s during the day (07:00-23:00) so family internet is never laggy,
# and 18MB/s during the night (23:00-07:00) when everyone is asleep.
BWLIMIT_CONFIG="${RCLONE_BWLIMIT:-07:00,5M 23:00,18M}"

echo "  Priority Command: ${PRIORITY_PREFIX:-Default}" | tee -a "$LOG_FILE"
echo "  Bandwidth Rule:   $BWLIMIT_CONFIG" | tee -a "$LOG_FILE"

set +e
$PRIORITY_PREFIX rclone sync "${NAS_SOURCE_PATH:-/volume1/data}" "${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}" \
    --config "$RCLONE_CONF" \
    --fast-list \
    --transfers "${RCLONE_TRANSFERS:-2}" \
    --checkers 4 \
    --bwlimit "$BWLIMIT_CONFIG" \
    --tpslimit 4 \
    --tpslimit-burst 6 \
    --exclude "@eaDir/**" \
    --exclude "@SynoResource/**" \
    --exclude "@tmp/**" \
    --exclude "#recycle/**" \
    --exclude ".SynologyWorkingDirectory/**" \
    --exclude ".DS_Store" \
    --exclude "Thumbs.db" \
    --exclude "*.tmp" \
    --retries 5 \
    --low-level-retries 10 \
    --stats 1m \
    --log-file "$LOG_FILE" \
    --log-level INFO
EXIT_CODE=$?
set -e

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

if [ $EXIT_CODE -eq 0 ]; then
    SUCCESS_MSG="✅ *Synology Backup to Telegram SUCCESS*%0A- Thời gian chạy: ${DURATION}s%0A- Đích: ${RCLONE_DEST_REMOTE:-teldrive-crypt:nas-backup}%0A- Log: backup_${TIMESTAMP}.log"
    echo "Backup completed successfully in ${DURATION}s." | tee -a "$LOG_FILE"
    send_telegram_alert "$SUCCESS_MSG"
else
    FAIL_MSG="❌ *Synology Backup to Telegram FAILED*%0A- Exit code: ${EXIT_CODE}%0A- Thời gian chạy: ${DURATION}s%0A- Kiểm tra log: backup_${TIMESTAMP}.log"
    echo "Backup failed with exit code ${EXIT_CODE}." | tee -a "$LOG_FILE"
    send_telegram_alert "$FAIL_MSG"
fi

# Rotate log files older than 30 days
find "$LOGS_DIR" -name "backup_*.log" -type f -mtime +30 -delete 2>/dev/null || true

exit $EXIT_CODE
