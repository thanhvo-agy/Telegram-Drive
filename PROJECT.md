# Project: Telegram-Drive (TeleClone Pro / `tgdrive`)

## Architecture
TeleClone Pro (`tgdrive`) is a high-performance, single static Go binary with 0 MB idle RAM overhead, zero CGO requirements, and zero Docker dependencies. It turns Telegram channels into an encrypted cloud backup and virtual filesystem.

### Module Boundaries & Data Flow
1. **CLI Layer (`cmd/tgdrive/main.go`)**: Entry point handling subcommands: `backup`/`sync`, `restore`, `snapshots`, `ls`, `diff`, `prune`, `mount` (WebDAV), `test-bot`, `serve`, `config`, `reindex`.
2. **Crypto & Compression Pipeline (`pkg/crypto/`)**:
   - Compresses raw chunks via Zstandard (`github.com/klauspost/compress/zstd`) before encryption.
   - Encrypts via AES-256-GCM with PBKDF2-SHA256 key derivation.
   - Transparently decrypts both `TGC1` (legacy uncompressed) and `TGC2` (modern compressed/authenticated container).
3. **Snapshot & Retention Engine (`pkg/snapshot/`)**:
   - Computes Time Machine point-in-time snapshots with CoW chunk deduplication.
   - Generates and encrypts JSON manifests.
   - Enforces Enterprise WORM locking (Compliance, Governance, Legal Hold) with cryptographic HMAC-SHA256 tamper-evident verification in `retention.go` and `engine.go`.
4. **Telegram Network Mesh (`pkg/telegram/`)**:
   - Multi-bot token pool with round-robin upload multiplexing, HTTP 429 backoff handling, and QoS bandwidth throttling.
   - Long-polling `getUpdates` worker on primary token (`BotTokens[0]`) for 2-way bot remote control.
   - Interactive command processing (`/status`, `/backup_now`, `/snapshots`, `/help`) and inline keyboard callbacks (`[Backup Now]`, `[Refresh]`, `[View Snapshot]`).
5. **WebDAV Server (`pkg/webdav/` & `cmd/tgdrive/mount.go`)**:
   - RFC 4918 WebDAV HTTP server on port 8089 (`golang.org/x/net/webdav`).
   - Virtual `webdav.FileSystem` and `webdav.File` with `io.ReadSeeker`.
   - On-the-fly streaming decryption and chunk caching without downloading multi-GB files.
   - OS mount compatibility for Windows Explorer, macOS Finder, and Synology File Station.
6. **Daemon & Web UI (`pkg/server/` & `web/`)**:
   - Micro HTTP server on port 8088 serving REST endpoints, SSE events, and embedded DSM-style Web UI.
   - Strict 100% Vector SVG icons and zero emojis policy.

## Code Layout
- `cmd/tgdrive/` : CLI entrypoints and command routers (`main.go`, `mount.go`, `bot.go`).
- `pkg/config/` : Configuration models (`config.go`), environment loader, and defaults.
- `pkg/crypto/` : AES-256-GCM encryption, Zstandard compression pipeline, PBKDF2, `TGC1`/`TGC2` envelopes (`crypto.go`, `compress.go`).
- `pkg/snapshot/` : Snapshots, manifests, directory tree, WORM locking, and retention pruner (`manifest.go`, `engine.go`, `retention.go`, `worm.go`).
- `pkg/telegram/` : Multi-bot client, QoS throttling, long polling, command handlers, and inline keyboard router (`client.go`, `controller.go`, `types.go`).
- `pkg/webdav/` : RFC 4918 WebDAV server, virtual filesystem adapter, on-the-fly chunk reader/seeker (`server.go`, `fs.go`, `file.go`, `cache.go`).
- `pkg/restore/` : Snapshot restorer, chunk downloader, whole-file verification (`restore.go`).
- `pkg/server/` : REST API and SSE event hub (`server.go`).
- `web/` : Embedded SPA UI with pure SVG icons and zero emojis (`index.html`, `app.js`, `embed.go`).
- `tests/e2e/` : Opaque-box requirement-driven E2E test suite (Tiers 1-4).
- `scripts/build.sh` : Multi-architecture static compilation script (`CGO_ENABLED=0`).

