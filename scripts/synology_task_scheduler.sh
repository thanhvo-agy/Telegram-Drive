#!/usr/bin/env bash
# ==============================================================================
# SynoTele Vault Enterprise - Synology DSM Auto-Start & Watchdog Sentinel
# Tự động khởi động khi NAS Reboot / Tự hồi sinh nếu bị sập / Chống phình RAM
# Dùng cho Synology DSM Task Scheduler (Control Panel > Task Scheduler)
# ==============================================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
DAEMON_SCRIPT="$ROOT_DIR/daemon/televault_daemon.py"
PID_FILE="$ROOT_DIR/data/televault.pid"
LOG_DIR="$ROOT_DIR/logs"

mkdir -p "$ROOT_DIR/data" "$LOG_DIR"

is_daemon_running() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE" 2>/dev/null || true)
        if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
            # Kiểm tra xem cổng 8089 có phản hồi HTTP không
            if curl -s -f -m 3 http://127.0.0.1:8089/api/status >/dev/null 2>&1; then
                return 0
            fi
        fi
    fi
    # Kiểm tra theo tên tiến trình ps
    if pgrep -f "televault_daemon.py" >/dev/null 2>&1; then
        return 0
    fi
    return 1
}

start_daemon() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [WATCHDOG] 🚀 Khởi động TeleVault Daemon trên Synology NAS..."
    cd "$ROOT_DIR"

    # Chạy ở độ ưu tiên thấp nhất (nice 19, ionice idle) để nhường toàn bộ tài nguyên cho DSM
    if command -v ionice >/dev/null 2>&1; then
        IONICE_CMD="ionice -c 3"
    else
        IONICE_CMD=""
    fi

    nohup nice -n 19 $IONICE_CMD python3 "$DAEMON_SCRIPT" >> "$LOG_DIR/daemon.log" 2>&1 &
    NEW_PID=$!
    echo "$NEW_PID" > "$PID_FILE"
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [WATCHDOG] ✅ Daemon đã chạy ngầm với PID: $NEW_PID (Nice: 19, Idle I/O)"
}

case "$1" in
    start)
        if is_daemon_running; then
            echo "Daemon đang chạy bình thường."
        else
            start_daemon
        fi
        ;;
    stop)
        echo "Dừng TeleVault Daemon..."
        pkill -f "televault_daemon.py" || true
        rm -f "$PID_FILE"
        echo "Đã dừng."
        ;;
    restart)
        $0 stop
        sleep 2
        $0 start
        ;;
    watchdog|status)
        if ! is_daemon_running; then
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] [WATCHDOG] ⚠️ Phát hiện Daemon chưa chạy hoặc bị ngắt! Đang tự động kích hoạt..."
            start_daemon
        else
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] [WATCHDOG] ✔️ Hệ thống hoạt động hoàn hảo."
        fi
        ;;
    *)
        echo "Sử dụng: $0 {start|stop|restart|watchdog}"
        echo ""
        echo "💡 HƯỚNG DẪN CÀI ĐẶT TỰ KHỞI ĐỘNG TRÊN SYNOLOGY DSM:"
        echo "1. Mở DSM > Control Panel (Bảng điều khiển) > Task Scheduler (Bộ lập lịch tác vụ)"
        echo "2. Chọn Create (Tạo) > Triggered Task (Tác vụ kích hoạt) > User-defined script"
        echo "3. Sự kiện (Event): Boot-up (Khởi động hệ thống)"
        echo "4. User: root (hoặc admin)"
        echo "5. Mục Run command dán lệnh: bash $ROOT_DIR/scripts/synology_task_scheduler.sh start"
        echo ""
        echo "💡 HƯỚNG DẪN CÀI ĐẶT WATCHDOG TỰ HỒI SINH (5 PHÚT/LẦN):"
        echo "1. Tạo Scheduled Task > User-defined script (Lịch chạy: Mỗi 5 phút)"
        echo "2. Mục Run command dán lệnh: bash $ROOT_DIR/scripts/synology_task_scheduler.sh watchdog"
        exit 1
        ;;
esac
