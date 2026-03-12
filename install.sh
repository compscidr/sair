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
INSTALL_SYSTEMD=false
INSTALL_SYSTEMD_ADB_ONLY=false
DIR_EXPLICITLY_SET=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)  VERSION="$2";      shift 2 ;;
        --dir)      INSTALL_DIR="$2"; DIR_EXPLICITLY_SET=true; shift 2 ;;
        --systemd)  INSTALL_SYSTEMD=true; shift ;;
        --systemd-adb-only) INSTALL_SYSTEMD_ADB_ONLY=true; shift ;;
        --help|-h)
            echo "Usage: install.sh [--version VERSION] [--dir INSTALL_DIR] [--systemd] [--systemd-adb-only]"
            echo ""
            echo "Options:"
            echo "  --version VERSION   Version to install (default: latest release)"
            echo "  --dir DIR           Install directory (default: ~/.local/bin)"
            echo "  --systemd           Install all systemd service units (requires sudo, Linux only)"
            echo "  --systemd-adb-only  Install only the ADB server systemd unit (requires sudo, Linux only)"
            exit 0
            ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# When installing systemd units, default to /usr/local/bin (matches unit ExecStart paths)
if [[ "$INSTALL_SYSTEMD" == "true" || "$INSTALL_SYSTEMD_ADB_ONLY" == "true" ]]; then
    if [[ "$DIR_EXPLICITLY_SET" == "false" ]]; then
        INSTALL_DIR="/usr/local/bin"
    fi
fi

REPO="compscidr/sair"

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# systemd is Linux-only
if [[ "$INSTALL_SYSTEMD" == "true" || "$INSTALL_SYSTEMD_ADB_ONLY" == "true" ]]; then
    if [[ "$OS" != "linux" ]]; then
        echo "Error: --systemd is only supported on Linux." >&2
        exit 1
    fi
fi

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

# Ensure INSTALL_DIR is in PATH
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        if [[ -n "${GITHUB_PATH:-}" ]]; then
            echo "$INSTALL_DIR" >> "$GITHUB_PATH"
            echo ""
            echo "Added ${INSTALL_DIR} to GITHUB_PATH."
        else
            echo ""
            echo "NOTE: ${INSTALL_DIR} is not in your PATH. Add it with:"
            echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        fi
        ;;
esac

# Install systemd units if requested
if [[ "$INSTALL_SYSTEMD" == "true" || "$INSTALL_SYSTEMD_ADB_ONLY" == "true" ]]; then
    echo ""
    echo "Installing systemd units..."

    # Determine which units to install
    if [[ "$INSTALL_SYSTEMD_ADB_ONLY" == "true" && "$INSTALL_SYSTEMD" == "false" ]]; then
        UNITS="sair-adb-server.service"
    else
        UNITS="sair-adb-server.service sair-device-source.service sair-proxy.service"
    fi

    # Use systemd files from the release archive (bundled in systemd/ directory)
    for unit in $UNITS; do
        if [[ -f "${TMP}/systemd/${unit}" ]]; then
            sudo install -m 644 "${TMP}/systemd/${unit}" "/etc/systemd/system/${unit}"
        else
            echo "Warning: ${unit} not found in release archive, skipping" >&2
        fi
        echo "  /etc/systemd/system/${unit}"
    done

    # Install env file templates (don't overwrite existing)
    sudo mkdir -p /etc/sair
    ENVS="device-source.env proxy.env"
    if [[ "$INSTALL_SYSTEMD_ADB_ONLY" == "true" && "$INSTALL_SYSTEMD" == "false" ]]; then
        ENVS="device-source.env"
    fi
    for env in $ENVS; do
        if [[ ! -f "/etc/sair/${env}" ]]; then
            if [[ -f "${TMP}/systemd/${env}" ]]; then
                sudo install -m 600 "${TMP}/systemd/${env}" "/etc/sair/${env}"
                echo "  /etc/sair/${env} (new)"
            fi
        else
            echo "  /etc/sair/${env} (exists, skipped)"
        fi
    done

    sudo systemctl daemon-reload
    echo ""
    echo "Systemd units installed. Enable with:"
    if [[ "$INSTALL_SYSTEMD_ADB_ONLY" == "true" && "$INSTALL_SYSTEMD" == "false" ]]; then
        echo "  sudo systemctl enable --now sair-adb-server"
    else
        echo "  sudo systemctl enable --now sair-adb-server sair-device-source  # device source machine"
        echo "  sudo systemctl enable --now sair-proxy                          # proxy machine"
    fi
    echo ""
    echo "Edit /etc/sair/*.env to configure."
fi
