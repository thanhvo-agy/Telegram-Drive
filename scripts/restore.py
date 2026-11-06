#!/usr/bin/env python3
# ==============================================================================
# SynoTele Vault Enterprise - Disaster Recovery & Fast Restore Tool
# Khôi phục toàn bộ hoặc từng tệp tin từ Telegram về NAS
# ==============================================================================
import os
import sys
import json
import shutil
import argparse
import tempfile
import subprocess
import urllib.request
import urllib.parse
from datetime import datetime

ROOT_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DATA_DIR = os.path.join(ROOT_DIR, "data")
ENV_FILE = os.path.join(ROOT_DIR, ".env")
MANIFEST_FILE = os.path.join(DATA_DIR, "vault_manifest.json")

def load_env():
    env = {}
    if os.path.exists(ENV_FILE):
        with open(ENV_FILE, "r", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith("#") and "=" in line:
                    k, v = line.split("=", 1)
                    env[k.strip()] = v.strip().strip('"').strip("'")
    return env

def decrypt_file(in_path, out_path, password):
    cmd = [
        "openssl", "enc", "-d", "-aes-256-cbc",
        "-salt", "-pbkdf2",
        "-in", in_path,
        "-out", out_path,
        "-pass", f"pass:{password}"
    ]
    res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if res.returncode != 0:
        raise RuntimeError(f"OpenSSL decryption failed: {res.stderr.decode()}")
    return out_path

def download_telegram_file(token, file_id, dest_path):
    # 1. getFile
    url = f"https://api.telegram.org/bot{token}/getFile?file_id={file_id}"
    with urllib.request.urlopen(url, timeout=30) as resp:
        res = json.loads(resp.read().decode())
    if not res.get("ok"):
        raise RuntimeError(f"Telegram getFile error: {res.get('description')}")
    
    file_path = res["result"]["file_path"]
    download_url = f"https://api.telegram.org/file/bot{token}/{file_path}"
    
    # 2. Download binary stream
    with urllib.request.urlopen(download_url, timeout=120) as r, open(dest_path, "wb") as f:
        while chunk := r.read(65536):
            f.write(chunk)
    return dest_path

def main():
    parser = argparse.ArgumentParser(description="SynoTele Vault - Khôi Phục Dữ Liệu Từ Telegram")
    parser.add_argument("--token", help="Telegram Bot Token (hoặc tự đọc từ .env)")
    parser.add_argument("--file", help="Đường dẫn tương đối của tệp cần khôi phục (để trống nếu khôi phục tất cả)")
    parser.add_argument("--dest", default="./restore_output", help="Thư mục nhận dữ liệu khôi phục")
    parser.add_argument("--password", help="Mật khẩu mã hoá AES-256 (tự sinh từ Token nếu để trống)")
    args = parser.parse_args()

    env = load_env()
    token = args.token or env.get("TELEGRAM_BOT_TOKEN")
    if not token:
        print("❌ Lỗi: Cần cung cấp Telegram Bot Token (qua --token hoặc trong .env)")
        sys.exit(1)

    password = args.password or env.get("ENCRYPTION_PASSWORD")
    if not password and token:
        import hashlib
        password = hashlib.sha256(f"SynoTeleVaultSalt_{token}".encode()).hexdigest()[:32]

    if not os.path.exists(MANIFEST_FILE):
        print(f"❌ Lỗi: Không tìm thấy tệp kê khai {MANIFEST_FILE}")
        sys.exit(1)

    with open(MANIFEST_FILE, "r", encoding="utf-8") as f:
        manifest = json.load(f)

    files_map = manifest.get("files", {})
    if not files_map:
        print("⚠️ Kho lưu trữ trống. Chưa có tệp nào được sao lưu.")
        sys.exit(0)

    target_files = []
    if args.file:
        if args.file in files_map:
            target_files.append((args.file, files_map[args.file]))
        else:
            print(f"❌ Không tìm thấy tệp [{args.file}] trong danh mục sao lưu!")
            sys.exit(1)
    else:
        target_files = list(files_map.items())

    os.makedirs(args.dest, exist_ok=True)
    print("=================================================================")
    print(f"  🚀 SynoTele Vault - Khởi Chạy Khôi Phục Dữ Liệu")
    print(f"  Tổng số tệp cần khôi phục: {len(target_files)}")
    print(f"  Thư mục đích:              {os.path.abspath(args.dest)}")
    print("=================================================================")

    success_count = 0
    for rel_path, file_meta in target_files:
        dest_file_path = os.path.join(args.dest, rel_path)
        os.makedirs(os.path.dirname(dest_file_path), exist_ok=True)
        chunks = file_meta.get("chunks", [])
        chunks = sorted(chunks, key=lambda c: c.get("part", 1))

        print(f"📥 Đang tải [{rel_path}] ({len(chunks)} phần)...", end="", flush=True)

        temp_decrypted_parts = []
        try:
            for chunk in chunks:
                with tempfile.NamedTemporaryFile("wb", delete=False) as f_enc, \
                     tempfile.NamedTemporaryFile("wb", delete=False) as f_dec:
                    enc_path = f_enc.name
                    dec_path = f_dec.name

                download_telegram_file(token, chunk["file_id"], enc_path)
                decrypt_file(enc_path, dec_path, password)
                temp_decrypted_parts.append(dec_path)
                if os.path.exists(enc_path):
                    os.unlink(enc_path)

            # Reassemble parts
            with open(dest_file_path, "wb") as f_out:
                for dec_part in temp_decrypted_parts:
                    with open(dec_part, "rb") as f_in:
                        while b := f_in.read(65536):
                            f_out.write(b)

            print(" [OK]")
            success_count += 1
        except Exception as e:
            print(f" [LỖI: {e}]")
        finally:
            for p in temp_decrypted_parts:
                try:
                    if os.path.exists(p):
                        os.unlink(p)
                except Exception:
                    pass

    print("=================================================================")
    print(f"🎉 Khôi phục hoàn tất: {success_count}/{len(target_files)} tệp thành công!")
    print(f"📂 Dữ liệu đã sẵn sàng tại: {os.path.abspath(args.dest)}")

if __name__ == "__main__":
    main()
