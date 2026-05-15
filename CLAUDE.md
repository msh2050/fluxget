# FluxGet — Project Guide for Claude

## What This Project Is

**FluxGet** is an IDM-inspired download manager built on the **Surge** Go engine.
It adds a Chrome/Edge/Firefox browser extension that intercepts downloads and captures
video streams from YouTube, Vimeo, Twitch, and 30+ platforms, routing everything
through a native HLS/DASH segment engine with ffmpeg mux — no yt-dlp required for
manifest URLs or YouTube adaptive streams.

The IDM techniques were reverse-engineered from the real IDM binaries:
`IDMNetMon.dll` (site dispatch table, segment skip logic), `IDMVMPrs.dll` (DASH MPD
parser template expansion), `idmbrbtn.dll` (floating button overlay),
`idmvconv.dll` (lossless container remux).

Full setup guide: `docs/FLUXGET.md`

---

## Project Structure

```
surge/
├── main.go
├── cmd/
│   ├── http_api.go          HTTP routes — /download /stream /ytdlp /events /ui /health + all management
│   ├── root_downloads.go    /download handler, DownloadRequest struct
│   ├── root_http_server.go  Binds to port 1700 by default (0.0.0.0)
│   └── server.go            `surge server start/stop/status` subcommands
├── internal/
│   ├── stream/
│   │   ├── hls.go           ★ HLS m3u8 parser (master + media, best-variant selection)
│   │   ├── dash.go          ★ DASH MPD XML parser ($Number$/$Time$/$Bandwidth$ expansion)
│   │   └── downloader.go    ★ Parallel segment fetcher, ffmpeg mux, YouTube CDN fragment skip
│   ├── webui/
│   │   ├── ui.go            ★ //go:embed wrapper — serves dashboard at /ui
│   │   └── ui.html          ★ Dark SSE-connected web dashboard
│   ├── ytdlp/
│   │   └── ytdlp.go         yt-dlp subprocess runner, NeedsYtDlp() dispatch logic
│   ├── engine/
│   │   ├── events/events.go SSE event types (ProgressMsg, DownloadCompleteMsg, …)
│   │   └── concurrent/      Multi-connection HTTP chunk downloader
│   └── processing/
│       └── manager.go       Download lifecycle: probe → reserve → dispatch to engine
├── extension-nexload/       ★ Browser extension (MV3, no build step — load unpacked)
│   ├── manifest.json        Permissions: downloads, notifications, webRequest, scripting
│   ├── document.js          MAIN world: captures ytInitialPlayerResponse, patches fetch/XHR
│   ├── background.js        Service worker: download interception, site dispatch, routing
│   ├── content.js           Injected: FluxGet button on <video>, quality picker, postMessage relay
│   ├── popup.html/js        Connection status popup
│   └── icons/
└── docs/
    └── FLUXGET.md           ★ Full setup, API reference, troubleshooting guide
```

---

## Quick Start

```bash
# Build (Go 1.25+ required — binary is at ~/.local/go-install/go/bin/go)
~/.local/go-install/go/bin/go build -o fluxget .

# Run backend (headless)
./fluxget server start --port 1700

# OR run with TUI
./fluxget

# Verify
curl http://127.0.0.1:1700/health
# → {"status":"ok","port":1700}

# Load extension: chrome://extensions → Developer mode → Load unpacked → extension-nexload/

# Web dashboard
xdg-open http://127.0.0.1:1700/ui
```

---

