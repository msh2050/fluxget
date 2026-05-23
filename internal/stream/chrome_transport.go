package stream

// chrome_transport.go — Chrome TLS-fingerprint HTTP client for Cloudflare-protected CDNs.
//
// Why this exists:
//   Cloudflare Bot Management ties the __cf_bm cookie to the browser's JA3/JA4 TLS
//   fingerprint. Go's net/http TLS ClientHello is identifiably different from Chrome, so
//   replaying a cookie captured from a browser session triggers a 403 even with valid auth.
//   Using utls with HelloChrome_Auto gives a byte-identical TLS handshake, bypassing the
//   fingerprint check. IDM on Windows avoids this by using WinHTTP (same OS TLS stack as
//   Chrome/Edge).
//
// ALPN override (http/1.1 only):
//   Chrome's spec normally advertises ["h2", "http/1.1"]. If h2 is offered, CDNs like
//   surrit.com negotiate HTTP/2 and send binary HTTP/2 frames. net/http.Transport only
//   upgrades to HTTP/2 for *tls.Conn — *utls.UConn is not *tls.Conn — so it tries to
//   parse HTTP/2 frames as HTTP/1.1 and panics with "malformed HTTP response". Limiting
//   ALPN to http/1.1 forces the server to use HTTP/1.1. JA3 is unchanged (it hashes the
//   extension list, not ALPN content); JA4 differs only in the ALPN component.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// newChromeClient returns an HTTP client whose TLS handshake mimics Chrome's JA3
// fingerprint. Use this for HLS/DASH segment fetches from Cloudflare-protected CDNs.
func newChromeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DialContext: dialer.DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, _ := net.SplitHostPort(addr)
			if host == "" {
				host = addr
			}

			// Clone Chrome's ClientHello spec.
			spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
			if err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("utls spec clone: %w", err)
			}

			// Override ALPN to http/1.1 only — prevents HTTP/2 negotiation.
			for i, ext := range spec.Extensions {
				if alpn, ok := ext.(*utls.ALPNExtension); ok {
					alpn.AlpnProtocols = []string{"http/1.1"}
					spec.Extensions[i] = alpn
					break
				}
			}

			conn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloCustom)
			if err := conn.ApplyPreset(&spec); err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("utls preset: %w", err)
			}
			if err := conn.HandshakeContext(ctx); err != nil {
				_ = rawConn.Close()
				return nil, err
			}
			return conn, nil
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
