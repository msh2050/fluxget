package stream

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

	maxAttempts  = 10
	baseBackoff  = 500 * time.Millisecond
	maxBackoff   = 30 * time.Second
	stallTimeout = 45 * time.Second // abort a body read that makes no progress
)

// ── Clients ───────────────────────────────────────────────────────────────────

// stdClient is the default HTTP client: HTTP/2-capable, proxy-aware, with a
// connection pool big enough for every parallel worker to keep its socket.
var stdClient = sync.OnceValue(func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 64
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DialContext = (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Transport: tr}
})

// chromeClient mimics Chrome's TLS fingerprint (see chrome_transport.go) for
// Cloudflare-fronted CDNs that 403 Go's native ClientHello.
var chromeClient = sync.OnceValue(func() *http.Client { return newChromeClient(0) })

// ── Errors ────────────────────────────────────────────────────────────────────

type httpStatusError struct {
	Code       int
	URL        string
	RetryAfter time.Duration
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d fetching %s", e.Code, redactURL(e.URL))
}

func statusCode(err error) int {
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.Code
	}
	return 0
}

// retryable classifies an error. Permanent client errors get only a couple of
// retries (some CDNs flake 403/404 under load); everything else gets the full
// exponential-backoff budget.
func retryBudget(err error) int {
	switch c := statusCode(err); {
	case c == 0:
		return maxAttempts // network error, stall, truncated body…
	case c == 429 || c == 408 || c == 425 || c >= 500:
		return maxAttempts
	case c == 403 || c == 404 || c == 410:
		return 3
	default:
		return 1
	}
}

func backoffFor(attempt int, err error) time.Duration {
	var he *httpStatusError
	if errors.As(err, &he) && he.RetryAfter > 0 {
		return min(he.RetryAfter, 2*time.Minute)
	}
	d := baseBackoff << min(attempt, 8)
	d = min(d, maxBackoff)
	// ±25% jitter so parallel workers don't retry in lockstep
	return d*3/4 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// withRetry runs fn until it succeeds, the error's retry budget is exhausted,
// or ctx is cancelled.
func withRetry(ctx context.Context, what string, fn func(attempt int) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(attempt); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt+1 >= retryBudget(err) {
			return err
		}
		wait := backoffFor(attempt, err)
		debugLog.Printf("retry %s (attempt %d) in %v: %v", what, attempt+1, wait.Round(time.Millisecond), err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// ── Requests ──────────────────────────────────────────────────────────────────

func newRequest(ctx context.Context, rawURL string, headers map[string]string, rng ByteRange) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for k, v := range headers {
		if strings.EqualFold(k, "Range") || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		req.Header.Set(k, v)
	}
	if h := rng.Header(); h != "" {
		req.Header.Set("Range", h)
	}
	return req, nil
}

// do performs one request and converts error statuses to *httpStatusError.
func do(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		he := &httpStatusError{Code: resp.StatusCode, URL: req.URL.String()}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n > 0 {
				he.RetryAfter = time.Duration(n) * time.Second
			} else if t, err := http.ParseTime(ra); err == nil {
				he.RetryAfter = time.Until(t)
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests && he.RetryAfter == 0 {
			he.RetryAfter = 5 * time.Second
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, he
	}
	return resp, nil
}

// fetchBytes downloads a whole (small) resource with retries. data: URIs are
// decoded locally.
func fetchBytes(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, rng ByteRange) ([]byte, error) {
	if strings.HasPrefix(rawURL, "data:") {
		return decodeDataURI(rawURL)
	}
	var out []byte
	err := withRetry(ctx, redactURL(rawURL), func(int) error {
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := newRequest(rctx, rawURL, headers, rng)
		if err != nil {
			return err
		}
		resp, err := do(client, req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body := newStallReader(resp.Body, cancel)
		defer body.stop()
		b, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		if resp.ContentLength > 0 && int64(len(b)) < resp.ContentLength {
			return io.ErrUnexpectedEOF
		}
		// A server that ignores Range returns the whole resource — slice it.
		if rng.Length > 0 && resp.StatusCode == http.StatusOK && int64(len(b)) > rng.Length {
			end := min(rng.Offset+rng.Length, int64(len(b)))
			if rng.Offset < end {
				b = b[rng.Offset:end]
			}
		}
		out = b
		return nil
	})
	return out, err
}

// fetchText fetches a manifest/playlist as a string.
func fetchText(ctx context.Context, rawURL string, headers map[string]string) (string, error) {
	debugLog.Printf("fetchText %s | Referer: %s | Cookie: %t", redactURL(rawURL), headers["Referer"], headers["Cookie"] != "")
	b, err := fetchBytes(ctx, stdClient(), rawURL, headers, ByteRange{})
	if statusCode(err) == http.StatusForbidden {
		// Cloudflare-fronted origins reject Go's TLS fingerprint.
		b, err = fetchBytes(ctx, chromeClient(), rawURL, headers, ByteRange{})
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(b), "\uFEFF"), nil
}

func decodeDataURI(s string) ([]byte, error) {
	meta, data, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	if !ok {
		return nil, fmt.Errorf("malformed data URI")
	}
	if strings.HasSuffix(meta, ";base64") {
		return base64.StdEncoding.DecodeString(data)
	}
	v, err := url.PathUnescape(data)
	return []byte(v), err
}

// redactURL strips query strings (signed tokens) from URLs written to logs.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?\u2026"
	}
	return u
}

// ── Stall detection ───────────────────────────────────────────────────────────

// stallReader cancels the request when no bytes arrive for stallTimeout —
// otherwise a silently dead TCP connection would hang a worker forever.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func newStallReader(r io.Reader, cancel context.CancelFunc) *stallReader {
	return &stallReader{r: r, timer: time.AfterFunc(stallTimeout, cancel)}
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}

func (s *stallReader) stop() { s.timer.Stop() }

// ── Parallel ranged file download ─────────────────────────────────────────────

// chunkSizeFor picks the range size for a parallel download. googlevideo
// throttles ranges much above ~10 MB, so YouTube gets smaller chunks.
func chunkSizeFor(rawURL string, size int64, workers int) int64 {
	maxChunk := int64(16 << 20)
	if strings.Contains(rawURL, "googlevideo.com") {
		maxChunk = 10 << 20
	}
	c := size / int64(workers*3)
	return min(max(c, 1<<20), maxChunk)
}

// downloadHTTPFile downloads rawURL to dest using parallel byte ranges when the
// server supports them, falling back to a single resumable stream otherwise.
// Bytes are reported to prog as they arrive.
func downloadHTTPFile(ctx context.Context, prog *progress, rawURL, dest string, headers map[string]string) error {
	client := stdClient()

	// Probe with a 1-byte range: tells us size + range support in one request.
	var probe *http.Response
	err := withRetry(ctx, "probe "+redactURL(rawURL), func(int) error {
		req, err := newRequest(ctx, rawURL, headers, ByteRange{Offset: 0, Length: 1})
		if err != nil {
			return err
		}
		resp, err := do(client, req)
		if statusCode(err) == http.StatusForbidden && client == stdClient() {
			client = chromeClient()
			req2, _ := newRequest(ctx, rawURL, headers, ByteRange{Offset: 0, Length: 1})
			resp, err = do(client, req2)
		}
		if statusCode(err) == http.StatusRequestedRangeNotSatisfiable {
			// Zero-length resource.
			return os.WriteFile(dest, nil, 0o644)
		}
		probe = resp
		return err
	})
	if err != nil {
		return err
	}
	if probe == nil { // empty file written above
		return nil
	}

	size := int64(-1)
	ranged := false
	if probe.StatusCode == http.StatusPartialContent {
		if cr := probe.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
					size, ranged = n, true
				}
			}
		}
	}

	if !ranged {
		// Server ignored Range: stream this very response (no second request).
		return singleStream(ctx, client, prog, rawURL, dest, headers, probe)
	}
	_ = probe.Body.Close()
	prog.addTotal(size)

	workers := segWorkerCount()
	chunk := chunkSizeFor(rawURL, size, workers)
	nChunks := int((size + chunk - 1) / chunk)
	workers = min(workers, nChunks)
	debugLog.Printf("ranged download %s: %s in %d chunks of %s, %d connections",
		redactURL(rawURL), formatBytes(size), nChunks, formatBytes(chunk), workers)

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		return err
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	prog.connections.Add(int64(workers))
	defer prog.connections.Add(-int64(workers))

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(nChunks) || dctx.Err() != nil {
					return
				}
				start := i * chunk
				end := min(start+chunk, size) // exclusive
				if err := fetchRangeInto(dctx, client, prog, rawURL, headers, f, start, end); err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.Sync()
}

