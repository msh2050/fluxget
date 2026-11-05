// Package ytdlp wraps yt-dlp as a download backend for video URLs.
// It runs yt-dlp as a subprocess, parses its progress output, and publishes
// events into Surge's SSE stream so the TUI / extension see live progress.
package ytdlp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/msh2050/fluxget/internal/types"
)

var debugLog *log.Logger

func init() {
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, "Downloads", "fluxget-debug.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		debugLog = log.New(io.Discard, "", 0)
		return
	}
	debugLog = log.New(f, "[ytdlp] ", log.LstdFlags)
}

// cookiePlatforms lists hosts where we pass --cookies-from-browser
// because login state is required for private/member-only content.
var cookiePlatforms = []string{
	"facebook.com", "fb.watch",
	"instagram.com",
	"twitter.com", "x.com",
	"tiktok.com",
	"reddit.com",
	"youtube.com", "youtu.be",
}

// Publisher is the subset of core.DownloadService we need.
type Publisher interface {
	Publish(msg types.DownloadEvent) error
}

// Request holds everything needed to start a yt-dlp download.
type Request struct {
	URL     string
	Title   string // used as filename hint
	Format  string // yt-dlp format selector, e.g. "bestvideo+bestaudio/best"
	DestDir string
	Headers map[string]string
}

// NeedsYtDlp returns true for URLs that should be handled by yt-dlp.
// The host list and priority order mirrors IDMNetMon.dll's site dispatch table
// (decompiled, lines 35974-36059):
//
//	Lumberjack manifest → MPD extension → YouTube CDN hosts → Facebook →
//	Vimeo → Instagram → ok.ru/mycdn → hydrax.to → generic HLS/DASH → yt-dlp platforms
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
		"huggingface.co",
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

// ── Cancellation registry ─────────────────────────────────────────────────────

var active sync.Map // download ID → context.CancelFunc

// Cancel stops a running yt-dlp download. Returns false when id is unknown.
func Cancel(id string) bool {
	if v, ok := active.LoadAndDelete(id); ok {
		v.(context.CancelFunc)()
		return true
	}
	return false
}

