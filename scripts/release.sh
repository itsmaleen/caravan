#!/usr/bin/env bash
# scripts/release.sh — build cross-platform caravan release artifacts
#
# Usage: bash scripts/release.sh
# Outputs to dist/ in the repo root. Idempotent (wipes dist/ first).
#
# Prerequisites: Go toolchain, shasum (macOS built-in or coreutils sha256sum)

set -euo pipefail

# Resolve repo root (works whether called from root or scripts/)
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# ---------------------------------------------------------------------------
# Read version from internal/buildinfo/buildinfo.go
# ---------------------------------------------------------------------------
VERSION="$(grep -E 'const Version\s*=' internal/buildinfo/buildinfo.go \
  | sed 's/.*Version[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/')"

if [[ -z "$VERSION" ]]; then
  echo "ERROR: could not extract Version from internal/buildinfo/buildinfo.go" >&2
  exit 1
fi

echo "caravan release build — version: ${VERSION}"
echo ""

# ---------------------------------------------------------------------------
# Clean slate
# ---------------------------------------------------------------------------
rm -rf dist
mkdir -p dist

# ---------------------------------------------------------------------------
# Build matrix
# ---------------------------------------------------------------------------
PLATFORMS=(
  "darwin arm64"
  "darwin amd64"
  "linux  arm64"
  "linux  amd64"
)

declare -a ARTIFACTS=()

for PLATFORM in "${PLATFORMS[@]}"; do
  # shellcheck disable=SC2086
  GOOS=$(echo $PLATFORM | awk '{print $1}')
  # shellcheck disable=SC2086
  GOARCH=$(echo $PLATFORM | awk '{print $2}')

  ARTIFACT_NAME="caravan-${VERSION}-${GOOS}-${GOARCH}"
  BINARY_PATH="dist/${ARTIFACT_NAME}"
  TARBALL_PATH="dist/${ARTIFACT_NAME}.tar.gz"

  printf "  building %-30s ... " "${ARTIFACT_NAME}"

  CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
    go build -trimpath \
    -ldflags="-s -w" \
    -o "${BINARY_PATH}" \
    .

  # Create tarball: single binary named `caravan` inside
  # Use a temp dir so the entry inside the tar is just `caravan`
  TMPDIR_TAR="$(mktemp -d)"
  cp "${BINARY_PATH}" "${TMPDIR_TAR}/caravan"
  tar -czf "${TARBALL_PATH}" -C "${TMPDIR_TAR}" caravan
  rm -rf "${TMPDIR_TAR}"

  SIZE="$(wc -c < "${TARBALL_PATH}" | tr -d ' ')"
  printf "OK  (%d bytes)\n" "$SIZE"

  ARTIFACTS+=("${ARTIFACT_NAME}")
done

# ---------------------------------------------------------------------------
# Checksums (shasum -a 256; fallback to sha256sum on Linux)
# ---------------------------------------------------------------------------
echo ""
echo "generating checksums ..."

CHECKSUM_FILE="dist/checksums.txt"

(
  cd dist
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 ./*.tar.gz > checksums.txt
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz > checksums.txt
  else
    echo "ERROR: no shasum or sha256sum found on PATH" >&2
    exit 1
  fi
)

# ---------------------------------------------------------------------------
# Summary table
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
printf "  caravan v%-s release artifacts\n" "$VERSION"
echo "============================================================"
printf "  %-42s  %10s\n" "artifact" "size"
echo "  ----------------------------------------------------------"

for ARTIFACT_NAME in "${ARTIFACTS[@]}"; do
  TARBALL="dist/${ARTIFACT_NAME}.tar.gz"
  SIZE="$(wc -c < "${TARBALL}" | tr -d ' ')"
  printf "  %-42s  %8d B\n" "${ARTIFACT_NAME}.tar.gz" "$SIZE"
done

echo ""
echo "  checksums (SHA-256):"
while IFS= read -r line; do
  printf "    %s\n" "$line"
done < "$CHECKSUM_FILE"

echo ""
echo "  dist/ is gitignored — commit scripts and Makefile, not dist/"
echo "============================================================"
echo ""
echo "Done. Artifacts in dist/"
