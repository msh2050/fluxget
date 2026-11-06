package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/msh2050/fluxget/internal/types"
	"github.com/msh2050/fluxget/internal/ytdlp"
)

// ── Debug log ─────────────────────────────────────────────────────────────────

var debugLog = log.New(io.Discard, "", 0)

func init() {
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, "Downloads", "fluxget-debug.log")
	// Rotate a runaway log instead of growing forever.
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 20<<20 {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	debugLog = log.New(f, "", log.LstdFlags)
	debugLog.Printf("=== FluxGet stream engine started ===")
	go cleanupOldDebugDirs()
}

// ── Tunables (wired from settings in cmd/settings_api.go) ─────────────────────

var segWorkers atomic.Int64

func init() { segWorkers.Store(8) }

// SetSegmentWorkers sets the parallel connection count used for HLS/DASH
// segments and ranged file downloads, clamped to [1, 32].
func SetSegmentWorkers(n int) {
	segWorkers.Store(int64(min(max(n, 1), 32)))
}

func segWorkerCount() int { return int(segWorkers.Load()) }

var wantSubtitles atomic.Bool

func init() { wantSubtitles.Store(true) }

// SetDownloadSubtitles toggles embedding/saving of subtitle renditions.
func SetDownloadSubtitles(on bool) { wantSubtitles.Store(on) }

func subtitlesEnabled() bool { return wantSubtitles.Load() }

// ── Public API ────────────────────────────────────────────────────────────────

// Publisher is the interface stream downloads use to emit lifecycle events.
// Satisfied by service.DownloadService.
type Publisher interface {
	Publish(msg types.DownloadEvent) error
}