## HTTP API

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/health` | Health check → `{status, port}` |
| GET | `/events` | SSE stream of all download events |
| POST | `/download` | Queue file download (multi-connection engine) |
| POST | `/stream` | Queue video/stream (native HLS/DASH or yt-dlp fallback) |
| POST | `/ytdlp` | Queue video via yt-dlp explicitly |
| GET | `/ui` | Web dashboard (dark theme, SSE live view) |
| GET | `/list` | Active downloads |
| GET | `/history` | Completed downloads |
| POST | `/pause?id=` | Pause |
| POST | `/resume?id=` | Resume |
| POST | `/delete?id=` | Remove |
| PUT | `/update-url?id=` | Update stale URL |
| POST | `/open-file?id=` | Open file (loopback only) |
| POST | `/open-folder?id=` | Open folder (loopback only) |

### /stream routing logic (mirrors IDMNetMon.dll dispatch table)

1. `formats[]` non-empty → native adaptive (YouTube CDN direct signed URLs → video + audio + ffmpeg mux)
2. `.m3u8` / `mpegurl` in URL → native HLS (parse → parallel segments → ffmpeg concat)
3. `.mpd` / `dash+xml` in URL → native DASH (parse MPD XML → parallel segments → ffmpeg mux)
4. `application/vnd.lumberjack.manifest` → native Lumberjack handler
5. Known platform page → yt-dlp fallback
6. Direct video URL → native single-connection download

---

## Extension Architecture

Three layers — mirrors IDM's three-layer design:

```
document.js  (MAIN world)          — reads ytInitialPlayerResponse, patches fetch/XHR/MediaSource
     ↓ window.postMessage(__fluxget)
content.js   (extension world)     — relays messages, injects floating button + quality picker
     ↓ chrome.runtime.sendMessage
background.js (service worker)     — intercepts downloads, routes to /download or /stream
     ↓ fetch POST
Backend      (port 1700)           — engine, SSE, ffmpeg mux
```

### Key constants in background.js

- `FLUXGET_PORT = 1700` — must match backend port
- `INTERCEPT_EXTS` — set of file extensions to intercept from chrome.downloads
- `INTERCEPT_MIME` — regex for MIME types to always intercept
- `SKIP_FRAG_RE` — skip .ts/.m4s adaptive chunks in webRequest capture
- `isYouTubeCDNFragment(url)` — exact IDMNetMon segment skip logic (M4S in /avf/ or /sep/, MP4 in /avf/ or /parcel/)
- `KNOWN_VIDEO_HOSTS` — 30+ platform hostnames, exact order from IDMNetMon dispatch table

---

## Key Files to Edit

| Task | File |
|------|------|
| Add a new video platform | `background.js` KNOWN_VIDEO_HOSTS + `internal/ytdlp/ytdlp.go` NeedsYtDlp() |
| Change intercept rules | `background.js` INTERCEPT_EXTS / INTERCEPT_MIME |
| Fix HLS parsing | `internal/stream/hls.go` |
| Fix DASH parsing | `internal/stream/dash.go` |
| Change download/mux logic | `internal/stream/downloader.go` |
| Add API endpoint | `cmd/http_api.go` registerHTTPRoutes() |
| Change web UI | `internal/webui/ui.html` |
| Change extension button | `extension-nexload/content.js` |
| Change YouTube data extraction | `extension-nexload/document.js` |

---

## Build

```bash
# Full build check
~/.local/go-install/go/bin/go build ./...

# Vet
~/.local/go-install/go/bin/go vet ./...

# Tests
~/.local/go-install/go/bin/go test ./...

# Build binary
~/.local/go-install/go/bin/go build -o fluxget .
```

Go binary location: `~/.local/go-install/go/bin/go` (was downloaded here in a prior session).
If missing: `curl -fsSL https://go.dev/dl/go1.25.3.linux-amd64.tar.gz | tar -C /tmp -xz`

---

## Dependencies

- **Go 1.25+** — build
- **ffmpeg** — mux HLS/DASH segments at runtime (`sudo apt install ffmpeg`)
- **yt-dlp** — fallback for platform pages at runtime (`pip install yt-dlp`)
- No npm/Node required for the extension

---

## Design Decisions

- **Fixed port 1700** — extension always targets `127.0.0.1:1700`, no port discovery needed
- **No build step for extension** — plain MV3 JS, load unpacked directly
- **document.js in MAIN world** — required to read `window.ytInitialPlayerResponse` and patch `fetch`/`XHR`; uses `window.postMessage` to relay to content.js
- **Native HLS/DASH first** — avoids yt-dlp subprocess for manifest URLs; yt-dlp is fallback only for platform pages where no manifest URL is known
- **ffmpeg `-c copy`** — lossless container remux (no re-encoding), mirrors `idmvconv.dll` approach
- **IDMNetMon dispatch order preserved** — `NeedsYtDlp()` and `KNOWN_VIDEO_HOSTS` follow the exact priority order from the decompiled IDMNetMon.dll site dispatch table