## Feature Inventory
| # | Feature | Description | Milestone | Source |
|---|---------|-------------|-----------|--------|
| 1 | Pre-Encryption Zstandard Compression | Compresses raw chunk data before AES-256-GCM encryption for 30-50% savings | M1 | survey_spec_miner_2 |
| 2 | Pure-Go Zero-Cgo Zstd Library | Integrates `github.com/klauspost/compress/zstd` with zero CGO across all 5 target architectures | M1 | survey_spec_miner_2 |
| 3 | `TGC2` Chunk Container Format | Envelope `[4B Magic][1B Flag][4B OrigSz][16B Salt][12B Nonce][Ciphertext+Tag]` | M1 | survey_spec_miner_2 |
| 4 | AAD Metadata Integrity Protection | Authenticates Magic, Flag, and OrigSz inside AES-GCM AEAD slice | M1 | survey_spec_miner_2 |
| 5 | Incompressible Data Bypass | Detects pre-compressed data; sets Flag 0x00 to avoid chunk inflation | M1 | survey_spec_miner_2 |
| 6 | Transparent Backwards Compatibility | `crypto.DecryptChunk` handles both `TGC1` and `TGC2` containers seamlessly | M1 | survey_spec_miner_2 |
| 7 | Manifest Metadata Extension | Adds optional `compression` and `compressed_size` fields to `ChunkRef` | M1 | survey_spec_miner_2 |
| 8 | Synchronous Zero-Goroutine Chunk API | Uses `EncodeAll`/`DecodeAll` to ensure 0 MB idle RAM overhead | M1 | survey_spec_miner_2 |
| 9 | WORM Metadata in Manifest | Extends `SnapshotManifest` with retention mode, lock timestamp, retain-until date, legal hold | M2 | survey_spec_miner_3 |
| 10 | Manifest Cryptographic HMAC | Generates HMAC-SHA256 tamper-evident signature over canonical manifest fields | M2 | survey_spec_miner_3 |
| 11 | WORM-Enforced Pruning Engine | Forces locked snapshots into Keep; prevents deletion in `CalculatePrunePlan` and `PruneSnapshots` | M2 | survey_spec_miner_3 |
| 12 | Cloud Deletion Prevention | Asserts Telegram append-only cloud storage by preventing chunk/manifest deletion API calls | M2 | survey_spec_miner_3 |
| 13 | Tamper Detection & Security Alerts | Automatically validates HMAC on load; enters quarantine and emits alerts if altered | M2 | survey_spec_miner_3 |
| 14 | Disaster Recovery Reindex (`tgdrive reindex`) | Scans Telegram channel for manifests, decrypts, validates HMAC and reconstructs local index | M2 | survey_spec_miner_3 |
| 15 | `tgdrive mount` CLI Subcommand | Subcommand to launch embedded WebDAV server foreground daemon | M3 | survey_spec_miner_2 |
| 16 | Port 8089 Serving & WebDAV Handler | Embedded HTTP server configured with RFC 4918 handler on port 8089 | M3 | survey_spec_miner_2 |
| 17 | Virtual FileSystem Adapter | Implements `webdav.FileSystem` routing paths to snapshot manifest files and directories | M3 | survey_spec_miner_2 |
| 18 | Snapshot Tree Presentation | Virtual root structure supporting single-snapshot mount and repo browsing (`/latest`, `/snapshots/<id>`) | M3 | survey_spec_miner_2 |
| 19 | On-The-Fly Streaming Decryption & Seeking | Virtual `webdav.File` with `io.ReadSeeker` downloading only required chunks on demand | M3 | survey_spec_miner_2 |
| 20 | LRU Chunk Cache | In-memory/temp cache storing decrypted 49MB chunks with idle eviction for 0 MB idle RAM | M3 | survey_spec_miner_2 |
| 21 | Windows Explorer Compatibility | Headers and behavior tailored for Windows WebDAV Mini-Redirector (`DAV: 1, 2`, `MS-Author-Via: DAV`) | M3 | survey_spec_miner_2 |
| 22 | macOS Finder Compatibility | Graceful handling of macOS Finder metadata requests (`._*` AppleDouble, `.DS_Store`) | M3 | survey_spec_miner_2 |
| 23 | Synology File Station Remote Connection | Compatibility with Synology DSM WebDAV client for mounting Telegram Drive inside DSM | M3 | survey_spec_miner_2 |
| 24 | Co-Hosting with Web Dashboard | Option in `tgdrive serve` to run both Web Dashboard (8088) and WebDAV (8089) simultaneously | M3 | survey_spec_miner_2 |
| 25 | `getUpdates` Long Polling Engine | Non-blocking long polling listener with offset tracking and HTTP 429 backoff | M4 | survey_spec_miner_3 |
| 26 | `/status` Command Handler | Reports live node health, state, latest snapshot, storage, QoS rate, and bot mesh status | M4 | survey_spec_miner_3 |
| 27 | `/backup_now` Command Handler | Initiates on-demand Time Machine snapshot in background and posts progress/completion alerts | M4 | survey_spec_miner_3 |
| 28 | `/snapshots` Command Handler | Displays paginated list of historical snapshots with interactive view/restore buttons | M4 | survey_spec_miner_3 |
| 29 | `/help` Command Handler | Interactive documentation for remote control commands and inline keyboard buttons | M4 | survey_spec_miner_3 |
| 30 | Inline Keyboard Callback Router | Processes button clicks (`[Backup Now]`, `[Refresh]`, `[View Snapshot]`), updates messages in-place | M4 | survey_spec_miner_3 |
| 31 | Secure Access Whitelist | Enforces authorization based on user IDs and chat IDs, preventing unauthorized execution | M4 | survey_spec_miner_3 |
| 32 | Zero-Emojis Policy Filter | Enforces 100% clean ASCII indicators (`[OK]`, `[IDLE]`, `[LOCKED]`) in all bot text responses | M4 | survey_spec_miner_3 |
| 33 | E2E Testing Suite (Tiers 1-4) | Comprehensive opaque-box test suite verifying all features against specifications | E2E | survey_explorer_1 |
| 34 | Adversarial Hardening (Tier 5) | White-box stress testing, chaos/tamper scenarios, and edge-case validation | M5 | survey_explorer_1 |
| 35 | Static Multi-Arch Zero-Cgo Binaries | Clean builds across 5 CPU architectures in `bin/` and git commit/push | M5 | survey_explorer_1 |