// Available reports whether a yt-dlp binary can be found.
func Available() bool {
	bin := findBin()
	if filepath.IsAbs(bin) {
		return true
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// Start runs yt-dlp for req, publishing progress events to pub.
// It returns the download ID immediately; the download runs in the background.
func Start(ctx context.Context, pub Publisher, req Request) (string, error) {
	if !Available() {
		return "", fmt.Errorf("yt-dlp not found in PATH \u2014 install it with: pip install -U yt-dlp")
	}
	id := uuid.New().String()
	destDir := resolveDestDir(req.DestDir)
	filename := req.Title
	if filename == "" {
		filename = filenameFromURL(req.URL)
	}
	_ = pub.Publish(types.DownloadEvent{Type: types.EventQueued,
		DownloadID: id,
		Filename:   filename,
		URL:        req.URL,
		DestPath:   destDir,
	})
	go func() {
		_ = pub.Publish(types.DownloadEvent{Type: types.EventStarted,
			DownloadID: id,
			URL:        req.URL,
			Filename:   filename,
			DestPath:   destDir,
		})
		if err := Run(ctx, pub, id, req); err != nil {
			_ = pub.Publish(types.DownloadEvent{Type: types.EventError,
				DownloadID: id,
				Filename:   filename,
				DestPath:   destDir,
				Err:        err,
			})
		}
	}()
	return id, nil
}

func resolveDestDir(dir string) string {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Downloads")
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Run executes yt-dlp synchronously under an existing download ID (the
// caller has already published Queued/Started). It publishes progress and,
// on success, the final Started(dest)+Complete pair. The returned error is
// for the caller to publish — Run never publishes EventError itself.
func Run(parent context.Context, pub Publisher, id string, req Request) error {
	bin := findBin()
	ctx, cancel := context.WithCancel(parent)
	active.Store(id, cancel)
	defer func() {
		active.Delete(id)
		cancel()
	}()

	destDir := resolveDestDir(req.DestDir)
	format := req.Format
	if format == "" {
		format = "bestvideo*+bestaudio/best"
	}

	start := time.Now()
	attempt := func(format string, withCookies bool) (string, int64, string, error) {
		args := buildArgs(req, destDir, format, withCookies)
		debugLog.Printf("start url=%s args=%v", req.URL, args)
		return runOnce(ctx, pub, id, bin, args, start)
	}

	browser := cookieBrowser(req.URL)
	finalPath, total, stderr, err := attempt(format, browser != "")
	if err != nil && ctx.Err() == nil {
		switch {
		case browser != "" && looksLikeCookieError(stderr):
			// Browser cookie DB unreadable (browser not installed, keyring
			// locked, profile in use…) — retry anonymously.
			debugLog.Printf("cookie extraction failed (%s), retrying without cookies", browser)
			finalPath, total, stderr, err = attempt(format, false)
		case strings.Contains(stderr, "Requested format is not available") && format != "best":
			debugLog.Printf("format not available, retrying with -f best url=%s", req.URL)
			finalPath, total, stderr, err = attempt("bv*+ba/b", browser != "")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return context.Canceled
		}
		debugLog.Printf("error url=%s\nstderr:\n%s", req.URL, stderr)
		return fmt.Errorf("yt-dlp: %s", lastMeaningfulLine(stderr, err))
	}

	filename := req.Title
	dest := destDir
	if finalPath != "" {
		filename = filepath.Base(finalPath)
		dest = finalPath
		if fi, statErr := os.Stat(finalPath); statErr == nil && fi.Size() > 0 {
			total = fi.Size()
		}
	}
	elapsed := time.Since(start)
	avgSpeed := 0.0
	if elapsed.Seconds() > 0 && total > 0 {
		avgSpeed = float64(total) / elapsed.Seconds()
	}
	// Re-announce with the real output path so the history entry (and
	// open-file / open-folder) point at the produced file, not the folder.
	_ = pub.Publish(types.DownloadEvent{Type: types.EventStarted,
		DownloadID: id, URL: req.URL, Filename: filename, DestPath: dest, Total: total})
	_ = pub.Publish(types.DownloadEvent{Type: types.EventComplete,
		DownloadID: id,
		Filename:   filename,
		Elapsed:    elapsed,
		Total:      total,
		AvgSpeed:   avgSpeed,
	})
	return nil
}

const finalPathMarker = "FLUXGET_FILE\t"

func buildArgs(req Request, destDir, format string, withCookies bool) []string {
	outTmpl := filepath.Join(destDir, "%(title).180B [%(id)s].%(ext)s")
	if t := strings.TrimSpace(req.Title); t != "" {
		outTmpl = filepath.Join(destDir, sanitizeTitle(t)+".%(ext)s")
	}
	args := []string{
		"-f", format,
		"-o", outTmpl,
		"--newline",
		"--progress",
		"--progress-template",
		"download:%(progress.status)s\t%(progress.downloaded_bytes)s\t%(progress.total_bytes)s\t%(progress.speed)s\t%(progress.eta)s\t%(progress.total_bytes_estimate)s",
		"--print", "after_move:" + finalPathMarker + "%(filepath)s",
		"--no-simulate",
		"--no-playlist",
		"--concurrent-fragments", "8",
		"--merge-output-format", "mp4/mkv",
		"--extractor-retries", "3",
		"--fragment-retries", "10",
		"--retries", "10",
		"--trim-filenames", "180",
	}
	if rt := jsRuntime(); rt != "" {
		args = append(args, "--js-runtimes", rt)
	}
	if withCookies {
		if b := cookieBrowser(req.URL); b != "" {
			args = append(args, "--cookies-from-browser", b)
		}
	}
	hasUA := false
	for k := range req.Headers {
		if strings.EqualFold(k, "User-Agent") {
			hasUA = true
		}
	}
	if !hasUA {
		args = append(args, "--user-agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
	}
	for k, v := range req.Headers {
		// Cookies are better sourced from the browser profile; a raw Cookie
		// header is scoped to one host and breaks on cross-domain redirects.
		if strings.EqualFold(k, "Cookie") && withCookies {
			continue
		}
		args = append(args, "--add-header", fmt.Sprintf("%s:%s", k, v))
	}
	return append(args, "--", req.URL)
}

// runOnce runs a single yt-dlp invocation, streaming progress. It returns the
// final file path (from --print after_move), the last known total size, the
// captured stderr and the process error.
func runOnce(ctx context.Context, pub Publisher, id, bin string, args []string, start time.Time) (string, int64, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, "", err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return "", 0, "", err
	}

	var (
		finalPath                 string
		lastDownloaded, lastTotal int64
		// yt-dlp downloads video and audio as separate files; keep a running
		// offset so the progress bar doesn't jump back to 0 for the audio.
		doneBytes, curTotal int64
		lastPublish         time.Time
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if p, ok := strings.CutPrefix(line, finalPathMarker); ok {
			finalPath = strings.TrimSpace(p)
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 5 {
			continue
		}
		status := strings.TrimSpace(parts[0])
		downloaded := parseInt64(parts[1])
		total := parseInt64(parts[2])
		if total <= 0 && len(parts) >= 6 {
			total = int64(parseFloat64(parts[5]))
		}
		speed := parseFloat64(parts[3])

		if total > 0 && total != curTotal && downloaded < lastDownloaded-doneBytes {
			// A new file started (e.g. the audio stream after the video).
			doneBytes += curTotal
		}
		if total > 0 {
			curTotal = total
		}
		lastDownloaded = doneBytes + downloaded
		if total > 0 {
			lastTotal = doneBytes + total
		}
		if status == "finished" || time.Since(lastPublish) > 250*time.Millisecond {
			lastPublish = time.Now()
			_ = pub.Publish(types.DownloadEvent{Type: types.EventProgress,
				DownloadID:  id,
				Downloaded:  lastDownloaded,
				Total:       lastTotal,
				Speed:       speed,
				Elapsed:     time.Since(start),
				Connections: 1,
			})
		}
	}
	err = cmd.Wait()
	return finalPath, lastTotal, strings.TrimSpace(stderrBuf.String()), err
}

func lastMeaningfulLine(stderr string, fallback error) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "WARNING:") {
			return l
		}
	}
	return fallback.Error()
}

func looksLikeCookieError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "cookie") && (strings.Contains(s, "could not") ||
		strings.Contains(s, "failed") || strings.Contains(s, "unable") ||
		strings.Contains(s, "not find") || strings.Contains(s, "keyring") || strings.Contains(s, "decrypt"))
}

