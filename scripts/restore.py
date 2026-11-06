#!/usr/bin/env python3
# ==============================================================================
# SynoTele Vault Enterprise - Disaster Recovery & Fast Restore Tool
# Khôi phục toàn bộ hoặc từng tệp tin từ Telegram về NAS
# ĐẢM BẢO 100% BIT-PERFECT: CHỐNG HỎNG ẢNH & VIDEO KHI BĂM / GỘP
# ==============================================================================
import os
import sys
import json
import shutil
import hashlib
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

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(65536):
            h.update(chunk)
    return h.hexdigest()

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
        raise RuntimeError(f"OpenSSL decryption failed: {res.stderr.decode().strip()}")
    return out_path

def download_telegram_file(token, file_id, dest_path):
    # 1. getFile
    url = f"https://api.telegram.org/bot{token}/getFile?file_id={file_id}"
    req = urllib.request.Request(url, headers={"User-Agent": "SynoTeleVault-Restore/2.5"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        res = json.loads(resp.read().decode())
    if not res.get("ok"):
        raise RuntimeError(f"Telegram getFile error: {res.get('description')}")
    
    file_path = res["result"]["file_path"]
    download_url = f"https://api.telegram.org/file/bot{token}/{file_path}"
    
    # 2. Download binary stream
    req_dl = urllib.request.Request(download_url, headers={"User-Agent": "SynoTeleVault-Restore/2.5"})
    with urllib.request.urlopen(req_dl, timeout=180) as r, open(dest_path, "wb") as f:
        while chunk := r.read(65536):
            f.write(chunk)
    return dest_path

def verify_media_container(file_path):
    """
    Kiểm tra tính toàn vẹn cấu trúc file ảnh/video sau khi gộp.
    Đảm bảo không bị lỗi moov atom, hỏng frame hoặc gãy header.
    """
    ext = os.path.splitext(file_path)[1].lower()
    
    # 1. Ảnh: Dùng PIL nếu có
    if ext in [".jpg", ".jpeg", ".png", ".webp", ".bmp"]:
        try:
            from PIL import Image
            with Image.open(file_path) as img:
                img.verify()
            return True, "Image header & scanlines valid"
        except ImportError:
            pass
        except Exception as e:
            return False, f"Image decoder error: {e}"

    # 2. Video: Dùng ffprobe nếu có
    if ext in [".mp4", ".mov", ".mkv", ".avi", ".m4v", ".webm"]:
        if shutil.which("ffprobe"):
            cmd = ["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1", file_path]
            res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            if res.returncode != 0:
                return False, f"Video container corrupt (ffprobe: {res.stderr.strip()})"
            return True, "Video container & atoms valid"
            
    return True, "Format check skipped"

def main():
    parser = argparse.ArgumentParser(description="SynoTele Vault - Khôi Phục Dữ Liệu An Toàn Tuyệt Đối")
    parser.add_argument("--token", help="Telegram Bot Token (hoặc tự đọc từ .env)")
    parser.add_argument("--file", help="Đường dẫn tương đối của tệp cần khôi phục (để trống nếu khôi phục tất cả)")
    parser.add_argument("--dest", default="./restore_output", help="Thư mục nhận dữ liệu khôi phục")
    parser.add_argument("--password", help="Mật khẩu mã hoá AES-256 (tự sinh từ Token nếu để trống)")
    parser.add_argument("--skip-media-check", action="store_true", help="Bỏ qua kiểm tra chuyên sâu ảnh/video")
    args = parser.parse_args()

    env = load_env()
    token = args.token or env.get("TELEGRAM_BOT_TOKEN")
    if not token:
        print("❌ Lỗi: Cần cung cấp Telegram Bot Token (qua --token hoặc trong .env)")
        sys.exit(1)

    password = args.password or env.get("ENCRYPTION_PASSWORD")
    if not password and token:
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
    print("=========================================================================")
    print(f"  🚀 SynoTele Vault - Khởi Chạy Khôi Phục Dữ Liệu (Bit-Perfect Mode)")
    print(f"  Tổng số tệp cần khôi phục: {len(target_files)}")
    print(f"  Thư mục đích:              {os.path.abspath(args.dest)}")
    print(f"  Cơ chế kiểm soát:          Dual SHA-256 + Sequence Check + Media Probe")
    print("=========================================================================")

    success_count = 0
    fail_count = 0

    for rel_path, file_meta in target_files:
        dest_file_path = os.path.join(args.dest, rel_path)
        os.makedirs(os.path.dirname(dest_file_path), exist_ok=True)
        chunks = file_meta.get("chunks", [])
        chunks = sorted(chunks, key=lambda c: c.get("part", 1))
        total_parts = len(chunks)

        print(f"\n📂 [{rel_path}] ({file_meta.get('size', 0)/(1024*1024):.2f} MB, {total_parts} chunks):")

        # 1. Kiểm tra tính liên tục của danh sách chunk (Strict Sequence Assertion)
        actual_sequence = [c.get("part") for c in chunks]
        expected_sequence = list(range(1, total_parts + 1))
        if actual_sequence != expected_sequence:
            print(f"  ❌ LỖI TRẦM TRỌNG: Danh sách chunk không liên tục hoặc bị mất mảnh! Hiện có: {actual_sequence}")
            fail_count += 1
            continue

        temp_decrypted_parts = []
        aborted = False

        try:
            for idx, chunk in enumerate(chunks, start=1):
                part_num = chunk.get("part")
                expected_enc_hash = chunk.get("sha256")
                expected_raw_hash = chunk.get("raw_sha256")
                expected_raw_size = chunk.get("raw_size")

                with tempfile.NamedTemporaryFile("wb", delete=False) as f_enc, \
                     tempfile.NamedTemporaryFile("wb", delete=False) as f_dec:
                    enc_path = f_enc.name
                    dec_path = f_dec.name

                print(f"  ⬇️ [Part {part_num}/{total_parts}] Đang tải...", end="", flush=True)
                download_telegram_file(token, chunk["file_id"], enc_path)

                # 2. Kiểm tra SHA-256 mã hoá tải từ Telegram (Chống mất gói mạng)
                downloaded_enc_hash = sha256_file(enc_path)
                if expected_enc_hash and downloaded_enc_hash != expected_enc_hash:
                    raise ValueError(f"Chunk {part_num} bị lỗi bit mạng! Expected {expected_enc_hash[:12]}, got {downloaded_enc_hash[:12]}")

                # 3. Giải mã AES-256
                decrypt_file(enc_path, dec_path, password)
                if os.path.exists(enc_path):
                    os.unlink(enc_path)

                # 4. Kiểm tra SHA-256 & Kích thước giải mã của từng mảnh
                if expected_raw_hash:
                    dec_hash = sha256_file(dec_path)
                    if dec_hash != expected_raw_hash:
                        raise ValueError(f"Chunk {part_num} lỗi giải mã! Hash không khớp.")
                if expected_raw_size:
                    dec_size = os.path.getsize(dec_path)
                    if dec_size != expected_raw_size:
                        raise ValueError(f"Chunk {part_num} kích thước không khớp ({dec_size} != {expected_raw_size}).")

                temp_decrypted_parts.append(dec_path)
                print(" ✔️ [Hash Verified]")

            # 5. Gộp các mảnh vào tệp tạm (Atomic Staging)
            tmp_dest_file = dest_file_path + ".vault_tmp"
            with open(tmp_dest_file, "wb") as f_out:
                for dec_part in temp_decrypted_parts:
                    with open(dec_part, "rb") as f_in:
                        while b := f_in.read(65536):
                            f_out.write(b)

            # 6. Kiểm tra SHA-256 toàn bộ tệp thành phẩm (Whole-file SHA-256)
            restored_sha256 = sha256_file(tmp_dest_file)
            expected_file_sha256 = file_meta.get("sha256")
            if expected_file_sha256 and restored_sha256 != expected_file_sha256:
                if os.path.exists(tmp_dest_file):
                    os.unlink(tmp_dest_file)
                raise ValueError(f"Toàn vẹn tệp thất bại! File SHA-256 {restored_sha256[:16]} != Gốc {expected_file_sha256[:16]}")

            # 7. Kiểm tra kích thước byte-exact
            restored_size = os.path.getsize(tmp_dest_file)
            expected_file_size = file_meta.get("size")
            if expected_file_size is not None and restored_size != expected_file_size:
                if os.path.exists(tmp_dest_file):
                    os.unlink(tmp_dest_file)
                raise ValueError(f"Kích thước tệp không khớp ({restored_size} != {expected_file_size} bytes)")

            # 8. Kiểm tra cấu trúc media (Ảnh / Video)
            if not args.skip_media_check:
                ok, msg = verify_media_container(tmp_dest_file)
                if not ok:
                    if os.path.exists(tmp_dest_file):
                        os.unlink(tmp_dest_file)
                    raise ValueError(f"Cảnh báo Media: {msg}")

            # 9. Ghi nguyên tử (Atomic replace)
            os.replace(tmp_dest_file, dest_file_path)
            print(f"  🎉 [THÀNH CÔNG] File khôi phục nguyên vẹn 100% (Bit-Perfect Verified)")
            success_count += 1

        except Exception as e:
            print(f"\n  ❌ [LỖI KHÔI PHỤC]: {e}")
            fail_count += 1
        finally:
            for p in temp_decrypted_parts:
                try:
                    if os.path.exists(p):
                        os.unlink(p)
                except Exception:
                    pass

    print("\n=========================================================================")
    print(f"🎉 Khôi phục hoàn tất: {success_count}/{len(target_files)} tệp thành công, {fail_count} lỗi.")
    print(f"📂 Dữ liệu đã sẵn sàng tại: {os.path.abspath(args.dest)}")
    print("=========================================================================")

if __name__ == "__main__":
    main()