## Milestones
| # | Name | Scope | Dependencies | Status |
|---|------|-------|-------------|--------|
| M1 | Fast Compression (Zstandard) & Crypto V2 | Features 1-8: `pkg/crypto` Zstd compression pipeline, `TGC2` container format, AAD verification, incompressible bypass, backward compatibility, chunking updates | none | DONE |
| M2 | Anti-Ransomware WORM Snapshot Locking | Features 9-14: `pkg/snapshot` WORM metadata, HMAC manifest signing, retention pruner lock enforcement, cloud deletion prevention, disaster recovery `reindex` | none | DONE |
| M3 | Embedded WebDAV Server (`tgdrive mount`) | Features 15-24: `pkg/webdav` RFC 4918 handler on port 8089, virtual filesystem, streaming on-the-fly decryption, seeking, LRU cache, OS compatibility | M1 | DONE |
| M4 | Interactive 2-Way Telegram Bot Remote Control | Features 25-32: `pkg/telegram` long polling `getUpdates`, `/status`, `/backup_now`, `/snapshots`, inline keyboards, user ID whitelist, zero emojis | M2 | DONE |
| E2E | E2E Testing Track | Feature 33: Independent opaque-box test suite (Tiers 1-4) covering all 35 features | none (parallel) | DONE (TEST_READY.md) |
| M5 | Final Milestone: 100% E2E Pass & Hardening | Features 34-35: 100% E2E test execution, Tier 5 adversarial hardening, static cross-platform binary build (`bin/`), git push | M1, M2, M3, M4, E2E | PLANNED |

## Interface Contracts

### M1 Crypto V2 (`pkg/crypto/`)
```go
var MagicHeaderV2 = []byte("TGC2")

type CompressionType uint8
const (
    CompressionNone CompressionType = 0x00
    CompressionZstd CompressionType = 0x01
)

func EncryptChunkWithCompression(plaintext []byte, passphrase string, compType CompressionType) ([]byte, error)
func DecryptChunk(data []byte, passphrase string) ([]byte, error) // Transparently handles TGC1 and TGC2
func CompressData(data []byte) ([]byte, error)
func DecompressData(data []byte, origSz uint32) ([]byte, error)
```

### M2 WORM Snapshots (`pkg/snapshot/`)
```go
type WORMMode string
const (
    WORMModeCompliance WORMMode = "compliance"
    WORMModeGovernance WORMMode = "governance"
)

type WORMMetadata struct {
    Enabled       bool      `json:"enabled"`
    Mode          WORMMode  `json:"mode"`
    LockedAt      time.Time `json:"locked_at"`
    RetainUntil   time.Time `json:"retain_until"`
    RetentionDays int       `json:"retention_days"`
    LegalHold     bool      `json:"legal_hold"`
    ManifestHMAC  string    `json:"manifest_hmac"`
}

func (m *SnapshotManifest) ComputeHMAC(key string) string
func (m *SnapshotManifest) VerifyHMAC(key string) bool
func (m *SnapshotManifest) IsWORMLocked(now time.Time) bool
func (e *Engine) ReindexFromTelegram(ctx context.Context) (*ReindexSummary, error)
```

### M3 WebDAV Server (`pkg/webdav/`)
```go
type ServerConfig struct {
    Addr      string
    Port      int
    ReadOnly  bool
    CacheSize int64
}

type Server struct { ... }
func NewServer(cfg ServerConfig, engine *snapshot.Engine, client *telegram.Client, encryptionKey string) (*Server, error)
func (s *Server) Start(ctx context.Context) error
func (s *Server) Stop() error
```

### M4 Bot Remote Controller (`pkg/telegram/`)
```go
type BotController struct { ... }
func NewBotController(cfg *config.Config, client *Client, engine *snapshot.Engine, srv *server.Server) *BotController
func (bc *BotController) Start(ctx context.Context) error
func (bc *BotController) Stop()
```
