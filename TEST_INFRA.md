# Test Infrastructure & E2E Architecture Guide

## 1. Overview
TeleClone Pro (`tgdrive`) features an opaque-box, requirement-driven end-to-end (E2E) testing framework located in `tests/e2e/`. The test suite operates without external network dependencies, third-party mock daemons, or Docker containers, ensuring 100% reproducible execution across all target platforms.

## 2. Test Execution Commands

### Run Entire E2E Test Suite
```bash
go test -v ./tests/e2e/...
```

### Run Specific Test Tiers
```bash
# Tier 1: Core Feature Isolation (>=5 tests per feature)
go test -v -run "TestTier1_" ./tests/e2e/...

# Tier 2: Boundary & Corner Cases (>=5 tests per feature)
go test -v -run "TestTier2_" ./tests/e2e/...

# Tier 3: Cross-Feature Combinations (Pairwise coverage)
go test -v -run "TestTier3_" ./tests/e2e/...

# Tier 4: Real-World Application Scenarios (>=5 realistic scenarios)
go test -v -run "TestTier4_" ./tests/e2e/...

# Zero-Emoji Policy Static Verification
go test -v -run "TestZeroEmojisPolicy" ./tests/e2e/...
```

### Run with Race Detector
```bash
go test -v -race ./tests/e2e/...
```

---

## 3. Test Architecture & Tier Structure

```
tests/e2e/
├── doc.go                     # E2E architecture documentation and guidelines
├── mock_telegram_test.go      # In-memory Telegram Bot & Cloud API mock server
├── test_context_test.go       # Sandbox lifecycle, binary build, and capability probe
├── tier1_features_test.go     # Tier 1: Happy path tests for WebDAV, Bot, WORM, Compression
├── tier2_boundaries_test.go   # Tier 2: Boundary, corner, and failure injection tests
├── tier3_pairwise_test.go     # Tier 3: Cross-feature interaction and concurrency tests
├── tier4_scenarios_test.go    # Tier 4: Real-world OS, disaster recovery, and timeline tests
└── zero_emojis_test.go        # Static analysis enforcing 100% pure ASCII / zero emojis
```

### Tier 1: Feature Coverage (Isolation)
Verifies each of the 4 core roadmap features in isolation:
1. **WebDAV Server (`tgdrive mount`)**: RFC 4918 compliance (`OPTIONS`, `PROPFIND`, `GET`, `HEAD`), header validation (`DAV: 1, 2`, `MS-Author-Via: DAV`), and `io.ReadSeeker` streaming decryption.
2. **Interactive 2-Way Bot Remote Control**: Long polling `getUpdates` engine, command processing (`/status`, `/backup_now`, `/snapshots`, `/help`), inline keyboards, and user ID whitelist enforcement.
3. **Anti-Ransomware WORM Snapshot Locking**: Manifest `WORMMetadata` generation, HMAC-SHA256 signature calculation/verification, retention pruner lock enforcement, cloud deletion prevention, and disaster recovery `reindex`.
4. **Fast Compression (Zstandard)**: Pre-encryption Zstd compression pipeline, `TGC2` chunk container layout, AES-GCM AAD integrity protection, incompressible data bypass, and transparent `TGC1` backward compatibility.

