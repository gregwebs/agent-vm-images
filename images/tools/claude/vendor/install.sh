#!/bin/bash

set -e

# Parse command line arguments
TARGET="$1"  # Exact version, always supplied by install-claude.sh

# This build path is exact. Reject an empty value, `stable`/`latest`, a range, a
# URL, whitespace and anything that is not the numeric version the pinned
# installer supports. install-claude.sh validates first; this is defence in
# depth at the point of use.
if [[ ! "$TARGET" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
    echo "Usage: $0 VERSION (exact, e.g. 2.1.286)" >&2
    exit 1
fi

# The classified download helper lives in the bind-mounted recipe contract.
: "${AGENT_VM_CONTRACT_DIR:?the claude installer needs AGENT_VM_CONTRACT_DIR}"

# The committed pin is the selection authority (images/tools/claude/claude_pin.py,
# ADR 0013). No default: a direct run without it fails before any download.
: "${CLAUDE_PIN_DIR:?the claude installer needs CLAUDE_PIN_DIR (the committed pin directory)}"

# Refuse to run under sudo from a regular user's shell. This installer puts
# everything under $HOME, which under sudo typically resolves to root's home:
# the binary lands in /root/.local/bin (or is left root-owned in the user's
# home, depending on the distro's sudo configuration), and the 'claude'
# command is then not found in the user's own shell. Plain root with no sudo
# (containers, CI, root-only systems) is unaffected by this check.
if [ "$(id -u)" -eq 0 ] && [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ] && [ -z "${CLAUDE_INSTALL_ALLOW_SUDO:-}" ]; then
    echo "Error: do not run this installer with sudo." >&2
    echo "" >&2
    echo "Claude Code installs into your home directory and does not need root access." >&2
    echo "With sudo, the installation would go into root's home directory instead of" >&2
    echo "yours, and the 'claude' command would not work from your own shell." >&2
    echo "" >&2
    echo "Please re-run the same command without sudo, e.g.:" >&2
    # pinned-dep-allow: display-only guidance text in an error message, not an executed install; install.sh is Anthropic's own installer
    echo "    curl -fsSL https://claude.ai/install.sh | bash" >&2
    echo "" >&2
    echo "To intentionally install Claude Code for the root user, re-run with" >&2
    echo "CLAUDE_INSTALL_ALLOW_SUDO=1 set in the installer's environment, e.g.:" >&2
    # pinned-dep-allow: display-only guidance text in an error message, not an executed install; install.sh is Anthropic's own installer
    echo "    curl -fsSL https://claude.ai/install.sh | sudo CLAUDE_INSTALL_ALLOW_SUDO=1 bash" >&2
    exit 1
fi

DOWNLOAD_BASE_URL="https://downloads.claude.ai/claude-code-releases"
DOWNLOAD_DIR="$HOME/.claude/downloads"

# The shared download helper uses curl only. Require it up front so a missing
# curl is a clear error rather than a mid-download failure.
if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required but is not installed" >&2
    exit 1
fi

# Classified download through the shared recipe contract. A transport failure
# writes the receipt and returns 75, which `set -e` propagates out of an
# assignment and an explicit `exit` propagates out of a plain call; every other
# failure is hard. wget and progress variants are not used here, so there is
# exactly one download policy.
download_file() {
    local url="$1"
    local output="$2"

    sh "${AGENT_VM_CONTRACT_DIR}/download.sh" "$url" "$output"
}

# Detect platform
case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    MINGW*|MSYS*|CYGWIN*) echo "Windows is not supported by this script. See https://code.claude.com/docs for installation options." >&2; exit 1 ;;
    *) echo "Unsupported operating system: $(uname -s). See https://code.claude.com/docs for supported platforms." >&2; exit 1 ;;
esac

case "$(uname -m)" in
    x86_64|amd64) arch="x64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

