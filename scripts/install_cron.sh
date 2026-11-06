#!/usr/bin/env bash
# ==============================================================================
# Telegram NAS Backup - Cron Job Helper
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
BACKUP_CMD="$SCRIPT_DIR/backup.sh"

CRON_TIME="${1:-0 2 * * *}"
CRON_JOB="$CRON_TIME /bin/bash $BACKUP_CMD >/dev/null 2>&1"

echo "================================================================="
echo "  ⏰ CÀI ĐẶT CRONJOB BACKUP TỰ ĐỘNG CHO NAS"
echo "================================================================="
echo "Dòng lệnh cronjob đề xuất (chạy lúc 2h sáng hàng ngày):"
echo ""
echo "  $CRON_JOB"
echo ""
echo "-----------------------------------------------------------------"
echo "Cách thêm vào hệ thống:"
echo "1. Đối với Linux / TrueNAS / Unraid:"
echo "   Chạy: (crontab -l 2>/dev/null; echo \"$CRON_JOB\") | crontab -"
echo ""
echo "2. Đối với Synology DSM:"
echo "   Vào Control Panel -> Task Scheduler -> Create -> Scheduled Task -> User-defined script"
echo "   - Tab General: User = root"
echo "   - Tab Schedule: Daily lúc 02:00"
echo "   - Tab Task Settings: Dán lệnh: /bin/bash $BACKUP_CMD"
echo "================================================================="