### Tier 2: Boundary & Corner Cases
Stresses edge conditions, boundary offsets, and malicious inputs:
- **WebDAV Seeking**: Offset 0, 49MB chunk boundaries, cross-chunk reads, EOF, and seeking past EOF.
- **Fast Compression**: Incompressible media (JPEG/PNG/MP4/ZIP) vs highly compressible logs (>70% ratio), empty/zero-byte chunks, corrupted Zstd frames, tampered OrigSz inside AAD.
- **Anti-Ransomware WORM**: Malformed/truncated manifests, tampered `retain_until` HMAC mismatches, NTP clock skew detection against Telegram HTTP `Date` headers, indefinite Legal Hold overrides, compliance mode parameter validation.
- **Bot Remote Control**: Poller network connection resets, HTTP 429 rate limit backoff (`RetryAfter`), unauthorized user message drops, Markdown special character escaping (`*`, `_`, `[`, `]`, `` ` ``), rapid duplicate `/backup_now` commands.

### Tier 3: Cross-Feature Combinations (Pairwise Coverage)
Evaluates interactions between features operating concurrently:
- WebDAV mounting a snapshot containing both `TGC1` (legacy) and `TGC2` (compressed) chunks with active WORM lock.
- Triggering `/backup_now` from Telegram Bot while WebDAV streaming reads are actively in progress.
- Attempting a retention prune during an active WebDAV mount session.
- Bot `/status` command reporting live WORM lock metrics and compression savings ratio.
- Disaster recovery `reindex` scanning and reconstructing mixed `TGC1`/`TGC2` manifests under WORM protection.

### Tier 4: Real-World Application Scenarios
Simulates realistic enterprise and end-user workflows:
- **Windows Explorer Client Simulation**: Drive mapping, `PROPFIND` depth 1, `LOCK` probe handling, `desktop.ini` probing, random range reads on large files.
- **macOS Finder Client Simulation**: WebDAV connection via `mount_webdav`, AppleDouble `._*` and `.DS_Store` probing, read-only volume mounting with lock badges.
- **Disaster Recovery Simulation**: Complete local disk format/wipe of `~/.config/tgdrive/snapshots/`; `tgdrive reindex` scanning remote Telegram channel, reconstructing manifest files, validating HMAC signatures, and restoring bit-for-bit file tree.
- **Multi-Version Snapshot Timeline**: 5-stage point-in-time snapshot timeline (Day 1 to Day 90) verifying that WORM retention policies protect active snapshots while allowing expired unlocked snapshots to be cleanly pruned.
- **Synology File Station Remote Connection**: Remote WebDAV connection on port 8089, nested directory navigation, and media streaming via HTTP 206 Partial Content.

---

## 4. Test Infrastructure Fixtures

### `MockTelegramServer` (`mock_telegram_test.go`)
An in-memory HTTP server (`httptest.Server`) providing high-fidelity simulation of the Telegram Bot API:
- **`getMe`**: Returns bot metadata.
- **`getUpdates`**: Simulates non-blocking and long-polling message queues; tracks offset updates.
- **`sendMessage`**: Captures sent text messages, markdown formatting, chat IDs, and inline keyboard markups.
- **`sendDocument`**: Handles multipart document uploads (encrypted chunks and snapshot manifests).
- **`getFile` & `/file/bot<token>/...`**: Provides chunk download endpoints.
- **`answerCallbackQuery`**: Records button click acknowledgments.
- **`editMessageText`**: Captures in-place message text updates.
- **Fault Injection**: Configurable HTTP 429 Too Many Requests (`RetryAfter`), network connection aborts, and artificial network latency.

### `E2EContext` (`test_context_test.go`)
Manages test isolation and sandboxing:
- Generates dedicated temporary directory sandboxes for source files, snapshot metadata, and restore targets.
- Compiles or locates the `tgdrive` static binary.
- Spawns and manages `MockTelegramServer`.
- Synthesizes compressible text corpora (repetitive text logs) and incompressible pseudo-random buffers (entropy ~8 bits/byte).
- Provides capability probes (`SupportsCommand`, `SupportsWebDAV`, etc.) enabling progressive execution across development milestones.

---

## 5. Feature Coverage Matrix (35 Features from PROJECT.md)

| # | Feature | Scope / Package | Primary Test Function | Tier |
|---|---------|-----------------|-----------------------|------|
| 1 | Pre-Encryption Zstandard Compression | `pkg/crypto` | `TestTier1_Compression_Zstd_Pipeline` | Tier 1 |
| 2 | Pure-Go Zero-Cgo Zstd Library | `pkg/crypto` | `TestTier1_Compression_Zstd_Pipeline` | Tier 1 |
| 3 | `TGC2` Chunk Container Format | `pkg/crypto` | `TestTier1_Compression_TGC2_Container_Format` | Tier 1 |
| 4 | AAD Metadata Integrity Protection | `pkg/crypto` | `TestTier1_Compression_AAD_Authentication` | Tier 1 |
| 5 | Incompressible Data Bypass | `pkg/crypto` | `TestTier1_Compression_Incompressible_Fallback` | Tier 1 |
| 6 | Transparent Backwards Compatibility | `pkg/crypto` | `TestTier1_Compression_Backward_Compatibility` | Tier 1 |
| 7 | Manifest Metadata Extension | `pkg/snapshot` | `TestTier1_Compression_Manifest_Metadata` | Tier 1 |
| 8 | Synchronous Zero-Goroutine Chunk API | `pkg/crypto` | `TestTier1_Compression_Zero_Goroutine_Leak` | Tier 1 |
| 9 | WORM Metadata in Manifest | `pkg/snapshot` | `TestTier1_WORM_Metadata_Structure` | Tier 1 |
| 10 | Manifest Cryptographic HMAC | `pkg/snapshot` | `TestTier1_WORM_HMAC_Verification` | Tier 1 |
| 11 | WORM-Enforced Pruning Engine | `pkg/snapshot` | `TestTier1_WORM_Retention_Lock_Enforcement` | Tier 1 |
| 12 | Cloud Deletion Prevention | `pkg/telegram` | `TestTier1_WORM_Cloud_Deletion_Prevention` | Tier 1 |
| 13 | Tamper Detection & Security Alerts | `pkg/snapshot` | `TestTier1_WORM_Tamper_Detection` | Tier 1 |
| 14 | Disaster Recovery Reindex (`tgdrive reindex`) | `pkg/snapshot` | `TestTier1_WORM_Disaster_Recovery_Reindex` | Tier 1 |
| 15 | `tgdrive mount` CLI Subcommand | `cmd/tgdrive` | `TestTier1_WebDAV_CLI_Mount_Subcommand` | Tier 1 |
| 16 | Port 8089 Serving & WebDAV Handler | `pkg/webdav` | `TestTier1_WebDAV_OPTIONS_RFC4918` | Tier 1 |
| 17 | Virtual FileSystem Adapter | `pkg/webdav` | `TestTier1_WebDAV_PROPFIND_SnapshotTree` | Tier 1 |
| 18 | Snapshot Tree Presentation | `pkg/webdav` | `TestTier1_WebDAV_PROPFIND_SnapshotTree` | Tier 1 |
| 19 | Streaming Decryption & Seeking | `pkg/webdav` | `TestTier1_WebDAV_ReadSeeker_RangeRequests` | Tier 1 |
| 20 | LRU Chunk Cache | `pkg/webdav` | `TestTier1_WebDAV_LRU_Cache` | Tier 1 |
| 21 | Windows Explorer Compatibility | `pkg/webdav` | `TestTier4_Windows_Explorer_Browse_And_Partial_Read` | Tier 4 |
| 22 | macOS Finder Compatibility | `pkg/webdav` | `TestTier4_MacOS_Finder_Browse_And_Metadata_Probe` | Tier 4 |
| 23 | Synology File Station Remote Connection | `pkg/webdav` | `TestTier4_Synology_FileStation_Remote_WebDAV_Mount` | Tier 4 |
| 24 | Co-Hosting with Web Dashboard | `pkg/server` | `TestTier1_WebDAV_CoHosting_With_Dashboard` | Tier 1 |
| 25 | `getUpdates` Long Polling Engine | `pkg/telegram` | `TestTier1_Bot_LongPolling_GetUpdates` | Tier 1 |
| 26 | `/status` Command Handler | `pkg/telegram` | `TestTier1_Bot_Command_Status` | Tier 1 |
| 27 | `/backup_now` Command Handler | `pkg/telegram` | `TestTier1_Bot_Command_BackupNow` | Tier 1 |
| 28 | `/snapshots` Command Handler | `pkg/telegram` | `TestTier1_Bot_Command_Snapshots` | Tier 1 |
| 29 | `/help` Command Handler | `pkg/telegram` | `TestTier1_Bot_Command_Help` | Tier 1 |
| 30 | Inline Keyboard Callback Router | `pkg/telegram` | `TestTier1_Bot_InlineKeyboard_Callback` | Tier 1 |
| 31 | Secure Access Whitelist | `pkg/telegram` | `TestTier1_Bot_UserID_Whitelist` | Tier 1 |
| 32 | Zero-Emojis Policy Filter | `pkg/telegram` | `TestTier1_Bot_Zero_Emojis_Format` | Tier 1 |
| 33 | E2E Testing Suite (Tiers 1-4) | `tests/e2e` | `TestTier1_*` - `TestTier4_*` | Tiers 1-4 |
| 34 | Adversarial Hardening (Tier 5) | `tests/e2e` | `TestTier2_*` Boundary & Injection Suite | Tier 2 |
| 35 | Static Multi-Arch Zero-Cgo Binaries | `scripts` | `TestStaticBinaryCompatibility` | Infra |

---

## 6. Strict Constraints Enforced
1. **Zero External Dependencies**: All tests run using standard Go test toolchain (`go test`).
2. **Zero Emojis**: Automated AST and string scanner test (`TestZeroEmojisPolicy`) scans all repository source files and assets to ensure zero Unicode emoji characters.
3. **Single Static Binary Compatibility**: Tests execute against pure Go compilation (`CGO_ENABLED=0`).
