#!/usr/bin/env bash
# ==============================================================================
# Telegram NAS Backup - Automated Initialization Script
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

echo "======================================================================"
echo "    🚀 Telegram NAS Backup (Teldrive + Rclone Crypt) Initializer"
echo "======================================================================"

# 1. Create required runtime directories
echo "[1/4] Tạo các thư mục lưu trữ cần thiết..."
mkdir -p data logs rclone

# 2. Check for OpenSSL
if ! command -v openssl >/dev/null 2>&1; then
    echo "⚠️ OpenSSL không được tìm thấy. Sẽ dùng fallback để sinh chuỗi ngẫu nhiên."
    GEN_RANDOM() { cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 32 | head -n 1; }
else
    GEN_RANDOM() { openssl rand -hex 16; }
    GEN_JWT() { openssl rand -hex 32; }
fi

# 3. Create .env if not exists
if [ ! -f .env ]; then
    echo "[2/4] Khởi tạo tệp .env..."
    cp .env.example .env
    
    RANDOM_PG_PASS="$(GEN_RANDOM)"
    # Replace default password in .env
    sed -i "s/replace_with_a_secure_password_here/${RANDOM_PG_PASS}/g" .env
    echo "  -> Đã tạo .env với mật khẩu database ngẫu nhiên."
else
    echo "[2/4] Tệp .env đã tồn tại. Bỏ qua bước tạo mới."
fi

# 4. Create config.toml if not exists
if [ ! -f config.toml ]; then
    echo "[3/4] Khởi tạo tệp config.toml cho Teldrive..."
    cp config.example.toml config.toml
    
    JWT_SECRET="$(GEN_JWT 2>/dev/null || GEN_RANDOM)"
    # Extract PG password from .env if available
    PG_PASS="$(grep '^POSTGRES_PASSWORD=' .env | cut -d '=' -f2- || echo "$RANDOM_PG_PASS")"
    
    sed -i "s/replace_with_a_secure_password_here/${PG_PASS}/g" config.toml
    sed -i "s/generate_random_32_hex_jwt_secret/${JWT_SECRET}/g" config.toml
    echo "  -> Đã tạo config.toml và đồng bộ mật khẩu database."
else
    echo "[3/4] Tệp config.toml đã tồn tại. Bỏ qua bước tạo mới."
fi

echo "[4/4] Kiểm tra cấu hình..."
echo ""
echo "======================================================================"
echo "    ✅ KHỞI TẠO HOÀN TẤT!"
echo "======================================================================"
echo "Các bước tiếp theo cần thực hiện:"
echo "1. Mở file 'config.toml' và điền Telegram credentials:"
echo "   - app-id & app-hash (lấy tại https://my.telegram.org)"
echo "   - allowed-users (username Telegram của bạn, ví dụ: [\"my_username\"])"
echo ""
echo "2. Mở file '.env' và chỉnh đường dẫn thư mục NAS cần backup:"
echo "   - NAS_SOURCE_PATH=/volume1/data"
echo ""
echo "3. Khởi động hệ thống bằng lệnh:"
echo "   docker compose up -d"
echo ""
echo "4. Mở trình duyệt truy cập: http://<IP_NAS>:8080 để đăng nhập Telegram."
echo "======================================================================"
