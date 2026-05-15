# FluxGet — Full Setup & Run Guide

FluxGet is an IDM-inspired download manager built on top of the Surge engine.
It adds a Chrome/Edge/Firefox browser extension that intercepts downloads, captures
video streams from YouTube, Vimeo, Twitch, and 30+ platforms, and routes everything
through Surge's multi-connection HTTP engine with native HLS/DASH support.

---

## Table of Contents

1. [How It Works](#1-how-it-works)
2. [Prerequisites](#2-prerequisites)
3. [Build from Source](#3-build-from-source)
4. [Run the Backend](#4-run-the-backend)
5. [Install the Browser Extension](#5-install-the-browser-extension)
6. [Web Dashboard](#6-web-dashboard)
7. [CLI Usage](#7-cli-usage)
8. [API Reference](#8-api-reference)
9. [Configuration](#9-configuration)
10. [Supported Sites & Formats](#10-supported-sites--formats)
11. [Troubleshooting](#11-troubleshooting)

---

## 1. How It Works

```
Browser                    FluxGet Extension              Backend (port 1700)
──────────────────────     ──────────────────────────     ──────────────────────────────
You click a download  ──►  chrome.downloads.onCreated     POST /download
                           cancels browser download  ──►  Surge multi-connection engine

You visit YouTube.com ──►  document.js reads                
                           ytInitialPlayerResponse    ──►  POST /stream
                           (signed CDN URLs, DASH)         native HLS/DASH downloader
                                                           or yt-dlp fallback
                                                           + ffmpeg mux (-c copy)
                           
You right-click link  ──►  context menu               ──►  POST /download or /stream

You open the popup    ──►  GET /ui                         web dashboard (SSE live view)
```

**Three layers (mirrors IDM's architecture):**

| Layer | IDM equivalent | FluxGet equivalent |
|---|---|---|
| Browser extension | IDMNetMon.dll socket hook | `background.js` chrome.downloads API |
| Page-level capture | document.js XHR/fetch hook | `document.js` MAIN-world injection |
| Native video button | idmbrbtn.dll overlay | `content.js` floating button |

---

## 2. Prerequisites

| Tool | Required for | Install |
|---|---|---|
| **Go 1.25+** | Build the backend | `https://go.dev/dl/` |
| **ffmpeg** | Muxing HLS/DASH segments (video + audio) | `sudo apt install ffmpeg` |
| **yt-dlp** | Fallback for platform pages (YouTube, Vimeo, etc.) | `pip install yt-dlp` or `sudo apt install yt-dlp` |
| Chrome / Edge / Brave / Firefox | Browser extension | — |

Only **Go** is required to build. `ffmpeg` and `yt-dlp` are runtime-only and both
have graceful fallbacks if missing (see [Troubleshooting](#11-troubleshooting)).

---

## 3. Build from Source

```bash
# Clone
git clone https://github.com/SurgeDM/Surge.git
cd Surge

# Build binary (outputs ./surge)
go build -o fluxget .

# Or with version info
go build -ldflags "-X main.Version=2.0.0" -o fluxget .
```

The binary is fully self-contained — no install step needed.

### Quick test build

```bash
go build ./...   # compile everything, check for errors
```

---

## 4. Run the Backend

The backend is a single binary. It exposes an HTTP API on **port 1700** (fixed)
that the browser extension talks to.

### Mode A — Interactive TUI (recommended for desktop use)

```bash
./fluxget
```

Opens a terminal dashboard with:
- Live download queue with progress bars and speed graphs
- Keyboard controls: `d` delete, `p` pause, `r` resume, `?` help
- Auto-resumes paused downloads on restart

### Mode B — Headless server (daemon, no terminal UI)

```bash
# Start in background
./fluxget server start &

# Or foreground with explicit port
./fluxget server start --port 1700

# Check status
./fluxget server status

# Stop
./fluxget server stop
```

### Mode C — systemd service (Linux, persistent across reboots)

Create `/etc/systemd/system/fluxget.service`:

```ini
[Unit]
Description=FluxGet download manager backend
After=network.target

[Service]
Type=simple
User=YOUR_USERNAME
ExecStart=/path/to/fluxget server start --port 1700 --output /home/YOUR_USERNAME/Downloads
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now fluxget
sudo systemctl status fluxget
```

### Verify it's running

```bash
curl http://127.0.0.1:1700/health
# → {"status":"ok","port":1700}
```

When the extension popup shows a green dot, the backend is reachable.

---

## 5. Install the Browser Extension

The extension is in `extension-nexload/`. It is a plain MV3 extension with **no build step** —
load it directly as an unpacked extension.

### Chrome / Edge / Brave

1. Open `chrome://extensions` (or `edge://extensions`)
2. Enable **Developer mode** (toggle, top-right)
3. Click **Load unpacked**
4. Select the `extension-nexload/` folder
5. The FluxGet icon appears in your toolbar

### Firefox

Firefox requires a temporary install (unsigned extension):

1. Open `about:debugging#/runtime/this-firefox`
2. Click **Load Temporary Add-on**
3. Select `extension-nexload/manifest.json`

> **Note:** Temporary installs in Firefox are removed when the browser restarts.
> For permanent install, the extension must be signed via addons.mozilla.org.

### Verify extension is working

1. Start the backend (`./fluxget` or `./fluxget server start`)
2. Click the FluxGet toolbar icon
3. The popup should show a **green dot** and "Connected"
4. If you see a red dot "OFF" — the backend is not running on port 1700

---

## 6. Web Dashboard

Once the backend is running, open:

```
http://127.0.0.1:1700/ui
```

The dashboard provides:
- **Live download list** with progress bars (updated via SSE, no polling)
- **Pause / Resume / Remove** buttons per download
- **Quick URL bar** — paste any URL and click Download
- **Stream modal** — force yt-dlp or native HLS/DASH engine
- **System log** — shows all backend events in real time

The extension's context menu "Open FluxGet dashboard" also opens this page.

---

## 7. CLI Usage

When the backend is running, you can queue downloads from any terminal tab:

```bash
# Add a file download
./fluxget add https://example.com/file.zip

# Add multiple URLs
./fluxget add https://example.com/a.zip https://example.com/b.zip

# Add from a file (one URL per line)
./fluxget add --batch urls.txt

# Set output directory
./fluxget add --output ~/Videos https://example.com/movie.mp4

# List active downloads
./fluxget ls

# Pause a download
./fluxget pause <id>

# Resume a download
./fluxget resume <id>

# Delete a download
./fluxget rm <id>
```

---

## 8. API Reference

The backend exposes a REST API on `http://127.0.0.1:1700`.
The extension calls this API directly; you can also call it with `curl`.

### Health check

```
GET /health
```
```json
{ "status": "ok", "port": 1700 }
```

---

### Download a file (multi-connection HTTP engine)

```
POST /download
Content-Type: application/json
```

```json
{
  "url": "https://example.com/file.zip",
  "filename": "file.zip",
  "path": "/home/user/Downloads",
  "headers": { "Cookie": "session=abc123" },
  "skip_approval": true
}
```

Response:
```json
{ "status": "queued", "id": "550e8400-...", "filename": "file.zip" }
```

The `skip_approval` flag bypasses the TUI confirmation prompt (used by the extension).

---

### Download a stream / video (native HLS/DASH engine)

```
POST /stream
Content-Type: application/json
```

```json
{
  "url": "https://example.com/stream.m3u8",
  "title": "My Video",
  "path": "/home/user/Videos",
  "headers": {},
  "formats": []
}
```

The `formats` array is optional. When the extension detects YouTube adaptive format
URLs from `ytInitialPlayerResponse`, it sends them here so the native engine can
download video + audio tracks directly from Google's CDN (no yt-dlp needed):

```json
{
  "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
  "title": "Rick Astley",
  "formats": [
    { "itag": 137, "mimeType": "video/mp4", "height": 1080, "bitrate": 4000000,
      "url": "https://rr1---sn-xxx.googlevideo.com/videoplayback?..." },
    { "itag": 140, "mimeType": "audio/mp4", "bitrate": 128000,
      "url": "https://rr1---sn-xxx.googlevideo.com/videoplayback?..." }
  ]
}
```

**Routing logic** (mirrors IDMNetMon dispatch table):

| URL type | Engine used |
|---|---|
| `formats[]` non-empty | Native: download video + audio CDN URLs → ffmpeg mux |
| `.m3u8` / `mpegurl` in URL | Native HLS: parse playlist → parallel segments → ffmpeg concat |
| `.mpd` / `dash+xml` in URL | Native DASH: parse MPD XML → parallel segments → ffmpeg mux |
| Known platform page (youtube.com, vimeo.com…) | yt-dlp fallback |
| Direct video URL (.mp4, .mkv…) | Native single-connection download |

Response:
```json
{ "status": "queued", "id": "550e8400-..." }
```

---

### yt-dlp download (explicit yt-dlp route)

```
POST /ytdlp
Content-Type: application/json
```

```json
{
  "url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
  "title": "My Video",
  "format": "bestvideo+bestaudio/best",
  "path": "/home/user/Videos"
}
```

---

### Live events (SSE)

```
GET /events
Accept: text/event-stream
```

Events emitted:

| Event | Data fields |
|---|---|
| `queued` | `DownloadID`, `Filename`, `URL`, `DestPath` |
| `started` | `DownloadID`, `Filename`, `URL` |
| `progress` | `DownloadID`, `Downloaded`, `Total`, `Speed`, `Elapsed`, `ActiveConnections` |
| `complete` | `DownloadID`, `Filename`, `Elapsed`, `Total`, `AvgSpeed` |
| `error` | `DownloadID`, `Filename`, `Err` |
| `paused` | `DownloadID`, `Filename` |
| `resumed` | `DownloadID`, `Filename` |
| `removed` | `DownloadID`, `Filename` |
| `system` | `Message` |

JavaScript example:
```js
const es = new EventSource('http://127.0.0.1:1700/events');
es.addEventListener('progress', e => {
  const d = JSON.parse(e.data);
  console.log(`${d.DownloadID}: ${d.Downloaded}/${d.Total} @ ${d.Speed} B/s`);
});
```

---

### List / manage downloads

```
GET  /list            → array of active download statuses
GET  /history         → array of completed downloads
POST /pause?id=<id>   → pause a download
POST /resume?id=<id>  → resume a paused download
POST /delete?id=<id>  → remove a download
PUT  /update-url?id=<id>  body: { "url": "..." }  → update stale URL
POST /open-file?id=<id>   → open the downloaded file (loopback only)
POST /open-folder?id=<id> → open containing folder (loopback only)
```

---

## 9. Configuration

Settings are stored in:
- **Linux:** `~/.config/surge/settings.json`
- **macOS:** `~/Library/Application Support/surge/settings.json`
- **Windows:** `%APPDATA%\surge\settings.json`

Key settings for FluxGet use:

```json
{
  "general": {
    "default_download_dir": "/home/user/Downloads"
  },
  "network": {
    "max_connections_per_host": 16
  }
}
```

You can also edit settings interactively with `s` inside the TUI.

### Extension settings

The extension stores its settings in `chrome.storage.local` under the key
`fluxget_settings`. You can change them from the popup:

| Setting | Default | Description |
|---|---|---|
| `interceptDownloads` | `true` | Intercept browser downloads and route to FluxGet |
| `autoDownloadVideo` | `false` | Auto-send detected video without showing quality picker |
| `port` | `1700` | Backend port |
| `destDir` | `""` | Download directory (empty = use backend default) |

---

## 10. Supported Sites & Formats

### Video platforms (page-level extraction, dispatch order from IDMNetMon.dll)

| Site | Method |
|---|---|
| youtube.com / youtu.be | Native: `ytInitialPlayerResponse` adaptive CDN URLs → no yt-dlp needed when extension is active |
| youtube-nocookie.com | Same as YouTube |
| googlevideo.com | Direct CDN URL → native concurrent downloader |
| drive.google.com / docs.google.com | yt-dlp |
| facebook.com / workplace.com | yt-dlp |
| vimeo.com | yt-dlp (native player manifest extraction planned) |
| instagram.com | yt-dlp |
| ok.ru / mycdn.me / vk.com | yt-dlp |
| hydrax.to | Native HLS (`/playlist/` or `/hls/` path detection) |
| udemy.com | Native HLS (`\.(m3u8|mpd)(\?|$)` path detection) |
| twitch.tv | yt-dlp (HLS: `^/playlist/\w+/(\d+)|^/hls/\w+/\w+`) |
| dailymotion.com | yt-dlp |
| tiktok.com | yt-dlp |
| twitter.com / x.com | yt-dlp |
| reddit.com | yt-dlp |
| bilibili.com | yt-dlp |
| nicovideo.jp | yt-dlp |
| soundcloud.com | yt-dlp (CDN: `*-hls-media.sndcdn.com` → native HLS) |
| bandcamp.com | yt-dlp |
| rumble.com | yt-dlp |
| yandex.ru (`strm.yandex.ru`) | yt-dlp |
| Any `.m3u8` URL | Native HLS parser |
| Any `.mpd` URL | Native DASH parser |
| `application/vnd.lumberjack.manifest` | Native Lumberjack handler |

### File download interception

The extension intercepts any browser download matching these extensions:

```
Archives:  zip rar 7z gz tar bz2 xz zst cab iso img dmg
Executables: exe msi deb rpm pkg apk appimage
Video:     mp4 mkv avi mov flv wmv webm m4v ts mpeg mpg 3gp ogv
Audio:     mp3 aac flac ogg opus wav m4a wma
Documents: pdf doc docx xls xlsx ppt pptx odt ods
Images:    jpg jpeg png gif webp bmp tiff svg
Streams:   m3u8 mpd
```

And any response with MIME type matching:
`video/*`, `audio/*`, `application/zip`, `application/x-rar`, `application/octet-stream`, etc.

### Segment skip logic (from IDMNetMon.dll lines 54504–54519)

The engine skips YouTube CDN adaptive fragments to avoid capturing individual chunks:
- `.m4s` in `/avf/` or `/sep/` path with `segment-` filename prefix
- `.mp4` in `/avf/` or `/parcel/` path
- `/api/manifest/init_segment`

---

## 11. Troubleshooting

### Extension shows red "OFF" badge

The backend is not running or not on port 1700.

```bash
# Check if it's running
curl http://127.0.0.1:1700/health

# Start it
./fluxget server start --port 1700
```

---

### "yt-dlp not found in PATH"

Install yt-dlp:
```bash
pip install yt-dlp

# Or download binary directly
sudo curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp
sudo chmod +x /usr/local/bin/yt-dlp
```

You can also place `yt-dlp` next to the `fluxget` binary — it checks that location first.

---

### "ffmpeg not found" — videos download but won't mux

Install ffmpeg:
```bash
sudo apt install ffmpeg        # Debian / Ubuntu
sudo dnf install ffmpeg        # Fedora
brew install ffmpeg            # macOS
```

Without ffmpeg:
- Single-stream HLS/direct downloads still work (raw file saved)
- MPEG-TS segments are byte-concatenated (playable for most TS content)
- YouTube adaptive (separate video + audio tracks) will fail to mux

---

### YouTube video has no sound (or only audio, no video)

This happens when only one adaptive track is sent to `/stream`. The extension
sends both video and audio tracks via the `formats[]` array from
`ytInitialPlayerResponse`. Make sure:
1. The extension is active on the YouTube tab (not disabled/blocked)
2. `document.js` loaded correctly (check DevTools → Console for errors)
3. ffmpeg is installed for muxing

---

### HLS download stops early / missing segments

Some HLS streams use token-expiring segment URLs. If segments return 403 after
a few minutes, you need to refresh the manifest. Use yt-dlp for such streams:

```bash
curl http://127.0.0.1:1700/ytdlp \
  -d '{"url":"https://example.com/live.m3u8","title":"Stream"}' \
  -H 'Content-Type: application/json'
```

---

### Extension not intercepting downloads on a specific site

The site may set the `Content-Disposition: attachment` header without a MIME type
FluxGet recognises. Check the download's MIME in DevTools → Network, then add it
to `INTERCEPT_MIME` in `background.js`.

---

### Port conflict — "could not bind to port 1700"

```bash
# Find what's using 1700
ss -tlnp | grep 1700

# Kill it or use a different port
./fluxget server start --port 1701
```

If you change the port, also update `FLUXGET_PORT` in `background.js`:
```js
const FLUXGET_PORT = 1701;
```

---

## Quick Start (TL;DR)

```bash
# 1. Build
cd /path/to/surge
go build -o fluxget .

# 2. Start backend
./fluxget server start &

# 3. Verify
curl http://127.0.0.1:1700/health

# 4. Load extension in Chrome
# chrome://extensions → Developer mode → Load unpacked → select extension-nexload/

# 5. Open dashboard
xdg-open http://127.0.0.1:1700/ui

# 6. Download something
# • Visit any site and click a video — FluxGet button appears
# • Right-click any link → "Download link with FluxGet"
# • Or paste a URL in the dashboard
```
