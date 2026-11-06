#!/usr/bin/env bash
# ==============================================================================
# SynoTele Vault - Zero-Config Quick Start (Chỉ Cần Token & Chat ID)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

echo "======================================================================"
echo "    🚀 SynoTele Vault - Khởi Tạo Siêu Nhẹ Cho NAS"
echo "    (Chỉ cần Telegram Bot Token & Chat ID - Chạy ngầm êm ái)"
echo "======================================================================"

TOKEN="${1:-}"
CHAT_ID="${2:-}"
SOURCE_PATH="${3:-/volume1/Photos}"

# Read existing from .env if available
if [ -f .env ]; then
    set -a
    source .env
    set +a
    TOKEN="${TOKEN:-${TELEGRAM_BOT_TOKEN:-}}"
    CHAT_ID="${CHAT_ID:-${TELEGRAM_CHAT_ID:-}}"
    SOURCE_PATH="${SOURCE_PATH:-${NAS_SOURCE_PATH:-/volume1/Photos}}"
fi

# Prompt interactively if still empty
if [ -z "$TOKEN" ]; then
    echo -n "👉 Nhập Telegram Bot Token (lấy từ @BotFather): "
    read -r TOKEN
fi

if [ -z "$CHAT_ID" ]; then
    echo -n "👉 Nhập Telegram Chat ID hoặc Channel ID (ví dụ: -1001234567890): "
    read -r CHAT_ID
fi

if [ -z "$TOKEN" ] || [ -z "$CHAT_ID" ]; then
    echo "❌ Lỗi: Cần cung cấp đầy đủ Bot Token và Chat ID!"
    exit 1
fi

echo ""
echo "🔍 Đang kiểm tra kết nối tới Telegram Cloud..."

# Test Bot Token
BOT_INFO=$(curl -s "https://api.telegram.org/bot${TOKEN}/getMe" || echo '{"ok":false}')
IS_OK=$(echo "$BOT_INFO" | grep -o '"ok":true' || true)

if [ -z "$IS_OK" ]; then
    echo "❌ Lỗi: Bot Token không hợp lệ hoặc không thể kết nối tới Telegram!"
    echo "Chi tiết: $BOT_INFO"
    exit 1
fi

BOT_USER=$(echo "$BOT_INFO" | grep -o '"username":"[^"]*' | cut -d'"' -f4 || echo "UnknownBot")
echo "  ✅ Bot Telegram: @${BOT_USER} (Xác thực thành công!)"

# Test Chat / Channel ID
CHAT_INFO=$(curl -s "https://api.telegram.org/bot${TOKEN}/getChat?chat_id=${CHAT_ID}" || echo '{"ok":false}')
CHAT_OK=$(echo "$CHAT_INFO" | grep -o '"ok":true' || true)

if [ -z "$CHAT_OK" ]; then
    echo "⚠️ Cảnh báo: Không thể lấy thông tin Chat ID '${CHAT_ID}'."
    echo "Hãy chắc chắn bạn đã thêm @${BOT_USER} làm Quản trị viên (Admin) của Kênh/Nhóm!"
else
    CHAT_TITLE=$(echo "$CHAT_INFO" | grep -o '"title":"[^"]*' | cut -d'"' -f4 || echo "Direct Chat")
    echo "  ✅ Kênh/Nhóm Lưu Trữ: '${CHAT_TITLE}' (Đã kết nối!)"
fi

# Write / Update .env
mkdir -p data logs rclone "$SOURCE_PATH"

if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || touch .env
fi

# Save config
python3 -c "
import os
updates = {
    'TELEGRAM_BOT_TOKEN': '${TOKEN}',
    'TELEGRAM_CHAT_ID': '${CHAT_ID}',
    'NAS_SOURCE_PATH': '${SOURCE_PATH}'
}
env_file = '.env'
lines = []
if os.path.exists(env_file):
    with open(env_file, 'r') as f:
        for l in f:
            k = l.split('=')[0].strip() if '=' in l and not l.strip().startswith('#') else None
            if k in updates:
                lines.append(f'{k}={updates.pop(k)}\n')
            else:
                lines.append(l)
for k, v in updates.items():
    lines.append(f'{k}={v}\n')
with open(env_file, 'w') as f:
    f.writelines(lines)
"

echo "  ✅ Đã lưu cấu hình vào .env"

# Send Test Welcome Message to Telegram
WELCOME_MSG="🚀 *[SynoTele Vault Enterprise Đã Kích Hoạt]*%0A• Máy chủ: \`$(hostname)\`%0A• Thư mục sao lưu: \`${SOURCE_PATH}\`%0A• Chế độ: *Chạy ngầm Trickle QoS (nice 19)*%0A• Bảo mật: *AES-256 Zero-Knowledge Encrypted*%0A%0A_Gõ /status để kiểm tra trạng thái từ điện thoại!_"
curl -s "https://api.telegram.org/bot${TOKEN}/sendMessage?chat_id=${CHAT_ID}&text=${WELCOME_MSG}&parse_mode=Markdown" >/dev/null 2>&1 || true

echo ""
echo "======================================================================"
echo "🎉 KHỞI TẠO HOÀN TẤT! ỨNG DỤNG ĐÃ SẴN SÀNG CHẠY NGẦM"
echo "======================================================================"
echo "👉 Cách 1 (Khuyên Dùng): Khởi động ngầm qua Docker:"
echo "   docker compose up -d"
echo ""
echo "👉 Cách 2: Chạy trực tiếp Python Daemon trên Synology/Linux:"
echo "   python3 daemon/televault_daemon.py &"
echo ""
echo "🌐 Truy cập Dashboard Quản Trị: http://<IP_NAS>:8088"
echo "📱 Điều khiển từ xa qua Telegram: Mở chat với @${BOT_USER} và gửi /status"
echo "======================================================================"
