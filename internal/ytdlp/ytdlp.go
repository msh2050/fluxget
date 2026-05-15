// Package ytdlp wraps yt-dlp as a download backend for video URLs.
// It runs yt-dlp as a subprocess, parses its progress output, and publishes
// events into Surge's SSE stream so the TUI / extension see live progress.
package ytdlp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/msh2050/fluxget/internal/engine/events"
	"github.com/google/uuid"
)

// Publisher is the subset of core.DownloadService we need.
type Publisher interface {
	Publish(msg interface{}) error
}

// Request holds everything needed to start a yt-dlp download.
type Request struct {
	URL      string
	Title    string // used as filename hint
	Format   string // yt-dlp format selector, e.g. "bestvideo+bestaudio/best"
	DestDir  string
	Headers  map[string]string
}

// NeedsYtDlp returns true for URLs that should be handled by yt-dlp.
// The host list and priority order mirrors IDMNetMon.dll's site dispatch table
// (decompiled, lines 35974-36059):
//   Lumberjack manifest → MPD extension → YouTube CDN hosts → Facebook →
//   Vimeo → Instagram → ok.ru/mycdn → hydrax.to → generic HLS/DASH → yt-dlp platforms
//
// CDN hosts (googlevideo.com, fbcdn.net, vimeocdn.com, cdninstagram.com, etc.)
// serve direct media URLs and are handled by the native stream engine, not yt-dlp.
func NeedsYtDlp(rawURL string) bool {
	u := strings.ToLower(rawURL)

	// ── Native engine handles these (do NOT send to yt-dlp) ─────────────────
	// Direct manifest URLs are parsed natively by internal/stream
	if strings.Contains(u, ".m3u8") || strings.Contains(u, ".mpd") ||
		strings.Contains(u, "mpegurl") || strings.Contains(u, "dash+xml") ||
		strings.Contains(u, "vnd.lumberjack.manifest") {
		return false
	}

	// YouTube CDN — direct signed video URLs from ytInitialPlayerResponse
	if hostMatches(u, "googlevideo.com") || hostMatches(u, "youtube.googleapis.com") {
		return false
	}
	// Facebook CDN
	if hostSuffix(u, ".fbcdn.net") {
		return false
	}
	// Vimeo CDN
	if hostSuffix(u, "vimeocdn.com") || hostSuffix(u, "-vimeo.akamaized.net") ||
		hostSuffix(u, "-adaptive.akamaized.net") {
		return false
	}
	// Instagram CDN
	if hostSuffix(u, ".cdninstagram.com") {
		return false
	}
	// VK / ok.ru CDN (byte-range URLs: &bytes=start-end)
	if hostSuffix(u, ".vkuser.net") || hostSuffix(u, ".mycdn.me") {
		return false
	}
	// SoundCloud CDN
	if hostSuffix(u, "-hls-media.sndcdn.com") {
		return false
	}

	// ── yt-dlp page-level platforms (exact IDMNetMon dispatch order) ─────────
	// 3. YouTube
	if hostMatches(u, "youtube.com") || hostMatches(u, "youtu.be") ||
		hostMatches(u, "youtube-nocookie.com") ||
		hostMatches(u, "drive.google.com") || hostMatches(u, "docs.google.com") {
		return true
	}
	// 4. Facebook / Meta
	if hostMatches(u, "facebook.com") || hostMatches(u, "workplace.com") ||
		hostMatches(u, "fb.watch") {
		return true
	}
	// 5. Vimeo
	if hostMatches(u, "vimeo.com") {
		return true
	}
	// 6. Instagram
	if hostMatches(u, "instagram.com") {
		return true
	}
	// 7. ok.ru / VK
	if hostMatches(u, "ok.ru") || hostMatches(u, "vk.com") {
		return true
	}
	// 8. Hydrax (when path is /playlist/ or /hls/)
	if hostMatches(u, "hydrax.to") {
		return true
	}
	// Udemy (uses HLS streams on course pages)
	if hostMatches(u, "udemy.com") {
		return true
	}

	// Additional platforms not in IDMNetMon but supported by yt-dlp
	ytdlpPlatforms := []string{
		"twitch.tv", "twitch.com",
		"dailymotion.com",
		"tiktok.com",
		"twitter.com", "x.com",
		"reddit.com",
		"bilibili.com",
		"nicovideo.jp",
		"soundcloud.com",
		"bandcamp.com",
		"rumble.com",
		"odysee.com",
		"streamable.com",
		"mixcloud.com",
		"yandex.ru",
	}
	for _, h := range ytdlpPlatforms {
		if hostMatches(u, h) {
			return true
		}
	}

	return false
}

