package stream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// segJob is one media segment to fetch.
type segJob struct {
	URL   string
	Range ByteRange
	Key   *HLSKey // AES-128 key info, nil when clear
}

// segmentFetcher downloads ordered segment lists in parallel.
type segmentFetcher struct {
	id      string
	client  *http.Client
	headers map[string]string
	prog    *progress
	keys    *keyCache
}

func newSegmentFetcher(id string, prog *progress, headers map[string]string) *segmentFetcher {
	// Chrome-fingerprinted TLS for segment CDNs: Cloudflare Bot Management ties
	// __cf_bm to the browser's JA3/JA4, and Go's ClientHello gets 403s.
	// The browser's Cloudflare cookies are bound to *its* TLS session, so they
	// are dropped and the CDN issues fresh ones.
	h := stripCloudflareCookies(headers)
	return &segmentFetcher{
		id:      id,
		client:  chromeClient(),
		headers: h,
		prog:    prog,
		keys:    &keyCache{m: map[string][]byte{}, headers: h},
	}
}

// fetchAll downloads every job into dir and returns the file paths in order.
// The first segment is validated before the rest are trusted, so a CDN that
// answers with an error page/tracking pixel aborts quickly.
func (sf *segmentFetcher) fetchAll(ctx context.Context, jobs []segJob, dir string) ([]string, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files := make([]string, len(jobs))
	for i := range files {
		files[i] = filepath.Join(dir, fmt.Sprintf("seg%07d", i))
	}
	sf.prog.segTotal.Add(int64(len(jobs)))

	// Segment 0 first, alone: validates auth/cookies before opening N sockets.
	if err := sf.fetchOne(ctx, 0, jobs[0], files[0], true); err != nil {
		return nil, err
	}
	if len(jobs) == 1 {
		return files, nil
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(segWorkerCount(), len(jobs)-1)
	sf.prog.connections.Add(int64(workers))
	defer sf.prog.connections.Add(-int64(workers))

	var next atomic.Int64
	next.Store(1)
	var firstErr error
	var once sync.Once
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) || dctx.Err() != nil {
					return
				}
				if err := sf.fetchOne(dctx, i, jobs[i], files[i], false); err != nil {
					once.Do(func() {
						firstErr = fmt.Errorf("segment %d/%d: %w", i+1, len(jobs), err)
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

func (sf *segmentFetcher) fetchOne(ctx context.Context, idx int, job segJob, dest string, validate bool) error {
	data, err := fetchBytes(ctx, sf.client, job.URL, sf.headers, job.Range)
	if err != nil {
		return err
	}
	if job.Key != nil {
		key, err := sf.keys.get(ctx, sf.client, job.Key.URI)
		if err != nil {
			return err
		}
		if data, err = decryptAES128(data, key, job.Key.IV); err != nil {
			return fmt.Errorf("decrypt: %w", err)
		}
	}
	// Some CDNs glue a fake PNG/JPEG/GIF header in front of each segment to
	// defeat naive downloaders. Strip it transparently, as IDM does.
	if stripped, ok := stripFakeImagePrefix(data); ok {
		data = stripped
	}
	if validate {
		if err := validateMediaBytes(data); err != nil {
			return err
		}
	}
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	sf.prog.add(int64(len(data)))
	sf.prog.segmentDone()
	_ = idx
	return nil
}

// ── AES-128 ───────────────────────────────────────────────────────────────────

type keyCache struct {
	mu      sync.Mutex
	m       map[string][]byte
	headers map[string]string
}

func (kc *keyCache) get(ctx context.Context, client *http.Client, uri string) ([]byte, error) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	if k, ok := kc.m[uri]; ok {
		return k, nil
	}
	if strings.HasPrefix(strings.ToLower(uri), "skd://") {
		return nil, ErrDRM
	}
	raw, err := fetchBytes(ctx, client, uri, kc.headers, ByteRange{})
	if err != nil {
		return nil, fmt.Errorf("fetch AES key: %w", err)
	}
	key := normalizeKey(raw)
	if len(key) != 16 {
		return nil, fmt.Errorf("invalid AES-128 key from %s (%d bytes)", redactURL(uri), len(raw))
	}
	kc.m[uri] = key
	return key, nil
}

// normalizeKey accepts the raw 16-byte key and the hex/base64 text forms a
// few misconfigured servers return.
func normalizeKey(b []byte) []byte {
	if len(b) == 16 {
		return b
	}
	s := strings.TrimSpace(string(b))
	if k, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(s), "0x")); err == nil && len(k) == 16 {
		return k
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 16 {
		return k
	}
	return b
}

// decryptAES128 decrypts one HLS segment (AES-128-CBC). Per RFC 8216 §5.2
// every segment is individually PKCS#7-padded; the padding is removed only
// when it verifies, so a rare unpadded segment is left intact.
func decryptAES128(data, key, iv []byte) ([]byte, error) {
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of %d", len(data), aes.BlockSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("invalid IV length %d", len(iv))
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	out = pkcs7Unpad(out)
	// Junk before the first MPEG-TS packet (seen on some CDNs) — only strip it
	// when the payload really is TS; fMP4 must never be touched.
	if len(out) > 0 && out[0] != 0x47 && !hasMP4Box(out) {
		if start := findMediaStart(out); start > 0 && start < 4096 {
			out = out[start:]
		}
	}
	return out, nil
}

func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	if !bytes.Equal(data[len(data)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return data
	}
	return data[:len(data)-pad]
}

// ── Media sniffing ────────────────────────────────────────────────────────────

func hasMP4Box(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	switch string(b[4:8]) {
	case "ftyp", "styp", "moof", "moov", "sidx", "mdat", "emsg", "free", "prft":
		return true
	}
	return false
}

func isFMP4File(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, 8)
	n, _ := f.Read(hdr)
	return n == 8 && hasMP4Box(hdr)
}

// validateMediaBytes rejects segments that are obviously not media (error
// pages, tracking pixels) so the download fails fast with a useful message.
func validateMediaBytes(b []byte) error {
	if len(b) < 4 {
		return fmt.Errorf("segment too small (%d bytes) \u2014 not valid media", len(b))
	}
	switch {
	case b[0] == 0x47, hasMP4Box(b):
		return nil
	case b[0] == 0xFF && b[1]&0xF6 == 0xF0: // ADTS AAC (audio-only HLS)
		return nil
	case bytes.HasPrefix(b, []byte("ID3")): // timed metadata before AAC/MP3
		return nil
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}): // WebM/Matroska
		return nil
	case bytes.HasPrefix(b, []byte("WEBVTT")):
		return nil
	case looksLikeFakeImageHeader(b):
		return fmt.Errorf("segment is an image with no video payload \u2014 the stream needs authentication (cookies or a signed token)")
	}
	head := strings.ToLower(strings.TrimSpace(string(b[:min(len(b), 256)])))
	if strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html") || strings.HasPrefix(head, "<?xml") || strings.HasPrefix(head, "{") {
		return fmt.Errorf("CDN returned a web page instead of video \u2014 the stream URL expired or requires authentication")
	}
	debugLog.Printf("validateMedia: unknown header %x \u2014 continuing", b[:min(len(b), 8)])
	return nil
}

