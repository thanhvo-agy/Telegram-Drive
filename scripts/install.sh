#!/usr/bin/env sh
# ==============================================================================
# TeleClone (tgdrive) - Universal 1-Click Installer
# Supports: Synology DSM, Buffalo NAS (Entware/Optware), OpenWrt, Linux, macOS
# Zero Docker. Zero Dependencies. Pure Single Static Binary.
# ==============================================================================
set -e

REPO="thanhvo-agy/Telegram-Drive"
VERSION="latest"

echo "================================================================="
echo "   TeleClone (tgdrive) - Telegram Backup Engine Installer"
echo "================================================================="

# 1. Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    linux*)  OS="linux" ;;
    darwin*) OS="darwin" ;;
    *) echo "Unsupported operating system: $OS"; exit 1 ;;
esac

# 2. Detect Architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)
        BINARY_NAME="tgdrive-${OS}-amd64"
        ;;
    armv7*|armhf)
        BINARY_NAME="tgdrive-${OS}-armv7"
        ;;
    armv5*|armv6*|arm)
        BINARY_NAME="tgdrive-${OS}-armv5"
        ;;
    aarch64|arm64)
        BINARY_NAME="tgdrive-${OS}-arm64"
        ;;
    *)
        echo "Unsupported CPU architecture: $ARCH"
        exit 1
        ;;
esac

echo "Detected Platform: $OS ($ARCH)"
echo "Target Binary:     $BINARY_NAME"

# 3. Determine Install Directory
INSTALL_DIR="/usr/local/bin"
if [ -d "/opt/bin" ]; then
    # Entware environment (Buffalo NAS, OpenWrt, Asuswrt)
    INSTALL_DIR="/opt/bin"
elif [ ! -w "/usr/local/bin" ] && [ -d "$HOME/bin" ]; then
    INSTALL_DIR="$HOME/bin"
elif [ ! -w "/usr/local/bin" ] && [ -d "$HOME/.local/bin" ]; then
    INSTALL_DIR="$HOME/.local/bin"
fi

mkdir -p "$INSTALL_DIR"
TARGET_PATH="$INSTALL_DIR/tgdrive"

echo "Install Target:    $TARGET_PATH"

DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${BINARY_NAME}"

# Fallback to local build if inside repo
SCRIPT_DIR="$(dirname "$0")"
LOCAL_BIN="$SCRIPT_DIR/../bin/$BINARY_NAME"
if [ -f "$LOCAL_BIN" ]; then
    echo "Found local compiled binary at $LOCAL_BIN, copying..."
    cp -f "$LOCAL_BIN" "$TARGET_PATH"
else
    echo "Downloading from GitHub Releases: $DOWNLOAD_URL ..."
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$DOWNLOAD_URL" -o "$TARGET_PATH"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$TARGET_PATH" "$DOWNLOAD_URL"
    else
        echo "Error: Neither curl nor wget found."
        exit 1
    fi
fi

chmod +x "$TARGET_PATH"

echo "================================================================="
echo "   Installation Completed Successfully!"
echo "================================================================="
"$TARGET_PATH" version
echo ""
echo "Quickstart:"
echo "  1. Initialize configuration: tgdrive config"
echo "  2. Run backup:               tgdrive backup"
echo "  3. Launch Web Dashboard:     tgdrive serve --port 8088"
echo "================================================================="