// cookieBrowser returns the yt-dlp --cookies-from-browser name to use for a
// login-gated platform URL, or "" when none applies or no browser profile is
// installed. FLUXGET_COOKIES_BROWSER overrides detection ("none" disables).
func cookieBrowser(rawURL string) string {
	u := strings.ToLower(rawURL)
	needs := false
	for _, h := range cookiePlatforms {
		if strings.Contains(u, h) {
			needs = true
			break
		}
	}
	if !needs {
		return ""
	}
	if env := strings.TrimSpace(os.Getenv("FLUXGET_COOKIES_BROWSER")); env != "" {
		if strings.EqualFold(env, "none") {
			return ""
		}
		return env
	}
	return detectBrowser()
}

var (
	browserOnce     sync.Once
	detectedBrowser string
)

// detectBrowser finds the first browser with a profile on disk, in the order
// users most commonly run the FluxGet extension in.
func detectBrowser() string {
	browserOnce.Do(func() {
		home, _ := os.UserHomeDir()
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		candidates := []struct{ name, path string }{
			{"chrome", filepath.Join(cfg, "google-chrome")},
			{"edge", filepath.Join(cfg, "microsoft-edge")},
			{"brave", filepath.Join(cfg, "BraveSoftware", "Brave-Browser")},
			{"chromium", filepath.Join(cfg, "chromium")},
			{"vivaldi", filepath.Join(cfg, "vivaldi")},
			{"opera", filepath.Join(cfg, "opera")},
			{"firefox", filepath.Join(home, ".mozilla", "firefox")},
			{"firefox", filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox")},
			// macOS / Windows profile roots
			{"chrome", filepath.Join(home, "Library", "Application Support", "Google", "Chrome")},
			{"edge", filepath.Join(home, "Library", "Application Support", "Microsoft Edge")},
			{"firefox", filepath.Join(home, "Library", "Application Support", "Firefox")},
			{"chrome", filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "User Data")},
			{"edge", filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Edge", "User Data")},
		}
		for _, c := range candidates {
			if fi, err := os.Stat(c.path); err == nil && fi.IsDir() {
				detectedBrowser = c.name
				return
			}
		}
	})
	return detectedBrowser
}

// jsRuntime returns the --js-runtimes value yt-dlp needs to solve YouTube's
// signature/n challenges. Deno is yt-dlp's default, so it needs no flag.
func jsRuntime() string {
	if _, err := exec.LookPath("deno"); err == nil {
		return ""
	}
	for _, rt := range []string{"node", "bun", "quickjs"} {
		if _, err := exec.LookPath(rt); err == nil {
			return rt
		}
	}
	return ""
}

func sanitizeTitle(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "", "\"", "'", "<", "", ">", "", "|", "-", "%", "%%")
	s = strings.TrimSpace(r.Replace(s))
	// Leave room for " [id].ext"; truncate on a rune boundary by bytes.
	for len(s) > 180 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	if s == "" {
		return "video"
	}
	return s
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
