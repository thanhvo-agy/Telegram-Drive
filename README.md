# TeleClone Pro (tgdrive)

> **High-Performance, Zero-Dependency Telegram Cloud Backup Engine in the Spirit of Rclone.**
>
> **Single Static Binary | Zero Docker | Zero Database | 0MB RAM Idle | AES-256-GCM | Time Machine CoW Snapshots**

---

## 1. Overview & Architectural Philosophy

TeleClone (`tgdrive`) is built from the ground up on the **core philosophy of Rclone**:
* **Single Static Binary**: Written in pure Go with zero runtime dependencies. No Docker engine, no PostgreSQL, no Nginx, no Python/Node runtimes required. Just download one file and run.
* **0 MB RAM Background**: Runs cleanly via standard Cron or Task Scheduler. Sleeps at 0MB RAM, wakes up to perform scheduled trickle synchronization, releases 100% of resources upon completion, allowing NAS hard drives to hibernate normally.
* **Universal CPU Architecture**: Pre-compiled static binaries for `amd64` (Synology x86_64, Dell-1, Linux servers), `armv7` (Buffalo LS410D, LS220D), `armv5` (Buffalo LinkStation Quad ARM9), `arm64` (64-bit ARM / Raspberry Pi), and `darwin/arm64` (Apple Silicon Mac).
* **Entware / Native Compatibility**: Directly deployable on any embedded Linux, Buffalo NAS, OpenWrt router, or Synology DSM.

---

## 2. Key Capabilities

### Time Machine Multi-Version Snapshots
* **Content-Addressable CoW Deduplication**: Files are fingerprinted via SHA-256. Unchanged files across snapshot generations are deduplicated instantly (0 bytes re-read, 0 bytes encrypted, 0 bytes uploaded).
* **Point-in-Time Historical Recovery**: Restore any individual file, subfolder, or full snapshot to any point in history with 1 click.
* **Encrypted Cloud Manifest**: Every snapshot generates a signed and AES-256 encrypted manifest (`snap-*.manifest.enc`) stored directly in Telegram. Even if the local NAS hardware is completely lost, all historical snapshots can be reconstructed directly from the Telegram channel.

### Enterprise Security & QoS
* **Client-Side Zero-Knowledge Encryption**: All data is encrypted locally using **AES-256-GCM** with PBKDF2-SHA256 key derivation before leaving the NAS. Telegram servers only store opaque encrypted chunks.
* **Multi-Bot Token Mesh**: Multiplexes upload and download requests across a pool of Telegram Bot tokens in round-robin fashion, mitigating per-bot rate limits (`429 Too Many Requests`).
* **24h Trickle QoS Scheduler**: Dynamic bandwidth limits based on time of day (e.g., `07:00,5M 23:00,18M` for daytime 5MB/s and nighttime 18MB/s) combined with automatic low process priority (`nice -n 19`) to keep NAS responsive.
* **Smart Retention Policy**: Automated pruning rules (`keep-hourly`, `keep-daily`, `keep-weekly`, `keep-monthly`).

### Commercial-Grade Embedded Dashboard
* **Micro Web Server (<8MB RAM)**: Serves a high-density, Google Cloud Console-grade UI embedded directly within the binary.
* **Synology File Station Explorer**: Dual-pane directory tree and file table with real-time backup state and 1-click restore.
* **Time Machine Timeline**: Interactive snapshot scrubber with live diff inspection (Added, Modified, Deleted).
* **100% Vector SVG Icons**: Clean typography, zero emojis.

---

## 3. Quickstart & Installation

### Option A: 1-Line Universal Installer (Recommended)
On Synology DSM, Buffalo NAS (Entware), OpenWrt, or Linux:
```bash
curl -fsSL https://raw.githubusercontent.com/thanhvo-agy/Telegram-Drive/main/scripts/install.sh | sh
```

### Option B: Manual Binary Download
Download the single static binary matching your CPU architecture:

| Platform / Device | Binary Name | Architecture | Size |
| :--- | :--- | :--- | :--- |
| **Synology DSM / Dell-1 / x86_64 PC** | `bin/tgdrive-linux-amd64` | `x86_64` | ~7.2 MB |
| **Buffalo LS410D / LS220D / Pi 3/4** | `bin/tgdrive-linux-armv7` | `ARMv7-a` | ~7.2 MB |
| **Buffalo LinkStation Quad (Legacy)** | `bin/tgdrive-linux-armv5` | `ARMv5TE` | ~7.2 MB |
| **ARM64 Servers / Apple Silicon Mac** | `bin/tgdrive-linux-arm64` | `AArch64` | ~6.8 MB |

