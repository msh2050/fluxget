package stream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msh2050/fluxget/internal/engine/events"
	"github.com/google/uuid"
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
	debugLog = log.New(f, "", log.LstdFlags)
	debugLog.Printf("=== FluxGet started ===")
	go cleanupOldDebugDirs()
}

// debugDir returns the path used to preserve failed download segments for 24h.
func debugDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", ".fluxget-debug")
}

// keepTempForDebug copies a temp dir to the debug directory so segments can be
// inspected. Uses copy+delete instead of os.Rename to handle cross-filesystem
// moves (e.g. /tmp on tmpfs → ~/Downloads on ext4).
// The dir is auto-deleted after 24h by cleanupOldDebugDirs.
func keepTempForDebug(tmp, id, reason string) {
	dest := filepath.Join(debugDir(), fmt.Sprintf("%s-%s-%s",
		time.Now().Format("20060102-150405"), id[:8], reason))
	_ = os.MkdirAll(debugDir(), 0o755)
	if err := copyDir(tmp, dest); err != nil {
		debugLog.Printf("debug-keep: failed to copy segments: %v", err)
		return
	}
	_ = os.RemoveAll(tmp)
	debugLog.Printf("debug-keep: segments saved → %s (auto-delete in 24h)", dest)
}

// copyDir recursively copies src directory to dst.
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

// copyFile copies a single file from src to dst with the given permissions.
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
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// cleanupOldDebugDirs removes debug segment dirs older than 24h.
func cleanupOldDebugDirs() {
	dir := debugDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			debugLog.Printf("debug-keep: removed old debug dir %s", e.Name())
		}
	}
}

const (
	segWorkers    = 4                    // conservative — Google Drive and similar CDNs rate-limit hard
	maxRetries    = 12                   // more retries for flaky/rate-limited CDNs
	retryDelay    = 500 * time.Millisecond
	maxRetryDelay = 60 * time.Second
)

// errRateLimit is returned when a CDN responds with HTTP 429.
// It carries the suggested wait duration so the worker can back off.
type errRateLimit struct{ wait time.Duration }

func (e errRateLimit) Error() string { return fmt.Sprintf("429 rate limited (retry in %v)", e.wait) }

// Publisher is the interface stream.Downloader uses to emit SSE events.
// Satisfied by core.DownloadService.
type Publisher interface {
	Publish(msg interface{}) error
}