// hostMatches returns true if the URL's host equals or ends with "."+host.
func hostMatches(u, host string) bool {
	return strings.Contains(u, host)
}

// hostSuffix returns true if the URL contains the given suffix as a host component.
func hostSuffix(u, suffix string) bool {
	return strings.Contains(u, suffix)
}

// findBin returns the path to yt-dlp, preferring a local binary.
func findBin() string {
	// Check next to the current executable first
	if exe, err := os.Executable(); err == nil {
		local := filepath.Join(filepath.Dir(exe), "yt-dlp")
		if _, err := os.Stat(local); err == nil {
			return local
		}
	}
	if path, err := exec.LookPath("yt-dlp"); err == nil {
		return path
	}
	return "yt-dlp"
}

// Start runs yt-dlp for req, publishing progress events to pub.
// It returns the download ID immediately; the download runs in the background.
func Start(ctx context.Context, pub Publisher, req Request) (string, error) {
	bin := findBin()
	if _, err := exec.LookPath(bin); err != nil {
		if bin == "yt-dlp" {
			return "", fmt.Errorf("yt-dlp not found in PATH — install it with: pip install yt-dlp")
		}
	}

	id := uuid.New().String()
	format := req.Format
	if format == "" {
		format = "bestvideo+bestaudio/best"
	}

	destDir := req.DestDir
	if destDir == "" {
		home, _ := os.UserHomeDir()
		destDir = filepath.Join(home, "Downloads")
	}
	_ = os.MkdirAll(destDir, 0o755)

	outTmpl := filepath.Join(destDir, "%(title)s.%(ext)s")

	args := []string{
		"-f", format,
		"-o", outTmpl,
		"--newline",
		"--progress-template",
		"%(progress.status)s\t%(progress.downloaded_bytes)s\t%(progress.total_bytes)s\t%(progress.speed)s\t%(progress.eta)s",
		"--no-playlist",
		"--user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		"--js-runtimes", "node",
	}
	for k, v := range req.Headers {
		args = append(args, "--add-header", fmt.Sprintf("%s:%s", k, v))
	}
	args = append(args, req.URL)

	// Announce queued
	filename := req.Title
	if filename == "" {
		filename = filenameFromURL(req.URL)
	}
	_ = pub.Publish(events.DownloadQueuedMsg{
		DownloadID: id,
		Filename:   filename,
		URL:        req.URL,
		DestPath:   destDir,
	})

	go func() {
		cmd := exec.CommandContext(ctx, bin, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = pub.Publish(events.DownloadErrorMsg{DownloadID: id, Filename: filename, Err: err})
			return
		}
		if err := cmd.Start(); err != nil {
			_ = pub.Publish(events.DownloadErrorMsg{DownloadID: id, Filename: filename, Err: err})
			return
		}

		_ = pub.Publish(events.DownloadStartedMsg{
			DownloadID: id,
			URL:        req.URL,
			Filename:   filename,
			DestPath:   destDir,
		})

		start := time.Now()
		var lastDownloaded int64
		var lastTotal int64

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Split(line, "\t")
			if len(parts) < 5 {
				continue
			}
			status := strings.TrimSpace(parts[0])
			downloaded := parseInt64(parts[1])
			total := parseInt64(parts[2])
			speed := parseFloat64(parts[3])

			if downloaded > 0 {
				lastDownloaded = downloaded
			}
			if total > 0 {
				lastTotal = total
			}

			if status == "downloading" || status == "finished" {
				_ = pub.Publish(events.ProgressMsg{
					DownloadID:        id,
					Downloaded:        lastDownloaded,
					Total:             lastTotal,
					Speed:             speed,
					Elapsed:           time.Since(start),
					ActiveConnections: 1,
				})
			}
		}

		if err := cmd.Wait(); err != nil {
			_ = pub.Publish(events.DownloadErrorMsg{
				DownloadID: id,
				Filename:   filename,
				DestPath:   destDir,
				Err:        fmt.Errorf("yt-dlp: %w", err),
			})
			return
		}

		elapsed := time.Since(start)
		avgSpeed := 0.0
		if elapsed.Seconds() > 0 && lastTotal > 0 {
			avgSpeed = float64(lastTotal) / elapsed.Seconds()
		}
		_ = pub.Publish(events.DownloadCompleteMsg{
			DownloadID: id,
			Filename:   filename,
			Elapsed:    elapsed,
			Total:      lastTotal,
			AvgSpeed:   avgSpeed,
		})
	}()

	return id, nil
}

func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func parseFloat64(s string) float64 {
	s = strings.TrimSpace(s)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func filenameFromURL(u string) string {
	parts := strings.Split(u, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.Split(parts[i], "?")[0]
		if p != "" {
			return p
		}
	}
	return "video"
}
