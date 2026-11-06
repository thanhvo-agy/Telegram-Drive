#!/bin/sh
# ==============================================================================
# Containerized Rclone Backup Runner
# Executes backup inside the rclone-backup Docker container
# ==============================================================================
set -e

TIMESTAMP=$(date "+%Y-%m-%d_%H-%M-%S")
LOG_FILE="/logs/backup_${TIMESTAMP}.log"
START_TIME=$(date +%s)

echo "=================================================================" >> "$LOG_FILE"
echo "  🚀 Starting Scheduled Backup at $(date)" >> "$LOG_FILE"
echo "  Source: /source_data" >> "$LOG_FILE"
echo "  Destination: ${RCLONE_REMOTE}" >> "$LOG_FILE"
echo "=================================================================" >> "$LOG_FILE"

send_telegram_alert() {
    if [ -n "$TELEGRAM_BOT_TOKEN" ] && [ -n "$TELEGRAM_CHAT_ID" ]; then
        MESSAGE="$1"
        wget -qO- --post-data="chat_id=${TELEGRAM_CHAT_ID}&text=${MESSAGE}&parse_mode=Markdown" \
            "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" >/dev/null 2>&1 || true
    fi
}

# Run Rclone Sync with optimized flags for NAS backup
# - fast-list: reduces API calls
# - bwlimit: prevents bandwidth saturation
# - exclude common NAS metadata/temp files
rclone sync /source_data "${RCLONE_REMOTE}" \
    --config /config/rclone/rclone.conf \
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
END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

if [ $EXIT_CODE -eq 0 ]; then
    SUCCESS_MSG="✅ *NAS Backup to Telegram SUCCESS*%0A- Thời gian chạy: ${DURATION}s%0A- Đích: ${RCLONE_REMOTE}%0A- Log: backup_${TIMESTAMP}.log"
    echo "Backup completed successfully in ${DURATION}s." >> "$LOG_FILE"
    send_telegram_alert "$SUCCESS_MSG"
else
    FAIL_MSG="❌ *NAS Backup to Telegram FAILED*%0A- Exit code: ${EXIT_CODE}%0A- Thời gian chạy: ${DURATION}s%0A- Vui lòng kiểm tra log: backup_${TIMESTAMP}.log"
    echo "Backup failed with exit code ${EXIT_CODE}." >> "$LOG_FILE"
    send_telegram_alert "$FAIL_MSG"
fi

# Rotate log files older than 30 days
find /logs -name "backup_*.log" -type f -mtime +30 -delete 2>/dev/null || true

exit $EXIT_CODE