// Format is one quality option sent by the browser extension (shape produced
// by document.js from YouTube's player response).
type Format struct {
	Itag          int    `json:"itag"`
	MimeType      string `json:"mimeType"`
	Quality       string `json:"quality"`
	Bitrate       int    `json:"bitrate"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	URL           string `json:"url"`
	ContentLength int64  `json:"contentLength"`
	Progressive   bool   `json:"progressive"` // muxed audio+video
}

func (f Format) isVideo() bool { return strings.HasPrefix(strings.ToLower(f.MimeType), "video/") }
func (f Format) isAudio() bool { return strings.HasPrefix(strings.ToLower(f.MimeType), "audio/") }

// Request describes a stream download.
type Request struct {
	URL     string
	Title   string
	DestDir string
	Headers map[string]string
	Formats []Format // optional: YouTube adaptive formats from the extension

	// MaxHeight caps the selected video quality (0 = best available). Used to
	// honour the quality the user picked in the extension for HLS/DASH masters.
	MaxHeight int
	// AudioOnly downloads just the best audio track.
	AudioOnly bool
	// PageURL is the web page the media came from. When the native engine
	// cannot handle the media (signature-protected YouTube URLs, live streams,
	// expired links…) and yt-dlp is installed, the page is handed to yt-dlp
	// under the same download ID.
	PageURL string

	// SubtitlesOnly downloads just the subtitle tracks (as .srt).
	SubtitlesOnly bool
}

// Start detects the URL type, picks the right download strategy, and runs it
// in a background goroutine. Returns a download ID immediately.
func Start(ctx context.Context, pub Publisher, req Request) (string, error) {
	id := uuid.New().String()

	destDir := req.DestDir
	if destDir == "" {
		home, _ := os.UserHomeDir()
		destDir = filepath.Join(home, "Downloads")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("stream: create destination: %w", err)
	}
	req.DestDir = destDir

	title := req.Title
	if title == "" {
		title = filenameFromURL(req.URL)
	}
	req.Title = title

	_ = pub.Publish(types.DownloadEvent{Type: types.EventQueued,
		DownloadID: id, Filename: title, URL: req.URL, DestPath: destDir})

	dctx, cancel := context.WithCancel(ctx)
	active.Store(id, cancel)

	go func() {
		defer func() {
			active.Delete(id)
			cancel()
		}()
		_ = pub.Publish(types.DownloadEvent{Type: types.EventStarted,
			DownloadID: id, URL: req.URL, Filename: title, DestPath: destDir})

		err := run(dctx, pub, id, req)
		if err != nil && dctx.Err() == nil && shouldFallBackToYtDlp(err, req) {
			debugLog.Printf("[%s] native engine failed (%v) \u2014 falling back to yt-dlp for %s", short(id), err, fallbackURL(req))
			err = ytdlp.Run(ctx, pub, id, ytdlp.Request{
				URL:     fallbackURL(req),
				Title:   req.Title,
				Format:  ytdlpFormatFor(req),
				DestDir: destDir,
				Headers: req.Headers,
			})
		}
		switch {
		case err == nil:
		case dctx.Err() != nil:
			debugLog.Printf("[%s] cancelled", short(id))
		default:
			debugLog.Printf("[%s] failed: %v", short(id), err)
			_ = pub.Publish(types.DownloadEvent{Type: types.EventError,
				DownloadID: id, Filename: title, DestPath: destDir, Err: err})
		}
	}()

	return id, nil
}

// ── Cancellation ──────────────────────────────────────────────────────────────

var active sync.Map // id → context.CancelFunc

// Cancel aborts a running stream download (and any yt-dlp fallback it spawned).
// Returns false if the id is not a running stream download.
func Cancel(id string) bool {
	ok := false
	if v, found := active.LoadAndDelete(id); found {
		v.(context.CancelFunc)()
		ok = true
	}
	if ytdlp.Cancel(id) {
		ok = true
	}
	return ok
}

// IsActive reports whether id is a running stream download.
func IsActive(id string) bool {
	_, ok := active.Load(id)
	return ok
}

// ── Dispatch ──────────────────────────────────────────────────────────────────

func run(ctx context.Context, pub Publisher, id string, req Request) error {
	if req.SubtitlesOnly {
		return runSubtitlesOnly(ctx, pub, id, req)
	}
	u := strings.ToLower(req.URL)
	switch {
	case len(req.Formats) > 0:
		return runAdaptive(ctx, pub, id, req)
	case strings.Contains(u, ".m3u8") || strings.Contains(u, "mpegurl"):
		return runHLS(ctx, pub, id, req)
	case strings.Contains(u, ".mpd") || strings.Contains(u, "dash+xml"):
		return runDASH(ctx, pub, id, req)
	default:
		return runDirect(ctx, pub, id, req)
	}
}

// errNeedsYtDlp is returned by native handlers when the media is known to be
// unhandleable natively (e.g. signature-ciphered YouTube formats).
var errNeedsYtDlp = errors.New("media requires yt-dlp")

func fallbackURL(req Request) string {
	if req.PageURL != "" {
		return req.PageURL
	}
	return req.URL
}

func shouldFallBackToYtDlp(err error, req Request) bool {
	if !ytdlp.Available() || errors.Is(err, ErrDRM) {
		return false
	}
	if errors.Is(err, errNeedsYtDlp) || errors.Is(err, errLiveStream) {
		return true
	}
	// A native failure on a page we know yt-dlp supports is worth a retry
	// (expired signed URLs, 403 from n-parameter throttling, etc.).
	return req.PageURL != "" && ytdlp.NeedsYtDlp(req.PageURL)
}

func ytdlpFormatFor(req Request) string {
	switch {
	case req.AudioOnly:
		return "bestaudio/best"
	case req.MaxHeight > 0:
		h := req.MaxHeight
		return fmt.Sprintf("bestvideo*[height<=%d]+bestaudio/best[height<=%d]/best", h, h)
	}
	return ""
}

// ── Progress tracking ─────────────────────────────────────────────────────────

// progress aggregates bytes across every parallel fetch belonging to one
// download and publishes throttled ProgressMsg events.
type progress struct {
	pub         Publisher
	id          string
	start       time.Time
	done        atomic.Int64
	total       atomic.Int64 // exact or estimated total bytes (0 = unknown)
	connections atomic.Int64
	mu          sync.Mutex
	last        time.Time
	// segment-based estimation
	segTotal, segDone atomic.Int64
}

func newProgress(pub Publisher, id string, start time.Time) *progress {
	return &progress{pub: pub, id: id, start: start}
}

func (p *progress) add(n int64) {
	p.done.Add(n)
	p.maybePublish(false)
}

func (p *progress) addTotal(n int64) { p.total.Add(n) }

func (p *progress) segmentDone() {
	p.segDone.Add(1)
	p.maybePublish(false)
}

func (p *progress) estimatedTotal() int64 {
	if t := p.total.Load(); t > 0 {
		return t
	}
	d, st, sd := p.done.Load(), p.segTotal.Load(), p.segDone.Load()
	if st > 0 && sd > 0 {
		return d * st / sd
	}
	return 0
}

func (p *progress) maybePublish(force bool) {
	p.mu.Lock()
	if !force && time.Since(p.last) < 250*time.Millisecond {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	elapsed := time.Since(p.start)
	done := p.done.Load()
	_ = p.pub.Publish(types.DownloadEvent{Type: types.EventProgress,
		DownloadID:  p.id,
		Downloaded:  done,
		Total:       p.estimatedTotal(),
		Speed:       float64(done) / max(elapsed.Seconds(), 1),
		Elapsed:     elapsed,
		Connections: int(max(p.connections.Load(), 1)),
	})
}

// ── Completion ────────────────────────────────────────────────────────────────

// publishComplete re-announces the download with its real output path (so the
// history entry, open-file and open-folder point at the file rather than the
// destination folder) and then marks it complete.
func publishComplete(pub Publisher, id, url, outFile string, start time.Time) error {
	elapsed := time.Since(start)
	var total int64
	if fi, err := os.Stat(outFile); err == nil {
		total = fi.Size()
	}
	avg := 0.0
	if elapsed.Seconds() > 0 {
		avg = float64(total) / elapsed.Seconds()
	}
	name := filepath.Base(outFile)
	_ = pub.Publish(types.DownloadEvent{Type: types.EventStarted,
		DownloadID: id, URL: url, Filename: name, DestPath: outFile, Total: total})
	debugLog.Printf("[%s] output OK: %s (%s)", short(id), outFile, formatBytes(total))
	return pub.Publish(types.DownloadEvent{Type: types.EventComplete,
		DownloadID: id, Filename: name, Elapsed: elapsed, Total: total, AvgSpeed: avg})
}

// ── Debug segment retention (failures only) ───────────────────────────────────

func debugDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", ".fluxget-debug")
}

// workDir is a per-download temp directory. On failure its contents are kept
// for 24h under ~/Downloads/.fluxget-debug for inspection; on success (or
// cancellation) it is removed.
type workDir struct {
	path   string
	id     string
	failed string
}

// newWorkDir creates the temp dir inside destDir (hidden) rather than /tmp:
// /tmp is often RAM-backed tmpfs, multi-GB videos would exhaust it, and
// staying on the destination filesystem makes the final move a cheap rename.
func newWorkDir(id, destDir string) (*workDir, error) {
	p, err := os.MkdirTemp(destDir, ".fluxget-"+short(id)+"-")
	if err != nil {
		p, err = os.MkdirTemp("", "fluxget-*")
	}
	if err != nil {
		return nil, fmt.Errorf("stream: mktemp: %w", err)
	}
	return &workDir{path: p, id: id}, nil
}

func (w *workDir) fail(reason string) { w.failed = reason }

func (w *workDir) close(ctx context.Context) {
	if w.failed == "" || ctx.Err() != nil {
		_ = os.RemoveAll(w.path)
		return
	}
	dest := filepath.Join(debugDir(), fmt.Sprintf("%s-%s-%s",
		time.Now().Format("20060102-150405"), short(w.id), w.failed))
	_ = os.MkdirAll(debugDir(), 0o755)
	if err := os.Rename(w.path, dest); err != nil {
		if err := copyDir(w.path, dest); err != nil {
			debugLog.Printf("debug-keep: copy failed: %v", err)
		}
		_ = os.RemoveAll(w.path)
	}
	debugLog.Printf("debug-keep: segments saved \u2192 %s (auto-delete in 24h)", dest)
}

func (w *workDir) join(elem ...string) string {
	return filepath.Join(append([]string{w.path}, elem...)...)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func cleanupOldDebugDirs() {
	entries, err := os.ReadDir(debugDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(debugDir(), e.Name()))
		}
	}
}

// ── Naming helpers ────────────────────────────────────────────────────────────

// outputPath returns a non-existing path "<dir>/<base><ext>", appending " (n)"
// when needed.
func outputPath(dir, base, ext string) string {
	path := filepath.Join(dir, base+ext)
	for i := 1; ; i++ {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

// maxNameBytes keeps "<name> (99).mkv" + sidecar suffixes under the 255-byte
// NAME_MAX of ext4/btrfs even for multi-byte (e.g. Arabic) titles.
const maxNameBytes = 200

// sanitizeFilename turns a page/video title into a safe file base name.
func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '|':
			return '-'
		case '?', '<', '>':
			return -1
		case '"':
			return '\''
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	s = strings.TrimLeft(s, "-.")
	if s == "" {
		return "video"
	}
	return s
}

// stripKnownExt removes a trailing media extension from a title so
// "clip.mp4" doesn't become "clip.mp4.mp4".
func stripKnownExt(s string) string {
	ext := strings.ToLower(filepath.Ext(s))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".m4a", ".mp3", ".ts", ".mov", ".m3u8", ".mpd", ".flv", ".avi", ".aac", ".opus", ".ogg":
		return strings.TrimSuffix(s, filepath.Ext(s))
	}
	return s
}

func titleBase(title string) string { return sanitizeFilename(stripKnownExt(title)) }

func filenameFromURL(u string) string {
	parts := strings.Split(strings.SplitN(u, "?", 2)[0], "/")
	// Skip the scheme ("https:") and host; take the last non-empty path element.
	for i := len(parts) - 1; i >= 3; i-- {
		if parts[i] != "" {
			if dec, err := url.PathUnescape(parts[i]); err == nil {
				return dec
			}
			return parts[i]
		}
	}
	return "video"
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