// stripFakeImagePrefix detects the fake image-header obfuscation used by some
// HLS CDNs (a tiny still image + padding glued before the real TS/fMP4).
func stripFakeImagePrefix(data []byte) ([]byte, bool) {
	if !looksLikeFakeImageHeader(data) {
		return nil, false
	}
	if start := findMediaStart(data); start > 0 {
		debugLog.Printf("stripFakeImagePrefix: stripped %d-byte image/padding prefix (magic %x)", start, data[:4])
		return data[start:], true
	}
	return nil, false
}

func looksLikeFakeImageHeader(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	switch {
	case data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G':
		return true
	case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return true
	case data[0] == 'G' && data[1] == 'I' && data[2] == 'F' && data[3] == '8':
		return true
	case data[0] == 'B' && data[1] == 'M':
		return true
	}
	return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// findMediaStart returns the offset of the first real media payload: an
// MPEG-TS sync byte repeating every 188 bytes, or an MP4 box header.
func findMediaStart(data []byte) int {
	limit := min(len(data), 1<<16)
	for i := 0; i < limit; i++ {
		if data[i] != 0x47 {
			continue
		}
		ok := true
		for k := 1; k <= 3; k++ {
			if p := i + 188*k; p >= len(data) || data[p] != 0x47 {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	for i := 0; i+8 <= limit; i++ {
		if hasMP4Box(data[i:]) {
			size := uint32(data[i])<<24 | uint32(data[i+1])<<16 | uint32(data[i+2])<<8 | uint32(data[i+3])
			if size >= 8 && int64(size) <= int64(len(data)-i) {
				return i
			}
		}
	}
	return -1
}

// stripCloudflareCookies drops Cloudflare session cookies (bound to the
// browser's TLS fingerprint) from the Cookie header.
func stripCloudflareCookies(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, "cookie") {
			var kept []string
			for _, part := range strings.Split(v, ";") {
				part = strings.TrimSpace(part)
				name, _, _ := strings.Cut(part, "=")
				switch strings.TrimSpace(name) {
				case "__cf_bm", "cf_clearance", "__cf_clearance", "_cfuvid", "":
					continue
				}
				kept = append(kept, part)
			}
			if len(kept) > 0 {
				out[k] = strings.Join(kept, "; ")
			}
			continue
		}
		out[k] = v
	}
	return out
}

// catFiles concatenates files byte-for-byte into dest.
func catFiles(paths []string, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	for _, p := range paths {
		in, err := os.Open(p)
		if err != nil {
			_ = out.Close()
			return err
		}
		_, err = io.Copy(out, in)
		_ = in.Close()
		if err != nil {
			_ = out.Close()
			return err
		}
	}
	return out.Close()
}
