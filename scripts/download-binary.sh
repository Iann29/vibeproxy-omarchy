#!/bin/bash
set -e

# Download the latest CLIProxyAPIPlus binary for Linux
# Usage: ./download-binary.sh [target-dir]

GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m'

TARGET_DIR="${1:-$HOME/.local/share/vibeproxy}"
ARCH=$(uname -m)

case "$ARCH" in
    x86_64)  GO_ARCH="amd64" ;;
    aarch64) GO_ARCH="arm64" ;;
    *)
        echo -e "${RED}❌ Unsupported architecture: $ARCH${NC}"
        exit 1
        ;;
esac

echo -e "${BLUE}📦 Fetching latest CLIProxyAPIPlus release...${NC}"

LATEST_JSON=$(curl -s -f https://api.github.com/repos/router-for-me/CLIProxyAPIPlus/releases/latest)
if [ -z "$LATEST_JSON" ]; then
    echo -e "${RED}❌ Failed to fetch release info from GitHub${NC}"
    exit 1
fi

LATEST_TAG=$(echo "$LATEST_JSON" | grep -o '"tag_name": "[^"]*"' | head -1 | cut -d'"' -f4)
VERSION="${LATEST_TAG#v}"

if [ -z "$VERSION" ]; then
    echo -e "${RED}❌ Failed to parse version from release${NC}"
    exit 1
fi

FILENAME="CLIProxyAPIPlus_${VERSION}_linux_${GO_ARCH}.tar.gz"
URL="https://github.com/router-for-me/CLIProxyAPIPlus/releases/download/${LATEST_TAG}/${FILENAME}"

echo -e "${BLUE}📥 Downloading CLIProxyAPIPlus v${VERSION} for linux/${GO_ARCH}...${NC}"
echo "   URL: $URL"

TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

if ! curl -sL -f -o "$TEMP_DIR/$FILENAME" "$URL"; then
    echo -e "${RED}❌ Failed to download $URL${NC}"
    exit 1
fi

echo -e "${BLUE}📂 Extracting...${NC}"
tar -xzf "$TEMP_DIR/$FILENAME" -C "$TEMP_DIR"

BINARY=$(find "$TEMP_DIR" -type f \( -name "CLIProxyAPIPlus" -o -name "cli-proxy-api-plus" \) | head -1)
if [ -z "$BINARY" ]; then
    BINARY=$(find "$TEMP_DIR" -type f ! -name "*.tar.gz" ! -name "*.md" ! -name "*.txt" ! -name "*.yaml" ! -name "LICENSE" | head -1)
fi

if [ -z "$BINARY" ]; then
    echo -e "${RED}❌ Could not find binary in archive${NC}"
    exit 1
fi

mkdir -p "$TARGET_DIR"
cp "$BINARY" "$TARGET_DIR/cli-proxy-api-plus"
chmod +x "$TARGET_DIR/cli-proxy-api-plus"

FILE_SIZE=$(stat -c%s "$TARGET_DIR/cli-proxy-api-plus" 2>/dev/null || stat -f%z "$TARGET_DIR/cli-proxy-api-plus" 2>/dev/null)
if [ "$FILE_SIZE" -lt 1048576 ]; then
    echo -e "${RED}❌ Binary seems too small (${FILE_SIZE} bytes)${NC}"
    exit 1
fi

echo -e "${GREEN}✅ CLIProxyAPIPlus v${VERSION} installed to ${TARGET_DIR}/cli-proxy-api-plus (${FILE_SIZE} bytes)${NC}"
echo -e "${GREEN}   Architecture: linux/${GO_ARCH}${NC}"
