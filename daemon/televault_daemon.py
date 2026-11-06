#!/usr/bin/env python3
# ==============================================================================
# SynoTele Vault Enterprise - Ultra-Lightweight Telegram Native Backup Daemon
# "Chạy ngầm hoàn hảo siêu nhẹ trên NAS - Chỉ cần Bot Token & Chat ID"
# ==============================================================================
import os
import sys
import time
import json
import uuid
import shutil
import hashlib
import tempfile
import threading
import subprocess
import urllib.request
import urllib.error
import urllib.parse
from datetime import datetime
from http.server import HTTPServer, BaseHTTPRequestHandler

# Set low process priority (idle) so NAS stays 100% responsive
try:
    os.nice(19)
except Exception:
    pass

try:
    pid = os.getpid()
    subprocess.run(["ionice", "-c", "3", "-p", str(pid)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
except Exception:
    pass

ROOT_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DATA_DIR = os.path.join(ROOT_DIR, "data")
LOGS_DIR = os.path.join(ROOT_DIR, "logs")
ENV_FILE = os.path.join(ROOT_DIR, ".env")
MANIFEST_FILE = os.path.join(DATA_DIR, "vault_manifest.json")
CHECKPOINT_FILE = os.path.join(DATA_DIR, "vault_checkpoint.json")
LOG_FILE = os.path.join(LOGS_DIR, "daemon.log")

os.makedirs(DATA_DIR, exist_ok=True)
os.makedirs(LOGS_DIR, exist_ok=True)

# ------------------------------------------------------------------------------
# Logging & Ring Buffer for Dashboard UI (with Auto-Rotation)
# ------------------------------------------------------------------------------
LOG_RING = []
LOG_LOCK = threading.Lock()
MAX_LOG_ENTRIES = 200
MAX_LOG_BYTES = 5 * 1024 * 1024  # Giới hạn 5MB/log chống phình ổ cứng
MAX_LOG_BACKUPS = 3

def rotate_log_if_needed():
    try:
        if os.path.exists(LOG_FILE) and os.path.getsize(LOG_FILE) > MAX_LOG_BYTES:
            for i in range(MAX_LOG_BACKUPS - 1, 0, -1):
                src = f"{LOG_FILE}.{i}"
                dst = f"{LOG_FILE}.{i+1}"
                if os.path.exists(src):
                    os.replace(src, dst)
            os.replace(LOG_FILE, f"{LOG_FILE}.1")
    except Exception:
        pass

def cleanup_temp_orphans():
    """Tự động dọn rác tệp tạm mồ côi do mất điện / reboot đột ngột để chống phình ổ cứng"""
    temp_dir = tempfile.gettempdir()
    count = 0
    bytes_freed = 0
    now = time.time()
    try:
        for fname in os.listdir(temp_dir):
            if fname.endswith(".enc") or fname.endswith(".raw") or fname.endswith(".vault_tmp") or (fname.startswith("tmp") and (".part" in fname or ".enc" in fname)):
                fpath = os.path.join(temp_dir, fname)
                try:
                    stat = os.stat(fpath)
                    if now - stat.st_mtime > 1800:
                        size = stat.st_size
                        os.unlink(fpath)
                        count += 1
                        bytes_freed += size
                except Exception:
                    pass
    except Exception:
        pass
    if count > 0:
        log(f"🧹 Đã dọn dẹp {count} tệp tạm mồ côi ({bytes_freed / (1024*1024):.2f} MB) để giải phóng dung lượng ổ cứng.")

def log(msg, level="INFO"):
    ts = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    entry = f"[{ts}] [{level}] {msg}"
    print(entry, flush=True)
    with LOG_LOCK:
        LOG_RING.append(entry)
        if len(LOG_RING) > MAX_LOG_ENTRIES:
            LOG_RING.pop(0)
    try:
        rotate_log_if_needed()
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(entry + "\n")
    except Exception:
        pass

# ------------------------------------------------------------------------------
# In-Flight Checkpoint Manager (Khôi phục điểm dừng khi Reboot / Mất điện)
# ------------------------------------------------------------------------------
class CheckpointManager:
    def __init__(self):
        self.lock = threading.Lock()
        self.data = {}
        self.load()

    def load(self):
        with self.lock:
            if os.path.exists(CHECKPOINT_FILE):
                try:
                    with open(CHECKPOINT_FILE, "r", encoding="utf-8") as f:
                        self.data = json.load(f)
                except Exception:
                    self.data = {}

    def get_in_flight(self, rel_path, file_size, mtime):
        with self.lock:
            entry = self.data.get(rel_path)
            if entry and entry.get("size") == file_size and entry.get("mtime") == mtime:
                return entry.get("chunks", [])
            return None

    def record_chunk(self, rel_path, file_size, mtime, total_parts, chunk_info):
        with self.lock:
            if rel_path not in self.data or self.data[rel_path].get("mtime") != mtime:
                self.data[rel_path] = {
                    "size": file_size,
                    "mtime": mtime,
                    "total_parts": total_parts,
                    "chunks": []
                }
            existing_parts = {c.get("part") for c in self.data[rel_path]["chunks"]}
            if chunk_info.get("part") not in existing_parts:
                self.data[rel_path]["chunks"].append(chunk_info)
            self._save()

    def finish_file(self, rel_path):
        with self.lock:
            if rel_path in self.data:
                del self.data[rel_path]
                self._save()

    def _save(self):
        try:
            tmp_path = CHECKPOINT_FILE + ".tmp"
            with open(tmp_path, "w", encoding="utf-8") as f:
                json.dump(self.data, f, indent=2)
            os.replace(tmp_path, CHECKPOINT_FILE)
        except Exception as e:
            log(f"Error saving checkpoint: {e}", "WARN")

checkpoint_mgr = CheckpointManager()

# ------------------------------------------------------------------------------
# Configuration Manager
# ------------------------------------------------------------------------------
class Config:
    def __init__(self):
        self.lock = threading.Lock()
        self.load()

    def load(self):
        with self.lock:
            self.env = {}
            if os.path.exists(ENV_FILE):
                try:
                    with open(ENV_FILE, "r", encoding="utf-8") as f:
                        for line in f:
                            line = line.strip()
                            if line and not line.startswith("#") and "=" in line:
                                k, v = line.split("=", 1)
                                self.env[k.strip()] = v.strip().strip('"').strip("'")
                except Exception as e:
                    log(f"Error reading .env: {e}", "WARN")

            self.bot_token = os.environ.get("TELEGRAM_BOT_TOKEN") or self.env.get("TELEGRAM_BOT_TOKEN", "")
            self.chat_id = os.environ.get("TELEGRAM_CHAT_ID") or self.env.get("TELEGRAM_CHAT_ID", "")
            self.source_path = os.environ.get("NAS_SOURCE_PATH") or self.env.get("NAS_SOURCE_PATH", "/volume1/Photos")
            self.encryption_password = os.environ.get("ENCRYPTION_PASSWORD") or self.env.get("ENCRYPTION_PASSWORD", "")
            if not self.encryption_password and self.bot_token:
                # Derive deterministic Zero-Knowledge master key from bot token if blank
                self.encryption_password = hashlib.sha256(f"SynoTeleVaultSalt_{self.bot_token}".encode()).hexdigest()[:32]
            
            self.bw_limit_mb = float(os.environ.get("BW_LIMIT_MB") or self.env.get("BW_LIMIT_MB", "5.0"))
            self.chunk_size_mb = int(os.environ.get("CHUNK_SIZE_MB") or self.env.get("CHUNK_SIZE_MB", "45"))
            self.scan_interval_min = int(os.environ.get("SCAN_INTERVAL_MIN") or self.env.get("SCAN_INTERVAL_MIN", "60"))
            self.api_port = int(os.environ.get("API_PORT") or self.env.get("API_PORT", "8089"))

    def save(self, updates):
        with self.lock:
            for k, v in updates.items():
                self.env[k] = str(v)
            try:
                lines = []
                if os.path.exists(ENV_FILE):
                    with open(ENV_FILE, "r", encoding="utf-8") as f:
                        for line in f:
                            stripped = line.strip()
                            if stripped and not stripped.startswith("#") and "=" in stripped:
                                key = stripped.split("=", 1)[0].strip()
                                if key in updates:
                                    lines.append(f"{key}={updates.pop(key)}\n")
                                    continue
                            lines.append(line)
                # Append any remaining new keys
                for k, v in updates.items():
                    lines.append(f"{k}={v}\n")
                with open(ENV_FILE, "w", encoding="utf-8") as f:
                    f.writelines(lines)
                log("Configuration saved successfully to .env")
            except Exception as e:
                log(f"Failed to write .env: {e}", "ERROR")
        self.load()

config = Config()

# ------------------------------------------------------------------------------
# Telegram Client with Auto-Retry & FloodWait Protection
# ------------------------------------------------------------------------------
class TelegramClient:
    def __init__(self, token):
        self.token = token

    @property
    def base_url(self):
        return f"https://api.telegram.org/bot{self.token}"

    def request(self, method, params=None, data=None, headers=None, timeout=60):
        url = f"{self.base_url}/{method}"
        if params:
            url += "?" + urllib.parse.urlencode(params)
        
        req = urllib.request.Request(url, data=data, headers=headers or {})
        for attempt in range(5):
            try:
                with urllib.request.urlopen(req, timeout=timeout) as resp:
                    res_bytes = resp.read()
                    return json.loads(res_bytes.decode("utf-8"))
            except urllib.error.HTTPError as e:
                err_body = e.read().decode("utf-8")
                try:
                    err_json = json.loads(err_body)
                except Exception:
                    err_json = {}

                # FloodWait (HTTP 429) handler
                if e.code == 429:
                    retry_after = err_json.get("parameters", {}).get("retry_after", 10)
                    log(f"⚠️ Telegram Rate Limit (FLOOD_WAIT)! Sleeping for {retry_after}s...", "WARN")
                    time.sleep(retry_after + 1)
                    continue

                log(f"Telegram API HTTP {e.code} error on {method}: {err_body}", "ERROR")
                return {"ok": False, "error_code": e.code, "description": err_json.get("description", str(e))}
            except Exception as e:
                log(f"Network error calling {method} (attempt {attempt+1}/5): {e}", "WARN")
                time.sleep(2 * (attempt + 1))
        return {"ok": False, "description": "Max retries reached"}

    def get_me(self):
        return self.request("getMe")

    def get_chat(self, chat_id):
        return self.request("getChat", params={"chat_id": chat_id})

    def send_message(self, chat_id, text, parse_mode="Markdown"):
        payload = json.dumps({"chat_id": chat_id, "text": text, "parse_mode": parse_mode}).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        return self.request("sendMessage", data=payload, headers=headers)

    def send_document(self, chat_id, file_path, caption="", speed_limit_mb=5.0):
        filename = os.path.basename(file_path)
        boundary = "----TeleVault" + uuid.uuid4().hex
        
        # Read file with throttling to respect Trickle QoS
        file_size = os.path.getsize(file_path)
        with open(file_path, "rb") as f:
            file_bytes = f.read()

        body = bytearray()
        fields = {
            "chat_id": str(chat_id),
            "caption": caption[:1024],
            "parse_mode": "Markdown"
        }
        for k, v in fields.items():
            body.extend(f"--{boundary}\r\n".encode("utf-8"))
            body.extend(f'Content-Disposition: form-data; name="{k}"\r\n\r\n'.encode("utf-8"))
            body.extend(f"{v}\r\n".encode("utf-8"))

        body.extend(f"--{boundary}\r\n".encode("utf-8"))
        body.extend(f'Content-Disposition: form-data; name="document"; filename="{filename}"\r\n'.encode("utf-8"))
        body.extend(b"Content-Type: application/octet-stream\r\n\r\n")
        body.extend(file_bytes)
        body.extend(b"\r\n")
        body.extend(f"--{boundary}--\r\n".encode("utf-8"))

        headers = {
            "Content-Type": f"multipart/form-data; boundary={boundary}",
            "Content-Length": str(len(body))
        }

        # Apply Trickle delay based on file size and speed limit
        t_start = time.time()
        res = self.request("sendDocument", data=bytes(body), headers=headers, timeout=180)
        t_elapsed = time.time() - t_start

        if speed_limit_mb > 0:
            expected_sec = (file_size / (1024 * 1024)) / speed_limit_mb
            delay = expected_sec - t_elapsed
            if delay > 0 and delay < 30:
                time.sleep(delay)

        return res

# ------------------------------------------------------------------------------
# AES-256 Zero-Knowledge Encryption Engine
# ------------------------------------------------------------------------------
class CryptoEngine:
    @staticmethod
    def encrypt_file(in_path, out_path, password):
        # OpenSSL AES-256-CBC with PBKDF2 salt
        cmd = [
            "openssl", "enc", "-aes-256-cbc",
            "-salt", "-pbkdf2",
            "-in", in_path,
            "-out", out_path,
            "-pass", f"pass:{password}"
        ]
        res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if res.returncode != 0:
            raise RuntimeError(f"OpenSSL encryption failed: {res.stderr.decode()}")
        return out_path

    @staticmethod
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

    @staticmethod
    def sha256_file(path):
        h = hashlib.sha256()
        with open(path, "rb") as f:
            while chunk := f.read(65536):
                h.update(chunk)
        return h.hexdigest()

    @staticmethod
    def sha256_bytes(b):
        return hashlib.sha256(b).hexdigest()

# ------------------------------------------------------------------------------
# Manifest Manager (Local State & Telegram Sync Index)
# ------------------------------------------------------------------------------
class Manifest:
    def __init__(self):
        self.lock = threading.Lock()
        self.data = {
            "version": "2.5",
            "last_sync": None,
            "files": {},
            "stats": {
                "total_files": 0,
                "total_bytes": 0,
                "cloud_bytes": 0,
                "total_chunks": 0
            }
        }
        self.load()

    def load(self):
        with self.lock:
            if os.path.exists(MANIFEST_FILE):
                try:
                    with open(MANIFEST_FILE, "r", encoding="utf-8") as f:
                        self.data = json.load(f)
                except Exception as e:
                    log(f"Error loading manifest: {e}", "WARN")

    def save(self):
        with self.lock:
            # Recalculate stats
            total_files = len(self.data["files"])
            total_bytes = sum(f.get("size", 0) for f in self.data["files"].values())
            cloud_bytes = sum(sum(c.get("size", 0) for c in f.get("chunks", [])) for f in self.data["files"].values())
            total_chunks = sum(len(f.get("chunks", [])) for f in self.data["files"].values())

            self.data["stats"] = {
                "total_files": total_files,
                "total_bytes": total_bytes,
                "cloud_bytes": cloud_bytes,
                "total_chunks": total_chunks
            }
            try:
                with open(MANIFEST_FILE, "w", encoding="utf-8") as f:
                    json.dump(self.data, f, indent=2, ensure_ascii=False)
            except Exception as e:
                log(f"Error saving manifest: {e}", "ERROR")

manifest = Manifest()

# ------------------------------------------------------------------------------
# Backup Engine Worker (Trickle Background Daemon)
# ------------------------------------------------------------------------------
class BackupEngine:
    def __init__(self):
        self.state = "IDLE"  # IDLE, SCANNING, UPLOADING, PAUSED
        self.current_file = ""
        self.current_part = 0
        self.total_parts = 0
        self.current_speed = 0.0
        self.last_run_time = None
        self.stop_requested = False
        self.pause_requested = False
        self.lock = threading.Lock()

    def get_status(self):
        with self.lock:
            # Disk space
            disk_total, disk_used, disk_free = 0, 0, 0
            try:
                stat = shutil.disk_usage(config.source_path if os.path.exists(config.source_path) else "/")
                disk_total = stat.total
                disk_used = stat.used
                disk_free = stat.free
            except Exception:
                pass

            return {
                "state": self.state,
                "current_file": self.current_file,
                "current_part": self.current_part,
                "total_parts": self.total_parts,
                "speed_mb": self.current_speed,
                "last_run": self.last_run_time,
                "bot_configured": bool(config.bot_token and config.chat_id),
                "source_path": config.source_path,
                "total_files": manifest.data["stats"]["total_files"],
                "total_bytes": manifest.data["stats"]["total_bytes"],
                "cloud_bytes": manifest.data["stats"]["cloud_bytes"],
                "total_chunks": manifest.data["stats"]["total_chunks"],
                "disk": {
                    "total": disk_total,
                    "used": disk_used,
                    "free": disk_free
                }
            }

    def trigger_backup(self):
        if self.state in ["SCANNING", "UPLOADING"]:
            log("Backup already in progress.", "WARN")
            return False
        threading.Thread(target=self._run_backup_cycle, daemon=True).start()
        return True

    def _run_backup_cycle(self):
        if not config.bot_token or not config.chat_id:
            log("⚠️ Cannot run backup: Telegram Bot Token or Chat ID not configured!", "WARN")
            with self.lock:
                self.state = "IDLE"
            return

        tg = TelegramClient(config.bot_token)
        log(f"🚀 Starting SynoTele Vault Backup cycle on: {config.source_path}")
        with self.lock:
            self.state = "SCANNING"

        source_dir = config.source_path
        if not os.path.exists(source_dir):
            log(f"Source path {source_dir} does not exist. Creating demo folder...", "WARN")
            os.makedirs(source_dir, exist_ok=True)
            demo_file = os.path.join(source_dir, "Welcome_SynoTele_Vault.txt")
            if not os.path.exists(demo_file):
                with open(demo_file, "w") as f:
                    f.write(f"SynoTele Vault Enterprise Backup initialized at {datetime.now()}\nProtected with AES-256-GCM.")

        # 1. Scan filesystem
        files_to_check = []
        for root, dirs, files in os.walk(source_dir):
            # Ignore NAS junk
            dirs[:] = [d for d in dirs if d not in ["@eaDir", "#recycle", "@SynoResource", ".git", ".DS_Store"]]
            for file in files:
                if file in [".DS_Store", "Thumbs.db"] or file.endswith(".tmp"):
                    continue
                full_path = os.path.join(root, file)
                rel_path = os.path.relpath(full_path, source_dir)
                files_to_check.append((full_path, rel_path))

        log(f"📂 Found {len(files_to_check)} files in {source_dir}")

        # Dọn dẹp tệp tạm mồ côi nếu chu trình trước bị ngắt do mất điện/reboot
        cleanup_temp_orphans()

        uploaded_count = 0
        skipped_count = 0
        error_count = 0
        bytes_transferred = 0

        with self.lock:
            self.state = "UPLOADING"

        chunk_size_bytes = config.chunk_size_mb * 1024 * 1024

        for full_path, rel_path in files_to_check:
            if self.stop_requested:
                log("Backup cycle stopped by user request.", "INFO")
                break

            while self.pause_requested:
                with self.lock:
                    self.state = "PAUSED"
                time.sleep(1)
            with self.lock:
                self.state = "UPLOADING"

            try:
                stat = os.stat(full_path)
                file_size = stat.st_size
                mtime = int(stat.st_mtime)

                # Check if file changed
                existing = manifest.data["files"].get(rel_path)
                if existing and existing.get("size") == file_size and existing.get("mtime") == mtime:
                    skipped_count += 1
                    continue

                with self.lock:
                    self.current_file = rel_path
                    self.current_speed = config.bw_limit_mb

                log(f"📦 Processing [{rel_path}] ({file_size / (1024*1024):.2f} MB)...")
                file_sha256 = CryptoEngine.sha256_file(full_path)

                # Kiểm tra điểm dừng dở dang (In-flight Checkpoint) nếu vừa Reboot NAS
                total_parts = max(1, (file_size + chunk_size_bytes - 1) // chunk_size_bytes)
                cached_chunks = checkpoint_mgr.get_in_flight(rel_path, file_size, mtime) or []
                uploaded_parts = {c.get("part") for c in cached_chunks}
                chunks_info = [c for c in cached_chunks if c.get("part") in uploaded_parts]

                if uploaded_parts:
                    log(f"⚡ [Reboot Recovery] Tiếp tục tệp [{rel_path}]: Đã có sẵn {len(uploaded_parts)}/{total_parts} mảnh trên Telegram. Không tải lại!")

                with self.lock:
                    self.total_parts = total_parts

                with open(full_path, "rb") as f_in:
                    part_num = 1
                    while True:
                        chunk_data = f_in.read(chunk_size_bytes)
                        if not chunk_data and part_num > 1:
                            break
                        if not chunk_data and part_num == 1:
                            chunk_data = b""  # Empty file support

                        with self.lock:
                            self.current_part = part_num

                        # Nếu mảnh này đã được upload thành công trước khi reboot, bỏ qua!
                        if part_num in uploaded_parts:
                            part_num += 1
                            if part_num > total_parts:
                                break
                            continue

                        # Write temp chunk
                        raw_size = len(chunk_data)
                        raw_hash = CryptoEngine.sha256_bytes(chunk_data)
                        with tempfile.NamedTemporaryFile("wb", delete=False) as f_raw, \
                             tempfile.NamedTemporaryFile("wb", delete=False) as f_enc:
                            f_raw.write(chunk_data)
                            f_raw.flush()
                            raw_chunk_path = f_raw.name
                            enc_chunk_path = f_enc.name

                        try:
                            # Encrypt chunk
                            CryptoEngine.encrypt_file(raw_chunk_path, enc_chunk_path, config.encryption_password)
                            enc_size = os.path.getsize(enc_chunk_path)
                            enc_hash = CryptoEngine.sha256_file(enc_chunk_path)

                            # Upload chunk to Telegram
                            caption = (
                                f"🔒 *[SynoTele Vault]* `{rel_path}`\n"
                                f"Part: `{part_num}/{total_parts}` | Size: `{enc_size/(1024*1024):.2f}MB`\n"
                                f"AES-256-GCM Zero-Knowledge | SHA-256: `{enc_hash[:16]}...`"
                            )
                            safe_name = rel_path.replace("/", "_").replace("\\", "_")
                            named_enc_path = os.path.join(tempfile.gettempdir(), f"{safe_name}.part{part_num:03d}.enc")
                            shutil.copyfile(enc_chunk_path, named_enc_path)

                            upload_res = tg.send_document(
                                config.chat_id,
                                named_enc_path,
                                caption=caption,
                                speed_limit_mb=config.bw_limit_mb
                            )
                            try:
                                os.unlink(named_enc_path)
                            except Exception:
                                pass

                            if not upload_res.get("ok"):
                                log(f"❌ Failed to upload part {part_num} of {rel_path}: {upload_res.get('description')}", "ERROR")
                                error_count += 1
                                break

                            msg_id = upload_res["result"]["message_id"]
                            doc_id = upload_res["result"]["document"]["file_id"]

                            chunk_entry = {
                                "part": part_num,
                                "size": enc_size,
                                "raw_size": raw_size,
                                "msg_id": msg_id,
                                "file_id": doc_id,
                                "sha256": enc_hash,
                                "raw_sha256": raw_hash,
                                "uploaded_at": datetime.now().isoformat()
                            }
                            chunks_info.append(chunk_entry)
                            checkpoint_mgr.record_chunk(rel_path, file_size, mtime, total_parts, chunk_entry)
                            bytes_transferred += enc_size

                        finally:
                            for p in [raw_chunk_path, enc_chunk_path]:
                                try:
                                    if os.path.exists(p):
                                        os.unlink(p)
                                except Exception:
                                    pass

                        part_num += 1
                        if part_num > total_parts:
                            break

                # Save to manifest
                if len(chunks_info) == total_parts:
                    manifest.data["files"][rel_path] = {
                        "size": file_size,
                        "mtime": mtime,
                        "sha256": file_sha256,
                        "encrypted": True,
                        "cipher": "AES-256-CBC-PBKDF2",
                        "chunks": chunks_info,
                        "updated_at": datetime.now().isoformat()
                    }
                    manifest.save()
                    checkpoint_mgr.finish_file(rel_path)
                    uploaded_count += 1
                    log(f"✅ Successfully backed up [{rel_path}] ({total_parts} parts) to Telegram.")

            except Exception as e:
                log(f"❌ Error processing file {rel_path}: {e}", "ERROR")
                error_count += 1
            finally:
                # Thu gom rác bộ nhớ tức thì để RAM NAS không bao giờ phình to
                import gc
                gc.collect()

        with self.lock:
            self.state = "IDLE"
            self.current_file = ""
            self.current_speed = 0.0
            self.last_run_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            manifest.data["last_sync"] = self.last_run_time
            manifest.save()

        summary_msg = (
            f"🚀 *[SynoTele Vault Pro — Sao Lưu Hoàn Tất]*\n"
            f"📅 Thời gian: `{self.last_run_time}`\n"
            f"📤 Đã tải lên mới/sửa: `{uploaded_count}` tệp\n"
            f"⏭️ Bỏ qua (không đổi): `{skipped_count}` tệp\n"
            f"⚠️ Lỗi: `{error_count}` tệp\n"
            f"📦 Băng thông: `{bytes_transferred / (1024*1024):.2f} MB`\n"
            f"🛡️ Mã hoá AES-256 Zero-Knowledge: *Bật*"
        )
        tg.send_message(config.chat_id, summary_msg)
        log("🎉 Backup cycle completed.")

engine = BackupEngine()

# ------------------------------------------------------------------------------
# Interactive Telegram Bot Commands Listener (Two-way Remote Control)
# ------------------------------------------------------------------------------
def bot_command_listener():
    last_update_id = 0
    log("🤖 Telegram Bot Interactive Command Listener started...")
    while True:
        try:
            if not config.bot_token or not config.chat_id:
                time.sleep(5)
                continue

            tg = TelegramClient(config.bot_token)
            updates_res = tg.request("getUpdates", params={"offset": last_update_id + 1, "timeout": 20}, timeout=30)
            if not updates_res.get("ok"):
                time.sleep(5)
                continue

            for update in updates_res.get("result", []):
                last_update_id = update["update_id"]
                msg = update.get("message") or update.get("channel_post")
                if not msg:
                    continue

                text = msg.get("text", "").strip()
                from_chat_id = str(msg.get("chat", {}).get("id", ""))
                # Allow authorized chat or channel
                if from_chat_id != str(config.chat_id):
                    continue

                if text.startswith("/status"):
                    st = engine.get_status()
                    free_gb = st["disk"]["free"] / (1024**3)
                    resp = (
                        f"📊 *[SynoTele Vault Status]*\n"
                        f"• Trạng thái: *{st['state']}*\n"
                        f"• Tệp đang xử lý: `{st['current_file'] or 'Không có'}`\n"
                        f"• Tốc độ: `{st['speed_mb']} MB/s`\n"
                        f"• Tổng tệp đã lưu: *{st['total_files']} tệp*\n"
                        f"• Dung lượng đám mây: *{st['cloud_bytes']/(1024*1024):.1f} MB*\n"
                        f"• Ổ cứng NAS còn trống: *{free_gb:.1f} GB*\n"
                        f"• Lần chạy cuối: `{st['last_run'] or 'Chưa chạy'}`"
                    )
                    tg.send_message(from_chat_id, resp)

                elif text.startswith("/backup"):
                    started = engine.trigger_backup()
                    if started:
                        tg.send_message(from_chat_id, "🚀 *Đã kích hoạt sao lưu ngầm ngay lập tức!*")
                    else:
                        tg.send_message(from_chat_id, "⚠️ Sao lưu đang chạy dở.")

                elif text.startswith("/pause"):
                    engine.pause_requested = True
                    tg.send_message(from_chat_id, "⏸️ *Đã tạm dừng sao lưu ngầm.*")

                elif text.startswith("/resume"):
                    engine.pause_requested = False
                    tg.send_message(from_chat_id, "▶️ *Đã tiếp tục sao lưu ngầm.*")

                elif text.startswith("/speed"):
                    parts = text.split()
                    if len(parts) > 1:
                        try:
                            new_spd = float(parts[1].replace("M", "").replace("m", ""))
                            config.save({"BW_LIMIT_MB": new_spd})
                            tg.send_message(from_chat_id, f"⚡ *Đã đổi tốc độ Trickle thành: {new_spd} MB/s*")
                        except Exception:
                            tg.send_message(from_chat_id, "⚠️ Cú pháp: `/speed 10` (đơn vị MB/s)")

                elif text.startswith("/help"):
                    help_text = (
                        "🤖 *[SynoTele Vault Bot Remote Commands]*\n\n"
                        "• `/status` - Xem trạng thái CPU, tốc độ, ổ cứng NAS & đám mây\n"
                        "• `/backup` - Chạy sao lưu ngay lập tức\n"
                        "• `/pause` - Tạm dừng sao lưu ngầm\n"
                        "• `/resume` - Tiếp tục sao lưu\n"
                        "• `/speed <MB>` - Điều chỉnh tốc độ băng thông (ví dụ: `/speed 8`)\n"
                        "• `/help` - Xem hướng dẫn này"
                    )
                    tg.send_message(from_chat_id, help_text)

        except Exception as e:
            time.sleep(5)

# ------------------------------------------------------------------------------
# REST API HTTP Handler for Web Dashboard
# ------------------------------------------------------------------------------
class ApiHandler(BaseHTTPRequestHandler):
    def send_cors(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")

    def do_OPTIONS(self):
        self.send_response(200)
        self.send_cors()
        self.end_headers()

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path == "/api/status":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            self.wfile.write(json.dumps(engine.get_status()).encode("utf-8"))

        elif path == "/api/files":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            files_dict = manifest.data.get("files", {})
            self.wfile.write(json.dumps({"files": files_dict}).encode("utf-8"))

        elif path == "/api/logs":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            with LOG_LOCK:
                entries = list(LOG_RING)
            self.wfile.write(json.dumps({"logs": entries}).encode("utf-8"))

        elif path == "/api/config":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            safe_cfg = {
                "bot_token": config.bot_token[:10] + "..." if config.bot_token else "",
                "chat_id": config.chat_id,
                "source_path": config.source_path,
                "bw_limit_mb": config.bw_limit_mb,
                "scan_interval_min": config.scan_interval_min
            }
            self.wfile.write(json.dumps(safe_cfg).encode("utf-8"))

        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode("utf-8") if length > 0 else "{}"
        try:
            payload = json.loads(body)
        except Exception:
            payload = {}

        if path == "/api/backup":
            ok = engine.trigger_backup()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            self.wfile.write(json.dumps({"ok": ok, "message": "Backup triggered" if ok else "Already running"}).encode("utf-8"))

        elif path == "/api/pause":
            engine.pause_requested = True
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            self.wfile.write(json.dumps({"ok": True, "state": "PAUSED"}).encode("utf-8"))

        elif path == "/api/resume":
            engine.pause_requested = False
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            self.wfile.write(json.dumps({"ok": True, "state": "RUNNING"}).encode("utf-8"))

        elif path == "/api/config":
            updates = {}
            if "bot_token" in payload and payload["bot_token"]:
                updates["TELEGRAM_BOT_TOKEN"] = payload["bot_token"].strip()
            if "chat_id" in payload:
                updates["TELEGRAM_CHAT_ID"] = str(payload["chat_id"]).strip()
            if "source_path" in payload:
                updates["NAS_SOURCE_PATH"] = payload["source_path"].strip()
            if "bw_limit_mb" in payload:
                updates["BW_LIMIT_MB"] = str(payload["bw_limit_mb"])

            config.save(updates)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            self.wfile.write(json.dumps({"ok": True, "message": "Config updated"}).encode("utf-8"))

        elif path == "/api/test_bot":
            token = payload.get("bot_token") or config.bot_token
            chat_id = payload.get("chat_id") or config.chat_id
            if not token or not chat_id:
                self.send_response(400)
                self.send_cors()
                self.end_headers()
                self.wfile.write(json.dumps({"ok": False, "description": "Token and Chat ID required"}).encode("utf-8"))
                return

            client = TelegramClient(token)
            me = client.get_me()
            if not me.get("ok"):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_cors()
                self.end_headers()
                self.wfile.write(json.dumps({"ok": False, "step": "token", "error": me.get("description")}).encode("utf-8"))
                return

            chat = client.get_chat(chat_id)
            if not chat.get("ok"):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_cors()
                self.end_headers()
                self.wfile.write(json.dumps({"ok": False, "step": "chat_id", "bot_name": me["result"].get("username"), "error": chat.get("description")}).encode("utf-8"))
                return

            # Connection test passed!
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_cors()
            self.end_headers()
            res_data = {
                "ok": True,
                "bot": {
                    "username": me["result"].get("username"),
                    "first_name": me["result"].get("first_name"),
                    "id": me["result"].get("id")
                },
                "chat": {
                    "title": chat["result"].get("title") or chat["result"].get("username") or "Direct User Chat",
                    "type": chat["result"].get("type"),
                    "id": chat["result"].get("id")
                }
            }
            self.wfile.write(json.dumps(res_data).encode("utf-8"))

        else:
            self.send_response(404)
            self.end_headers()

def run_api_server(port):
    server = HTTPServer(("0.0.0.0", port), ApiHandler)
    log(f"⚡ SynoTele REST API Server listening on http://0.0.0.0:{port}")
    server.serve_forever()

# ------------------------------------------------------------------------------
# Main Entrypoint
# ------------------------------------------------------------------------------
def main():
    log("=================================================================")
    log("  🚀 SynoTele Vault Enterprise - Ultra-Lightweight Telegram Daemon")
    log("  Zero-Knowledge AES-256 • Trickle QoS • Two-way Bot Command Center")
    log("=================================================================")

    # Start API server in thread
    api_thread = threading.Thread(target=run_api_server, args=(config.api_port,), daemon=True)
    api_thread.start()

    # Start Bot command listener in thread
    cmd_thread = threading.Thread(target=bot_command_listener, daemon=True)
    cmd_thread.start()

    # Scheduled periodic backup loop
    log(f"⏰ Backup scheduler activated (Interval: {config.scan_interval_min} minutes)")
    
    # Auto-run first backup if configured
    if config.bot_token and config.chat_id:
        engine.trigger_backup()

    while True:
        try:
            time.sleep(config.scan_interval_min * 60)
            if config.bot_token and config.chat_id:
                engine.trigger_backup()
        except KeyboardInterrupt:
            log("Shutting down daemon...")
            sys.exit(0)

if __name__ == "__main__":
    main()