# Detect Rosetta 2 on macOS: if the shell is running as x64 under Rosetta on an ARM Mac,
# download the native arm64 binary instead of the x64 one
if [ "$os" = "darwin" ] && [ "$arch" = "x64" ]; then
    if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = "1" ]; then
        arch="arm64"
    fi
fi

# Check for musl on Linux and adjust platform accordingly
if [ "$os" = "linux" ]; then
    if [ -f /lib/libc.musl-x86_64.so.1 ] || [ -f /lib/libc.musl-aarch64.so.1 ] || ldd /bin/ls 2>&1 | grep -q musl; then
        platform="linux-${arch}-musl"
    else
        platform="linux-${arch}"
    fi
else
    platform="${os}-${arch}"
fi

# The native installer's own layout: a launcher symlink into versions/. A build is
# a fresh install, so an existing payload or launcher is refused rather than
# adopted without the content check.
versions_dir="$HOME/.local/share/claude/versions"
installed="$versions_dir/$TARGET"
launcher="$HOME/.local/bin/claude"
if [ -e "$launcher" ] || [ -L "$launcher" ] || { [ -d "$versions_dir" ] && [ -n "$(ls -A "$versions_dir")" ]; }; then
    echo "Refusing to install over an existing Claude Code payload or launcher under $HOME" >&2
    exit 1
fi

version="$TARGET"
selection=$(python3 "$CLAUDE_PIN_DIR/claude_pin.py" select --pin-dir "$CLAUDE_PIN_DIR" \
    --version "$version" --platform "$platform")
read -r url checksum size <<EOF
$selection
EOF
# Recomputed here as well: the pin can name bytes, never another location.
if [ "$url" != "$DOWNLOAD_BASE_URL/$version/$platform/claude" ] ||
    [[ ! "$checksum" =~ ^[a-f0-9]{64}$ ]] || [[ ! "$size" =~ ^[1-9][0-9]*$ ]]; then
    echo "The committed pin selection is malformed: $selection" >&2
    exit 1
fi

checksum_matches() {
    local actual
    if [ "$os" = "darwin" ]; then
        actual=$(shasum -a 256 "$1" | cut -d' ' -f1)
    else
        actual=$(sha256sum "$1" | cut -d' ' -f1)
    fi
    [ "$actual" = "$2" ]
}

mkdir -p "$DOWNLOAD_DIR"
binary_path="$DOWNLOAD_DIR/claude-$version-$platform"
# `|| exit $?` propagates a classified transport 75 instead of turning it into a
# generic "Download failed" hard error. This is the only classified site: the
# native step no longer downloads anything.
download_file "$url" "$binary_path" || exit $?
if ! checksum_matches "$binary_path" "$checksum"; then
    echo "Checksum verification failed" >&2
    rm -f "$binary_path"
    exit 1
fi
# The digest proves which bytes these are, not that they are the expected
# executable: the ELF preflight runs before anything is placed.
if ! python3 "$CLAUDE_PIN_DIR/claude_pin.py" preflight --pin-dir "$CLAUDE_PIN_DIR" \
    --version "$version" --platform "$platform" "$binary_path"; then
    rm -f "$binary_path"
    exit 1
fi
# Placement is the part of `claude install` that downloads. Doing it from verified
# bytes leaves the native step only its local setup, which setup-claude.sh runs with
# no network. A failure from here on is exit 1, never 75: the moved payload and any
# launcher stay in place and the RUN fails, so no softening or cleanup applies.
mkdir -p "$versions_dir" "${launcher%/*}"
chmod 0755 "$binary_path"
mv "$binary_path" "$installed"
ln -s "$installed" "$launcher"
python3 "$CLAUDE_PIN_DIR/claude_pin.py" installed --pin-dir "$CLAUDE_PIN_DIR" --version "$version" \
    --platform "$platform" --prefix "$HOME" --owner "$(id -u):$(id -g)"
echo ""
echo "✅ Claude Code $version placed; native setup runs offline next."
