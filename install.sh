#!/usr/bin/env bash
# Install SAIR tools.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/compscidr/sair/main/install.sh | bash -s -- --version v0.2.0 --dir /usr/local/bin
#
set -euo pipefail

VERSION=""
INSTALL_DIR="${HOME}/.local/bin"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)  VERSION="$2";      shift 2 ;;
        --dir)      INSTALL_DIR="$2";   shift 2 ;;
        --help|-h)
            echo "Usage: install.sh [--version VERSION] [--dir INSTALL_DIR]"
            echo ""
            echo "Options:"
            echo "  --version VERSION   Version to install (default: latest release)"
            echo "  --dir DIR           Install directory (default: ~/.local/bin)"
            exit 0
            ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

REPO="compscidr/sair"

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)   ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    *)               echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# Resolve version
if [[ -z "$VERSION" ]]; then
    echo "Fetching latest release..."
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
    if [[ -z "$VERSION" ]]; then
        echo "Failed to determine latest version." >&2
        exit 1
    fi
fi

ARCHIVE="sair-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

echo "Installing SAIR ${VERSION} (${OS}/${ARCH})..."
echo "  From: ${URL}"
echo "  To:   ${INSTALL_DIR}"

# Download and extract to a temp directory
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "${TMP}/${ARCHIVE}"
tar xzf "${TMP}/${ARCHIVE}" -C "$TMP"

# Install
mkdir -p "$INSTALL_DIR"
for bin in sair-device-source sair-proxy sair-acquire sair-release; do
    if [[ -f "${TMP}/${bin}" ]]; then
        install -m 755 "${TMP}/${bin}" "${INSTALL_DIR}/${bin}"
    fi
done

echo ""
echo "Installed:"
for bin in sair-device-source sair-proxy sair-acquire sair-release; do
    if [[ -f "${INSTALL_DIR}/${bin}" ]]; then
        echo "  ${INSTALL_DIR}/${bin}"
    fi
done

# Check PATH
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        echo ""
        echo "NOTE: ${INSTALL_DIR} is not in your PATH. Add it with:"
        echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        ;;
esac
