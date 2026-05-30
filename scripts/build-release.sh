#!/usr/bin/env bash
#
# build-release.sh — build the FluxGet CLI + Wails GUI and package a .deb.
#
# This codifies the release flow that was previously run by hand:
#   1. build CLI  -> release-assets/fluxget-linux-amd64
#   2. build GUI  -> release-assets/fluxget-gui-linux-amd64   (wails, webkit2_41)
#   3. stage a Debian tree in /tmp/fluxget-deb and dpkg-deb --build it
#   4. emit release-assets/fluxget_<version>_amd64.deb
#
# The version is read from cmd/root.go (var Version) unless passed as $1.
# The build is stamped with the git short hash (cmd.Commit) so every fresh
# build is visibly distinct in the GUI titlebar (served via /health).
#
# Usage:
#   scripts/build-release.sh           # version from cmd/root.go
#   scripts/build-release.sh 2.1.2     # override version
#
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

GO="${GO:-$HOME/.local/go-install/go/bin/go}"
WAILS="${WAILS:-$HOME/go/bin/wails}"
export PATH="$(dirname "$GO"):$(dirname "$WAILS"):$PATH"

MODULE="github.com/msh2050/fluxget"

# --- version + build metadata -------------------------------------------------
VERSION="${1:-$(grep -oP 'Version\s*=\s*"\K[^"]+' cmd/root.go | head -1)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
BUILDTIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w \
  -X ${MODULE}/cmd.Version=${VERSION} \
  -X ${MODULE}/cmd.Commit=${COMMIT} \
  -X ${MODULE}/cmd.BuildTime=${BUILDTIME}"

echo "==> Building FluxGet ${VERSION} (${COMMIT}) @ ${BUILDTIME}"
mkdir -p release-assets

# --- 1. CLI / headless server -------------------------------------------------
echo "==> [1/4] CLI -> release-assets/fluxget-linux-amd64"
"$GO" build -ldflags "$LDFLAGS" -o release-assets/fluxget-linux-amd64 .

# --- 2. Wails GUI -------------------------------------------------------------
echo "==> [2/4] GUI (wails, webkit2_41) -> release-assets/fluxget-gui-linux-amd64"
( cd gui && "$WAILS" build -clean -tags webkit2_41 -ldflags "$LDFLAGS" )
cp gui/build/bin/fluxget-gui release-assets/fluxget-gui-linux-amd64

# --- 3. stage the Debian tree -------------------------------------------------
echo "==> [3/4] Staging Debian package tree"
STAGE=/tmp/fluxget-deb
rm -rf "$STAGE"
mkdir -p "$STAGE"/DEBIAN \
         "$STAGE"/usr/local/bin \
         "$STAGE"/usr/share/applications \
         "$STAGE"/usr/share/icons/hicolor/128x128/apps \
         "$STAGE"/usr/share/icons/hicolor/48x48/apps \
         "$STAGE"/usr/lib/systemd/user

install -m 755 release-assets/fluxget-linux-amd64     "$STAGE"/usr/local/bin/fluxget
install -m 755 release-assets/fluxget-gui-linux-amd64 "$STAGE"/usr/local/bin/fluxget-gui

cp extension-nexload/icons/icon128.png "$STAGE"/usr/share/icons/hicolor/128x128/apps/fluxget.png
cp extension-nexload/icons/icon48.png  "$STAGE"/usr/share/icons/hicolor/48x48/apps/fluxget.png

cat > "$STAGE"/usr/share/applications/fluxget.desktop <<'EOF'
[Desktop Entry]
Name=FluxGet
Comment=IDM-inspired download manager with browser extension
Exec=fluxget-gui
Icon=fluxget
Terminal=false
Type=Application
Categories=Network;FileTransfer;
Keywords=download;video;stream;youtube;hls;dash;
StartupNotify=true
StartupWMClass=fluxget-gui
EOF

cat > "$STAGE"/usr/lib/systemd/user/fluxget.service <<'EOF'
[Unit]
Description=FluxGet download manager daemon
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/fluxget server start --port 1700
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF

cat > "$STAGE"/DEBIAN/control <<EOF
Package: fluxget
Version: ${VERSION}
Section: net
Priority: optional
Architecture: amd64
Depends: ffmpeg, libwebkit2gtk-4.1-0, libgtk-3-0, libayatana-appindicator3-1
Recommends: yt-dlp
Maintainer: FluxGet <https://github.com/msh2050/fluxget>
Homepage: https://github.com/msh2050/fluxget
Description: IDM-inspired download manager with browser extension
 FluxGet is a download manager that intercepts browser video streams
 from YouTube, Vimeo, Twitch, and 30+ platforms. It features native
 HLS/DASH parsing, AES-128 decryption, parallel segment downloading,
 ffmpeg muxing, and a browser extension for Chrome/Edge.
 .
 Includes both a TUI/headless server (fluxget) and a desktop GUI
 (fluxget-gui) built with Wails and WebKit2GTK. The desktop GUI shows
 a system tray icon and can start automatically on login.
EOF

cat > "$STAGE"/DEBIAN/postinst <<'EOF'
#!/bin/sh
set -e
if [ "$1" = "configure" ]; then
    update-icon-caches /usr/share/icons/hicolor 2>/dev/null || true
    update-desktop-database -q 2>/dev/null || true
fi
EOF
chmod 755 "$STAGE"/DEBIAN/postinst

# --- 4. build the .deb --------------------------------------------------------
echo "==> [4/4] dpkg-deb --build"
DEB="release-assets/fluxget_${VERSION}_amd64.deb"
dpkg-deb --build --root-owner-group "$STAGE" "$DEB"

echo
echo "Built: $DEB"
ls -lh "$DEB"
echo
echo "Install with:"
echo "  sudo dpkg -i $ROOT/$DEB"
