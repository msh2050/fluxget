#!/usr/bin/env bash
# Build the Windows GUI binary from Linux using mingw-w64.
# Prerequisites: sudo apt install gcc-mingw-w64-x86-64
set -euo pipefail

WAILS=~/go/bin/wails
OUT=../release-assets/fluxget-gui-windows-amd64.exe

echo "Building Windows GUI..."
cd "$(dirname "$0")/gui"
CC=x86_64-w64-mingw32-gcc \
  GOARCH=amd64 \
  "$WAILS" build \
    -platform windows/amd64 \
    -ldflags="-s -w" \
    -o "$OUT" \
    -skipbindings \
    2>&1

echo "Done. Output: release-assets/fluxget-gui-windows-amd64.exe"
echo ""
echo "To add it to the existing GitHub release:"
echo "  gh release upload v1.0.0 release-assets/fluxget-gui-windows-amd64.exe"
