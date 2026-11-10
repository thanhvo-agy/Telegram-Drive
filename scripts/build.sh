#!/usr/bin/env bash
# ==============================================================================
# TeleClone (tgdrive) Multi-Architecture Static Cross-Compiler
# Builds static, stripped, zero-dependency binaries for all platforms
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

mkdir -p bin

LDFLAGS="-s -w"

echo "Building TeleClone Pro (tgdrive) binaries..."

echo "-> [1/5] linux/amd64 (Synology x86_64, Dell-1, Linux Servers)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" -o bin/tgdrive-linux-amd64 ./cmd/tgdrive

echo "-> [2/5] linux/armv7 (Buffalo LS410D, LS220D, Raspberry Pi 3/4)..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$LDFLAGS" -o bin/tgdrive-linux-armv7 ./cmd/tgdrive

echo "-> [3/5] linux/armv5 (Buffalo LinkStation Quad, Legacy ARM)..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=5 go build -ldflags="$LDFLAGS" -o bin/tgdrive-linux-armv5 ./cmd/tgdrive

echo "-> [4/5] linux/arm64 (64-bit ARM NAS & Servers)..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$LDFLAGS" -o bin/tgdrive-linux-arm64 ./cmd/tgdrive

echo "-> [5/5] darwin/arm64 (Apple Silicon Mac M1/M2/M3/M4)..."
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="$LDFLAGS" -o bin/tgdrive-darwin-arm64 ./cmd/tgdrive

# Default symlink
cp -f bin/tgdrive-linux-amd64 bin/tgdrive

echo ""
echo "Build complete! Output binaries in bin/:"
ls -lh bin/