Make executable and place on your PATH:
```bash
chmod +x tgdrive-linux-amd64
sudo mv tgdrive-linux-amd64 /usr/local/bin/tgdrive
```

---

## 4. Configuration

Initialize a template configuration file:
```bash
tgdrive config
```

This creates `tgdrive.json` (or `~/.config/tgdrive/tgdrive.json`):
```json
{
  "bot_tokens": [
    "123456789:ABCdefGhIJKlmNoPQRsTUVwxyZ_Token1",
    "987654321:ZYXwvuTsRQPonMlKJIhgFedCBA_Token2"
  ],
  "channel_id": "-1001234567890",
  "encryption_key": "YOUR_STRONG_SECRET_PASSPHRASE",
  "source_path": "/volume1/data",
  "bandwidth_schedule": "07:00,5M 23:00,18M",
  "nice_priority": 19,
  "retention": {
    "keep_hourly": 24,
    "keep_daily": 7,
    "keep_weekly": 4,
    "keep_monthly": 12
  },
  "server_port": 8088,
  "node_name": "Synology-DS920+",
  "node_mode": "PUSH"
}
```

---

## 5. CLI Command Reference

### Backup / Synchronization
Run a point-in-time CoW snapshot backup:
```bash
tgdrive backup
```

### List Snapshots
Display all historical point-in-time snapshots:
```bash
tgdrive snapshots
```
Output:
```text
SNAPSHOT ID              CREATED AT           FILES    TOTAL SIZE   DELTA        NODE      
------------------------------------------------------------------------------------------
snap-20261110-060000     2026-11-10 06:00:00  5        142.0 MB     6.0 MB       Synology-DS920+
snap-20261110-020000     2026-11-10 02:00:00  4        136.0 MB     136.0 MB     Synology-DS920+
```

### Explore Files within a Snapshot
Browse virtual directories at a specific moment in time:
```bash
tgdrive ls snap-20261110-060000 documents
```

### Snapshot Diff Inspection
Compare changes between two snapshots:
```bash
tgdrive diff snap-20261110-020000 snap-20261110-060000
```
Output:
```text
Comparing snap-20261110-020000 -> snap-20261110-060000 (2 changes):
ACTION     SIZE         PATH
----------------------------------------------------------------------
MODIFIED   600.0 KB     documents/financial_report.xlsx
ADDED      5.4 MB       documents/meeting_minutes_20261110.docx
```

### Point-in-Time Restore
Restore an individual file:
```bash
tgdrive restore snap-20261110-060000 documents/financial_report.xlsx /volume1/restored
```
Restore an entire folder:
```bash
tgdrive restore snap-20261110-060000 documents /volume1/restored_docs
```

### Retention Pruning
Enforce retention rules and prune expired snapshots:
```bash
tgdrive prune
```

### Embedded Web Dashboard
Launch the zero-dependency embedded web server:
```bash
tgdrive serve --port 8088
```
Open `http://<NAS_IP>:8088` to access the full visual console.

---

## 6. Multi-NAS Topology Deployment

### Rayma Network Topology Example:
* **Synology (`192.168.1.23`)**: Primary storage node, running `tgdrive backup` via Synology DSM Task Scheduler.
* **Buffalo NAS 199 (`192.168.1.199`)**: Mounted via SMB CIFS (`\\192.168.1.199\Source-0`) into `/volume1/remote_buffalo/nas199` on Synology.
* **Buffalo NAS 198 (`192.168.1.198`)**: Mounted via SMB CIFS (`\\192.168.1.198\google-drive-inforayma`) into `/volume1/remote_buffalo/nas198`.
* **Buffalo NAS 196 (`192.168.1.196`)**: Mounted via SMB CIFS (`\\192.168.1.196\HDD-01`) into `/volume1/remote_buffalo/nas196`.

By mounting Buffalo network shares into Synology, `tgdrive` backs up all NAS units simultaneously from a single central host without running any background processes on the Buffalo devices.

---

## 7. Compiling from Source

Prerequisites: Go 1.22+
```bash
# Clone the repository
git clone https://github.com/thanhvo-agy/Telegram-Drive.git
cd Telegram-Drive

# Build static binary for current machine
go build -ldflags="-s -w" -o bin/tgdrive ./cmd/tgdrive

# Cross-compile for all architectures
bash scripts/build.sh
```

---

## 8. License

Licensed under the MIT License. See [LICENSE](LICENSE) for details.
