#!/usr/bin/env bash
# scripts/install.sh — install caravan to ~/.local/bin
#
# PRE-RELEASE SCAFFOLDING: No hosted release URL exists yet. By default this
# script copies a locally-built binary from dist/. When a GitHub release is
# published, set CARAVAN_BASE_URL (or pass --url) to the release download root
# and the script will fetch the right artifact with curl.
#
# Usage:
#   bash scripts/install.sh                      # use local dist/
#   bash scripts/install.sh --url URL            # fetch from URL
#   CARAVAN_BASE_URL=URL bash scripts/install.sh # same via env var
#
# curl-pipe usage (once a URL is live):
#   curl -fsSL URL/install.sh | bash -s -- --url URL

set -euo pipefail

# ---------------------------------------------------------------------------
# Argument / env parsing
# ---------------------------------------------------------------------------
BASE_URL="${CARAVAN_BASE_URL:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --url)
      BASE_URL="$2"
      shift 2
      ;;
    --url=*)
      BASE_URL="${1#--url=}"
      shift
      ;;
    -h|--help)
      sed -n '2,20p' "$0"   # print the comment header
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Resolve repo root (script may be curl-piped, so be defensive)
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd)" || SCRIPT_DIR=""
REPO_ROOT=""
if [[ -n "$SCRIPT_DIR" ]] && [[ -f "${SCRIPT_DIR}/../internal/buildinfo/buildinfo.go" ]]; then
  REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
fi

# ---------------------------------------------------------------------------
# Version
# ---------------------------------------------------------------------------
if [[ -n "$REPO_ROOT" ]] && [[ -f "${REPO_ROOT}/internal/buildinfo/buildinfo.go" ]]; then
  VERSION="$(grep -E 'const Version\s*=' "${REPO_ROOT}/internal/buildinfo/buildinfo.go" \
    | sed 's/.*Version[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/')"
else
  # When curl-piped we may not have the source; require caller to set VERSION
  VERSION="${CARAVAN_VERSION:-}"
fi

if [[ -z "$VERSION" ]]; then
  echo "ERROR: could not determine caravan version." >&2
  echo "Set CARAVAN_VERSION=x.y.z when running curl-piped." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Detect platform
# ---------------------------------------------------------------------------
OS_RAW="$(uname -s)"
ARCH_RAW="$(uname -m)"

case "$OS_RAW" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *)
    echo "ERROR: unsupported OS '${OS_RAW}'. Build from source: go build -o caravan ." >&2
    exit 1
    ;;
esac

case "$ARCH_RAW" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64|amd64)  ARCH="amd64" ;;
  *)
    echo "ERROR: unsupported architecture '${ARCH_RAW}'. Build from source: go build -o caravan ." >&2
    exit 1
    ;;
esac

ARTIFACT_NAME="caravan-${VERSION}-${OS}-${ARCH}"
TARBALL="${ARTIFACT_NAME}.tar.gz"

echo "caravan installer — v${VERSION} / ${OS}/${ARCH}"
echo ""

# ---------------------------------------------------------------------------
# Install dir
# ---------------------------------------------------------------------------
INSTALL_DIR="${HOME}/.local/bin"
mkdir -p "$INSTALL_DIR"

INSTALL_PATH="${INSTALL_DIR}/caravan"

# ---------------------------------------------------------------------------
# Fetch or copy
# ---------------------------------------------------------------------------
TMPDIR_INST="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_INST"' EXIT

if [[ -n "$BASE_URL" ]]; then
  # Remote fetch via curl
  TARBALL_URL="${BASE_URL%/}/${TARBALL}"
  echo "Downloading ${TARBALL_URL} ..."
  curl -fsSL --progress-bar -o "${TMPDIR_INST}/${TARBALL}" "${TARBALL_URL}"
else
  # Local dist/ fallback (pre-release / development)
  if [[ -z "$REPO_ROOT" ]]; then
    echo "ERROR: no CARAVAN_BASE_URL set and could not find local dist/." >&2
    echo "Run 'make release' first, or pass --url to point at a hosted release." >&2
    exit 1
  fi
  LOCAL_TARBALL="${REPO_ROOT}/dist/${TARBALL}"
  if [[ ! -f "$LOCAL_TARBALL" ]]; then
    echo "ERROR: ${LOCAL_TARBALL} not found. Run 'make release' first." >&2
    exit 1
  fi
  echo "Installing from local dist/ (pre-release mode) ..."
  cp "$LOCAL_TARBALL" "${TMPDIR_INST}/${TARBALL}"
fi

# ---------------------------------------------------------------------------
# Extract and install
# ---------------------------------------------------------------------------
tar -xzf "${TMPDIR_INST}/${TARBALL}" -C "${TMPDIR_INST}"
EXTRACTED="${TMPDIR_INST}/caravan"

if [[ ! -f "$EXTRACTED" ]]; then
  echo "ERROR: tarball did not contain a 'caravan' binary." >&2
  exit 1
fi

chmod +x "$EXTRACTED"
mv "$EXTRACTED" "$INSTALL_PATH"

echo "Installed: ${INSTALL_PATH}"
echo ""

# ---------------------------------------------------------------------------
# PATH hint
# ---------------------------------------------------------------------------
if ! command -v caravan >/dev/null 2>&1; then
  echo "NOTE: ${INSTALL_DIR} is not on your PATH."
  echo "Add it to your shell profile:"
  echo ""
  echo '  export PATH="${HOME}/.local/bin:${PATH}"'
  echo ""
  echo "Then reload your shell or run: source ~/.zshrc  (or ~/.bashrc)"
else
  echo "caravan is on PATH. Run: caravan version"
fi