// Format is one quality option sent by the browser extension.
// Matches the shape produced by document.js from ytInitialPlayerResponse.
type Format struct {
	Itag     int    `json:"itag"`
	MimeType string `json:"mimeType"`
	Quality  string `json:"quality"`
	Bitrate  int    `json:"bitrate"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	URL      string `json:"url"`
}

// Request describes a stream download.
type Request struct {
	URL     string
	Title   string
	DestDir string
	Headers map[string]string
	Formats []Format // optional: YouTube adaptive formats from the extension
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
	_ = os.MkdirAll(destDir, 0o755)

	title := req.Title
	if title == "" {
		title = filenameFromURL(req.URL)
	}

	_ = pub.Publish(events.DownloadQueuedMsg{
		DownloadID: id,
		Filename:   title,
		URL:        req.URL,
		DestPath:   destDir,
	})

	go func() {
		if err := run(ctx, pub, id, title, destDir, req); err != nil {
			_ = pub.Publish(events.DownloadErrorMsg{
				DownloadID: id,
				Filename:   title,
				DestPath:   destDir,
				Err:        err,
			})
		}
	}()

	return id, nil
}

// ── Main dispatch ─────────────────────────────────────────────────────────────

func run(ctx context.Context, pub Publisher, id, title, destDir string, req Request) error {
	start := time.Now()

	_ = pub.Publish(events.DownloadStartedMsg{
		DownloadID: id,
		URL:        req.URL,
		Filename:   title,
		DestPath:   destDir,
	})

	u := strings.ToLower(req.URL)

	switch {
	case len(req.Formats) > 0:
		// Browser extension provided YouTube adaptive format URLs directly.
		return runAdaptive(ctx, pub, id, title, destDir, req, start)

	case strings.Contains(u, ".m3u8") || strings.Contains(u, "mpegurl"):
		return runHLS(ctx, pub, id, title, destDir, req, start)

	case strings.Contains(u, ".mpd") || strings.Contains(u, "dash+xml"):
		return runDASH(ctx, pub, id, title, destDir, req, start)

	default:
		// Direct video file — use concurrent HTTP downloader via ffmpeg copy.
		return runDirect(ctx, pub, id, title, destDir, req, start)
	}
}

// ── YouTube adaptive (direct CDN URLs from ytInitialPlayerResponse) ───────────

func runAdaptive(ctx context.Context, pub Publisher, id, title, destDir string, req Request, start time.Time) error {
	// Find best video and audio streams
	var bestVideo, bestAudio *Format
	for i := range req.Formats {
		f := &req.Formats[i]
		if f.URL == "" {
			continue
		}
		mime := strings.ToLower(f.MimeType)
		if strings.HasPrefix(mime, "video/") {
			if bestVideo == nil || f.Bitrate > bestVideo.Bitrate {
				bestVideo = f
			}
		} else if strings.HasPrefix(mime, "audio/") {
			if bestAudio == nil || f.Bitrate > bestAudio.Bitrate {
				bestAudio = f
			}
		}
	}

	if bestVideo == nil && bestAudio == nil {
		return fmt.Errorf("stream: no usable formats in adaptive list")
	}

	tmp, err := os.MkdirTemp("", "fluxget-*")
	if err != nil {
		return fmt.Errorf("stream: mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	outFile := outputPath(destDir, sanitizeFilename(title), ".mp4")

	var inputs []string

	// Download video track
	if bestVideo != nil {
		vpath := filepath.Join(tmp, "video.mp4")
		if err := downloadFileDirect(ctx, pub, id, bestVideo.URL, vpath, req.Headers, start); err != nil {
			return fmt.Errorf("stream: video download: %w", err)
		}
		inputs = append(inputs, vpath)
	}
	// Download audio track
	if bestAudio != nil {
		apath := filepath.Join(tmp, "audio.mp4")
		if err := downloadFileDirect(ctx, pub, id, bestAudio.URL, apath, req.Headers, start); err != nil {
			return fmt.Errorf("stream: audio download: %w", err)
		}
		inputs = append(inputs, apath)
	}

	return mux(ctx, pub, id, title, inputs, outFile, start)
}

// ── HLS ───────────────────────────────────────────────────────────────────────

func runHLS(ctx context.Context, pub Publisher, id, title, destDir string, req Request, start time.Time) error {
	body, err := fetchText(ctx, req.URL, req.Headers)
	if err != nil {
		return fmt.Errorf("stream: fetch HLS manifest: %w", err)
	}

	// Try master playlist first
	variants, err := ParseHLSMaster(body, req.URL)
	if err != nil {
		return err
	}

	mediaURL := req.URL
	if len(variants) > 0 {
		best := HLSBestVariant(variants)
		mediaURL = best.URL
		debugLog.Printf("[%s] HLS best variant: %dx%d %d kbps → %s", id[:8], best.Width, best.Height, best.Bandwidth/1000, mediaURL)
		body, err = fetchText(ctx, mediaURL, req.Headers)
		if err != nil {
			return fmt.Errorf("stream: fetch HLS media playlist: %w", err)
		}
	}

	segments, err := ParseHLSMedia(body, mediaURL)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return fmt.Errorf("stream: HLS playlist has no segments")
	}

	encrypted := 0
	for _, s := range segments {
		if s.Key != nil {
			encrypted++
		}
	}
	debugLog.Printf("[%s] HLS url=%s segments=%d encrypted=%d", id[:8], req.URL, len(segments), encrypted)

	tmp, err := os.MkdirTemp("", "fluxget-*")
	if err != nil {
		return err
	}
	// keepOnce ensures only one keepTempForDebug call fires per download so the
	// defer fallback does not try to copy a dir that an explicit error path already
	// moved and deleted.
	var kept bool
	keepOnce := func(reason string) {
		if !kept {
			kept = true
			keepTempForDebug(tmp, id, reason)
		}
	}
	defer func() { keepOnce("hls") }()

	segFiles, err := downloadAndDecryptSegments(ctx, pub, id, segments, req.Headers, tmp, start)
	if err != nil {
		debugLog.Printf("[%s] segment download error: %v", id[:8], err)
		keepOnce("seg-error")
		return err
	}

	var totalBytes int64
	for _, sf := range segFiles {
		if fi, e := os.Stat(sf); e == nil {
			totalBytes += fi.Size()
		}
	}
	debugLog.Printf("[%s] segments downloaded: %d files, total=%s", id[:8], len(segFiles), formatBytes(totalBytes))

	if len(segFiles) > 0 {
		if err := validateVideoSegment(segFiles[0]); err != nil {
			keepOnce("invalid-seg")
			return err
		}
	}

	outFile := outputPath(destDir, sanitizeFilename(title), ".mp4")
	debugLog.Printf("[%s] muxing %d segs → %s", id[:8], len(segFiles), outFile)
	if err := muxConcat(ctx, pub, id, title, segFiles, outFile, start); err != nil {
		if fi, statErr := os.Stat(outFile); statErr != nil || fi.Size() == 0 {
			_ = os.Remove(outFile)
		}
		keepOnce("mux-error")
		return err
	}
	if fi, e := os.Stat(outFile); e == nil {
		debugLog.Printf("[%s] output OK: %s (%s)", id[:8], outFile, formatBytes(fi.Size()))
	}
	return nil
}

// ── DASH ──────────────────────────────────────────────────────────────────────

func runDASH(ctx context.Context, pub Publisher, id, title, destDir string, req Request, start time.Time) error {
	body, err := fetchText(ctx, req.URL, req.Headers)
	if err != nil {
		return fmt.Errorf("stream: fetch DASH manifest: %w", err)
	}

	streams, err := ParseMPD(body, req.URL)
	if err != nil {
		return err
	}

	video := DASHBestVideo(streams)
	audio := DASHBestAudio(streams)

	if video == nil && audio == nil {
		return fmt.Errorf("stream: DASH manifest has no usable streams")
	}

	tmp, err := os.MkdirTemp("", "fluxget-*")
	if err != nil {
		return err
	}
	var kept bool
	keepOnce := func(reason string) {
		if !kept {
			kept = true
			keepTempForDebug(tmp, id, reason)
		}
	}
	defer func() { keepOnce("dash") }()

	var inputs []string

	if video != nil {
		allURLs := prependInit(video.InitURL, video.SegURLs)
		segFiles, err := downloadSegments(ctx, pub, id, allURLs, req.Headers, tmp, start)
		if err != nil {
			keepOnce("seg-error")
			return fmt.Errorf("stream: DASH video segments: %w", err)
		}
		if len(segFiles) > 0 {
			if err := validateVideoSegment(segFiles[0]); err != nil {
				keepOnce("invalid-seg")
				return err
			}
		}
		vconcat := filepath.Join(tmp, "video.mp4")
		if err := catFiles(segFiles, vconcat); err != nil {
			return err
		}
		inputs = append(inputs, vconcat)
	}

	if audio != nil {
		allURLs := prependInit(audio.InitURL, audio.SegURLs)
		segFiles, err := downloadSegments(ctx, pub, id, allURLs, req.Headers, tmp+"_a", start)
		if err != nil {
			return fmt.Errorf("stream: DASH audio segments: %w", err)
		}
		_ = os.MkdirAll(tmp+"_a", 0o755)
		aconcat := filepath.Join(tmp, "audio.mp4")
		if err := catFiles(segFiles, aconcat); err != nil {
			return err
		}
		inputs = append(inputs, aconcat)
	}

	outFile := outputPath(destDir, sanitizeFilename(title), ".mp4")
	debugLog.Printf("[%s] muxing → %s", id[:8], outFile)
	if err := mux(ctx, pub, id, title, inputs, outFile, start); err != nil {
		if fi, statErr := os.Stat(outFile); statErr != nil || fi.Size() == 0 {
			_ = os.Remove(outFile)
		}
		return err
	}
	if fi, e := os.Stat(outFile); e == nil {
		debugLog.Printf("[%s] output OK: %s (%s)", id[:8], outFile, formatBytes(fi.Size()))
	}
	return nil
}

// ── Direct file download ──────────────────────────────────────────────────────

func runDirect(ctx context.Context, pub Publisher, id, title, destDir string, req Request, start time.Time) error {
	debugLog.Printf("[%s] direct url=%s headers=%v", id[:8], req.URL, req.Headers)
	ext := extFromURL(req.URL)
	if ext == "" {
		ext = ".mp4"
	}
	outFile := outputPath(destDir, sanitizeFilename(title), ext)
	if err := downloadFileDirect(ctx, pub, id, req.URL, outFile, req.Headers, start); err != nil {
		return err
	}
	elapsed := time.Since(start)
	fi, _ := os.Stat(outFile)
	total := int64(0)
	if fi != nil {
		total = fi.Size()
	}
	avgSpeed := 0.0
	if elapsed.Seconds() > 0 && total > 0 {
		avgSpeed = float64(total) / elapsed.Seconds()
	}
	_ = pub.Publish(events.DownloadCompleteMsg{
		DownloadID: id,
		Filename:   filepath.Base(outFile),
		Elapsed:    elapsed,
		Total:      total,
		AvgSpeed:   avgSpeed,
	})
	return nil
}

// ── Segment download worker pool ──────────────────────────────────────────────

// downloadAndDecryptSegments downloads HLS segments and decrypts AES-128 encrypted ones.
func downloadAndDecryptSegments(ctx context.Context, pub Publisher, id string, segments []HLSSegment, headers map[string]string, tmpDir string, start time.Time) ([]string, error) {
	// Check if any segment is encrypted
	hasEncryption := false
	for _, s := range segments {
		if s.Key != nil {
			hasEncryption = true
			break
		}
	}

	// No encryption — use fast parallel downloader directly
	if !hasEncryption {
		return downloadSegments(ctx, pub, id, segments2urls(segments), headers, tmpDir, start)
	}

	// Fetch all unique AES keys upfront (keyed by URI)
	aesKeys := make(map[string][]byte)
	client := &http.Client{Timeout: 30 * time.Second}
	for _, seg := range segments {
		if seg.Key == nil || seg.Key.URI == "" {
			continue
		}
		if _, ok := aesKeys[seg.Key.URI]; ok {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, seg.Key.URI, nil)
		if err != nil {
			return nil, fmt.Errorf("hls: key request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("hls: fetch key %s: %w", seg.Key.URI, err)
		}
		keyBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || len(keyBytes) != 16 {
			return nil, fmt.Errorf("hls: invalid AES key from %s (got %d bytes)", seg.Key.URI, len(keyBytes))
		}
		aesKeys[seg.Key.URI] = keyBytes
	}

	// Download segments in parallel, then decrypt sequentially
	urls := segments2urls(segments)
	rawFiles, err := downloadSegments(ctx, pub, id, urls, headers, tmpDir, start)
	if err != nil {
		return nil, err
	}

	// Decrypt each encrypted segment in-place.
	// PKCS#7 padding only exists on the last segment — intermediate segments
	// are exact AES block multiples. Unpadding every segment corrupts the stream.
	lastIdx := len(segments) - 1
	for i, seg := range segments {
		if seg.Key == nil || i >= len(rawFiles) {
			continue
		}
		keyBytes := aesKeys[seg.Key.URI]
		if keyBytes == nil {
			continue
		}
		block, err := aes.NewCipher(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("hls: AES cipher: %w", err)
		}
		data, err := os.ReadFile(rawFiles[i])
		if err != nil {
			return nil, fmt.Errorf("hls: read segment %d: %w", i, err)
		}
		if len(data)%aes.BlockSize != 0 {
			return nil, fmt.Errorf("hls: segment %d size %d not a multiple of AES block size", i, len(data))
		}
		mode := cipher.NewCBCDecrypter(block, seg.Key.IV)
		mode.CryptBlocks(data, data)
		if i == lastIdx {
			data = pkcs7Unpad(data)
		}
		// Strip any junk before the first MPEG-TS sync byte (0x47)
		data = syncByteAlign(data)
		if err := os.WriteFile(rawFiles[i], data, 0o600); err != nil {
			return nil, fmt.Errorf("hls: write decrypted segment %d: %w", i, err)
		}
	}

	return rawFiles, nil
}

// syncByteAlign strips any junk bytes before the first MPEG-TS sync byte (0x47).
// Some CDNs prepend garbage to encrypted segments; ffmpeg chokes on it.
func syncByteAlign(data []byte) []byte {
	for i, b := range data {
		if b == 0x47 {
			return data[i:]
		}
	}
	return data
}

// pkcs7Unpad removes PKCS#7 padding.
func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	// Verify all padding bytes are correct
	if !bytes.Equal(data[len(data)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return data
	}
	return data[:len(data)-pad]
}

func downloadSegments(ctx context.Context, pub Publisher, id string, urls []string, headers map[string]string, tmpDir string, start time.Time) ([]string, error) {
	_ = os.MkdirAll(tmpDir, 0o755)
	files := make([]string, len(urls))
	for i := range files {
		files[i] = filepath.Join(tmpDir, fmt.Sprintf("seg%07d", i))
	}

	total := int64(len(urls))
	var done int64
	var mu sync.Mutex
	var downloadedBytes int64

	// Derive a cancellable context so we can abort all workers when segment 0
	// fails validation (e.g. CDN returns PNG tracking pixels instead of video).
	dCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type work struct {
		idx int
		url string
	}

	jobs := make(chan work, len(urls))
	for i, u := range urls {
		jobs <- work{i, u}
	}
	close(jobs)

	// Use Chrome-fingerprinted TLS for all segment fetches. Cloudflare Bot Management
	// ties __cf_bm to the browser's JA3/JA4 fingerprint; Go's TLS ClientHello is
	// identifiably different and causes 403s even with valid cookies.
	segClient := newChromeClient(120 * time.Second)
	// Strip cookies that are bound to Chrome's TLS session fingerprint. Replaying
	// __cf_bm / __cf_clearance from a browser session through Go's (now Chrome-disguised)
	// TLS does work, but belt-and-suspenders: scrub them so we always start fresh.
	segHeaders := stripCloudflareCookies(headers)

	errs := make(chan error, segWorkers)
	var wg sync.WaitGroup
	for w := 0; w < segWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := segClient
			backoff := retryDelay // per-worker backoff, grows on 429
			for job := range jobs {
				if dCtx.Err() != nil {
					return
				}
				var lastErr error
				for attempt := 0; attempt < maxRetries; attempt++ {
					n, err := downloadSegment(dCtx, client, job.url, segHeaders, files[job.idx])
					if err == nil {
						mu.Lock()
						done++
						downloadedBytes += n
						d := done
						db := downloadedBytes
						if d == 1 || d%100 == 0 {
							elapsed := time.Since(start).Seconds()
							speed := float64(db) / max1(elapsed)
							estimatedTotal := db * total / max64(d, 1)
							eta := int64(0)
							if speed > 0 {
								eta = int64(float64(estimatedTotal-db) / speed)
							}
							debugLog.Printf("seg progress: %d/%d segs (%.0f%%) | %s / ~%s | %.1f MB/s | ETA ~%ds",
								d, total, float64(d)/float64(total)*100,
								formatBytes(db), formatBytes(estimatedTotal),
								speed/1e6, eta)
						}
						mu.Unlock()
						// Validate the first segment immediately so we can abort
						// early if the CDN is returning error pages (PNG/HTML) instead
						// of real video data.
						if job.idx == 0 {
							if verr := validateVideoSegment(files[0]); verr != nil {
								debugLog.Printf("[%s] seg0 invalid — aborting all workers: %v", id[:8], verr)
								cancel()
								errs <- verr
								return
							}
						}
						_ = pub.Publish(events.ProgressMsg{
							DownloadID:        id,
							Downloaded:        db,
							Total:             db * total / max64(d, 1),
							Speed:             float64(db) / max1(time.Since(start).Seconds()),
							Elapsed:           time.Since(start),
							ActiveConnections: segWorkers,
						})
						backoff = retryDelay // reset on success
						lastErr = nil
						break
					}

					// 429: honour the CDN's Retry-After and back off exponentially
					if e, ok := err.(errRateLimit); ok {
						wait := e.wait
						if backoff > wait {
							wait = backoff
						}
						debugLog.Printf("[%s] 429 on seg %d — waiting %v (attempt %d)", id[:8], job.idx, wait, attempt+1)
						select {
						case <-dCtx.Done():
							return
						case <-time.After(wait):
						}
						backoff *= 2
						if backoff > maxRetryDelay {
							backoff = maxRetryDelay
						}
						continue // retry without counting as a normal failure
					}

					lastErr = err
					if attempt < maxRetries-1 {
						select {
						case <-dCtx.Done():
							return
						case <-time.After(retryDelay):
						}
					}
				}
				if lastErr != nil {
					// 404 at end of open-ended DASH stream — stop gracefully
					if strings.Contains(lastErr.Error(), "404") {
						mu.Lock()
						done = total
						mu.Unlock()
						return
					}
					errs <- fmt.Errorf("segment %d: %w", job.idx, lastErr)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Trim trailing missing files (for open-ended DASH)
	end := len(files)
	for end > 0 {
		if _, err := os.Stat(files[end-1]); err == nil {
			break
		}
		end--
	}
	return files[:end], nil
}

func downloadSegment(ctx context.Context, client *http.Client, u string, headers map[string]string, dest string) (int64, error) {
	// Skip YouTube CDN adaptive fragments — mirrors IDMNetMon.dll segment skip logic
	// (decompiled lines 54504-54519):
	//   M4S ext + (/avf/ or /sep/ in path) + filename starts "segment-" → skip
	//   MP4 ext + (/avf/ or /parcel/ in path) → skip
	if isYouTubeCDNFragment(u) {
		return 0, nil
	}

	// VK / mycdn CDN byte-range URLs: same base URL + "&bytes=start-end"
	// IDMNetMon deduplicates these against the base segment URL (lines 52386-52393).
	// We store the resolved URL as-is; the byte-range is already encoded in u.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		debugLog.Printf("seg HTTP %d CT=%s url=%s", resp.StatusCode, resp.Header.Get("Content-Type"), u)
	}
	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("404: %s", u)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := 5 * time.Second
		var n int
		if _, err2 := fmt.Sscanf(resp.Header.Get("Retry-After"), "%d", &n); err2 == nil && n > 0 {
			wait = time.Duration(n) * time.Second
		}
		return 0, errRateLimit{wait: wait}
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, u)
	}
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, resp.Body)
	return n, err
}

// isYouTubeCDNFragment detects YouTube adaptive fragmented media segments that
// should be skipped during segment enumeration. Mirrors IDMNetMon.dll lines 54504-54519.
//
//   M4S extension + (/avf/ or /sep/ in path) + filename begins "segment-" → adaptive fragment
//   MP4 extension + (/avf/ or /parcel/ in path)                           → fragmented MP4 chunk
//   /api/manifest/init_segment                                             → DASH init segment URL
func isYouTubeCDNFragment(u string) bool {
	lower := strings.ToLower(u)
	path := strings.SplitN(lower, "?", 2)[0]
	file := path[strings.LastIndexByte(path, '/')+1:]

	if strings.HasSuffix(path, ".m4s") {
		if (strings.Contains(path, "/avf/") || strings.Contains(path, "/sep/")) &&
			strings.HasPrefix(file, "segment-") {
			return true
		}
	}
	if strings.HasSuffix(path, ".mp4") {
		if strings.Contains(path, "/avf/") || strings.Contains(path, "/parcel/") {
			return true
		}
	}
	if strings.Contains(lower, "/api/manifest/init_segment") {
		return true
	}
	return false
}

// ── Direct file download (single connection) ──────────────────────────────────

func downloadFileDirect(ctx context.Context, pub Publisher, id, u, dest string, headers map[string]string, start time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, u)
	}

	total := resp.ContentLength

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 256*1024)
	var downloaded int64
	lastReport := time.Now()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			downloaded += int64(n)
			if time.Since(lastReport) > 250*time.Millisecond {
				_ = pub.Publish(events.ProgressMsg{
					DownloadID:        id,
					Downloaded:        downloaded,
					Total:             total,
					Speed:             float64(downloaded) / max1(time.Since(start).Seconds()),
					Elapsed:           time.Since(start),
					ActiveConnections: 1,
				})
				lastReport = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ── ffmpeg muxing ─────────────────────────────────────────────────────────────

// mux combines one or two input files into a final mp4 using ffmpeg -c copy.
func mux(ctx context.Context, pub Publisher, id, title string, inputs []string, outFile string, start time.Time) error {
	if len(inputs) == 0 {
		return fmt.Errorf("stream: mux: no input files")
	}

	ffmpeg, err := findFFmpeg()
	if err != nil {
		// No ffmpeg — if single input, just rename it
		if len(inputs) == 1 {
			return os.Rename(inputs[0], outFile)
		}
		return fmt.Errorf("stream: ffmpeg not found — install ffmpeg to mux video+audio: %w", err)
	}

	args := []string{"-y"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	args = append(args, "-c", "copy", outFile)

	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		debugLog.Printf("[%s] ffmpeg mux error: %v\n%s", id[:8], err, out)
		return fmt.Errorf("stream: ffmpeg: %w\n%s", err, out)
	}
	debugLog.Printf("[%s] ffmpeg mux OK → %s", id[:8], outFile)

	return publishComplete(pub, id, title, outFile, start)
}

// muxConcat concatenates segment files into one output using ffmpeg concat demuxer.
func muxConcat(ctx context.Context, pub Publisher, id, title string, segFiles []string, outFile string, start time.Time) error {
	if len(segFiles) == 0 {
		return fmt.Errorf("stream: no segments downloaded")
	}

	ffmpeg, err := findFFmpeg()
	if err != nil {
		// Fallback: raw concatenation (works for MPEG-TS)
		if err2 := catFiles(segFiles, outFile); err2 != nil {
			return err2
		}
		return publishComplete(pub, id, title, outFile, start)
	}

	// Write ffmpeg concat list
	listFile := outFile + ".list"
	var sb strings.Builder
	for _, f := range segFiles {
		sb.WriteString("file '")
		sb.WriteString(f)
		sb.WriteString("'\n")
	}
	if err := os.WriteFile(listFile, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(listFile) }()

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-y", "-f", "concat", "-safe", "0",
		"-i", listFile,
		"-c", "copy", outFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		debugLog.Printf("[%s] ffmpeg concat error: %v\n%s", id[:8], err, out)
		return fmt.Errorf("stream: ffmpeg concat: %w\n%s", err, out)
	}
	if fi, e := os.Stat(outFile); e == nil {
		debugLog.Printf("[%s] ffmpeg concat OK → %s (%s)", id[:8], outFile, formatBytes(fi.Size()))
	} else {
		debugLog.Printf("[%s] ffmpeg concat OK but output missing: %v", id[:8], e)
	}

	return publishComplete(pub, id, title, outFile, start)
}

// catFiles concatenates files byte-for-byte (MPEG-TS safe).
func catFiles(paths []string, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func fetchText(ctx context.Context, u string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	// Set browser-like defaults before applying caller headers so CDNs like
	// turbosplayer.com don't detect Go-http-client and serve a fallback playlist.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cookie := req.Header.Get("Cookie")
	if cookie == "" {
		cookie = "(none)"
	} else if len(cookie) > 120 {
		cookie = cookie[:120] + "…"
	}
	debugLog.Printf("fetchText %s | Cookie: %s | Referer: %s", u, cookie, req.Header.Get("Referer"))
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, u)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func findFFmpeg() (string, error) {
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("ffmpeg not in PATH")
}

func publishComplete(pub Publisher, id, title, outFile string, start time.Time) error {
	elapsed := time.Since(start)
	fi, _ := os.Stat(outFile)
	total := int64(0)
	if fi != nil {
		total = fi.Size()
	}
	avgSpeed := 0.0
	if elapsed.Seconds() > 0 && total > 0 {
		avgSpeed = float64(total) / elapsed.Seconds()
	}
	return pub.Publish(events.DownloadCompleteMsg{
		DownloadID: id,
		Filename:   filepath.Base(outFile),
		Elapsed:    elapsed,
		Total:      total,
		AvgSpeed:   avgSpeed,
	})
}

func prependInit(initURL string, segs []string) []string {
	if initURL == "" {
		return segs
	}
	return append([]string{initURL}, segs...)
}

func segments2urls(segs []HLSSegment) []string {
	urls := make([]string, len(segs))
	for i, s := range segs {
		urls[i] = s.URL
	}
	return urls
}

func outputPath(dir, base, ext string) string {
	name := base + ext
	path := filepath.Join(dir, name)
	// Avoid overwriting existing files
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

func sanitizeFilename(s string) string {
	if s == "" {
		return "video"
	}
	// Remove path separators and other unsafe chars
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "", "\"", "'", "<", "", ">", "", "|", "-",
	)
	s = replacer.Replace(s)
	// Truncate to avoid OS limits
	runes := []rune(s)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return strings.TrimSpace(string(runes))
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

// validateVideoSegment reads the first bytes of a downloaded segment and returns
// an error if the content is clearly not video (e.g. a PNG/JPEG tracking pixel
// returned by a CDN when the request lacks authentication).
func validateVideoSegment(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	hdr := make([]byte, 8)
	n, _ := f.Read(hdr)
	if n < 4 {
		return fmt.Errorf("stream: segment too small (%d bytes) — not valid video", n)
	}

	// MPEG-TS sync byte
	if hdr[0] == 0x47 {
		debugLog.Printf("validateVideoSegment: MPEG-TS OK (%s)", path)
		return nil
	}
	// fMP4 / MP4 box (4-byte size + 4-byte type)
	if n >= 8 {
		switch string(hdr[4:8]) {
		case "ftyp", "moof", "styp", "mdat", "moov":
			debugLog.Printf("validateVideoSegment: fMP4 OK box=%s (%s)", string(hdr[4:8]), path)
			return nil
		}
	}
	// PNG magic: 89 50 4E 47
	if hdr[0] == 0x89 && hdr[1] == 0x50 && hdr[2] == 0x4E && hdr[3] == 0x47 {
		return fmt.Errorf("stream: CDN returned a PNG image instead of video — the stream URL requires authentication (missing cookies or signed token)")
	}
	// JPEG magic: FF D8
	if hdr[0] == 0xFF && hdr[1] == 0xD8 {
		return fmt.Errorf("stream: CDN returned a JPEG image instead of video — the stream URL requires authentication")
	}
	// HTML error page
	if strings.HasPrefix(string(hdr[:n]), "<!") || strings.HasPrefix(string(hdr[:n]), "<ht") {
		return fmt.Errorf("stream: CDN returned an HTML error page instead of video — the stream URL requires authentication")
	}
	// Unknown but not obviously video — warn but allow (could be a codec we don't recognise)
	debugLog.Printf("validateVideoSegment: unknown segment header %x — continuing", hdr[:n])
	return nil
}

func extFromURL(u string) string {
	parts := strings.Split(u, "?")
	path := parts[0]
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return ""
	}
	ext := strings.ToLower(path[dot:])
	// Only return known video/audio extensions
	switch ext {
	case ".mp4", ".mkv", ".webm", ".avi", ".mov", ".flv", ".mp3", ".aac", ".ogg", ".opus", ".flac", ".wav":
		return ext
	}
	return ""
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

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func max1(f float64) float64 {
	if f < 1 {
		return 1
	}
	return f
}

// stripCloudflareCookies returns a copy of headers with Cloudflare session-fingerprint
// cookies (__cf_bm, __cf_clearance, _cfuvid) removed from the Cookie value.
// These cookies are cryptographically tied to Chrome's TLS session fingerprint;
// replaying them from any other client triggers an immediate 403 from Cloudflare WAF.
// With the Chrome-fingerprinted transport we start fresh sessions and Cloudflare
// issues new cookies — there is no need to forward the browser-captured ones.
func stripCloudflareCookies(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, "cookie") {
			var kept []string
			for _, part := range strings.Split(v, ";") {
				part = strings.TrimSpace(part)
				name := part
				if i := strings.IndexByte(part, '='); i >= 0 {
					name = strings.TrimSpace(part[:i])
				}
				if name == "__cf_bm" || name == "__cf_clearance" || name == "_cfuvid" {
					continue
				}
				kept = append(kept, part)
			}
			if len(kept) == 0 {
				continue
			}
			out[k] = strings.Join(kept, "; ")
			continue
		}
		out[k] = v
	}
	return out
}

// hostOf returns the hostname from a URL string, or "" on parse error.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