// fetchRangeInto downloads [start,end) into f at the same offset, resuming
// from the last written byte on every retry.
func fetchRangeInto(ctx context.Context, client *http.Client, prog *progress, rawURL string, headers map[string]string, f *os.File, start, end int64) error {
	pos := start
	return withRetry(ctx, fmt.Sprintf("range %d-%d", start, end-1), func(int) error {
		if pos >= end {
			return nil
		}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := newRequest(rctx, rawURL, headers, ByteRange{Offset: pos, Length: end - pos})
		if err != nil {
			return err
		}
		resp, err := do(client, req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("server ignored range request (HTTP %d)", resp.StatusCode)
		}
		body := newStallReader(resp.Body, cancel)
		defer body.stop()
		buf := make([]byte, 128<<10)
		for pos < end {
			n, rerr := body.Read(buf[:min(int64(len(buf)), end-pos)])
			if n > 0 {
				if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
					return werr
				}
				pos += int64(n)
				prog.add(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if pos < end {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
}

// singleStream downloads over one connection, resuming with Range after a
// dropped connection when the server advertises byte-range support.
func singleStream(ctx context.Context, client *http.Client, prog *progress, rawURL, dest string, headers map[string]string, first *http.Response) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		_ = first.Body.Close()
		return err
	}
	defer func() { _ = f.Close() }()
	if first.ContentLength > 0 {
		prog.addTotal(first.ContentLength)
	}
	canResume := strings.EqualFold(first.Header.Get("Accept-Ranges"), "bytes")
	var written int64
	resp := first
	prog.connections.Add(1)
	defer prog.connections.Add(-1)

	return withRetry(ctx, "stream "+redactURL(rawURL), func(attempt int) error {
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if resp == nil {
			if written > 0 && !canResume {
				// Cannot resume: start over.
				prog.add(-written)
				written = 0
				if err := f.Truncate(0); err != nil {
					return err
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return err
				}
			}
			rng := ByteRange{}
			if written > 0 {
				rng = ByteRange{Offset: written, Length: 1 << 62}
			}
			req, err := newRequest(rctx, rawURL, headers, rng)
			if err != nil {
				return err
			}
			r, err := do(client, req)
			if err != nil {
				return err
			}
			if written > 0 && r.StatusCode != http.StatusPartialContent {
				_ = r.Body.Close()
				canResume = false
				return fmt.Errorf("server no longer honours range requests")
			}
			resp = r
		}
		body := newStallReader(resp.Body, cancel)
		defer func() {
			body.stop()
			_ = resp.Body.Close()
			resp = nil
		}()
		buf := make([]byte, 256<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					return werr
				}
				written += int64(n)
				prog.add(int64(n))
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	})
}
