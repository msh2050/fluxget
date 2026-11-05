package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackOriginGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware("tok", ok)
	cases := []struct {
		origin, site string
		want         int
	}{
		{"", "", http.StatusOK},                                      // curl / CLI
		{"chrome-extension://abcdefgh", "", http.StatusOK},           // FluxGet extension
		{"moz-extension://1234-5678", "", http.StatusOK},             // Firefox extension
		{"http://127.0.0.1:1700", "same-origin", http.StatusOK},      // /ui dashboard
		{"http://wails.localhost", "", http.StatusOK},                // desktop GUI
		{"https://evil.example", "cross-site", http.StatusForbidden}, // drive-by page
		{"null", "cross-site", http.StatusForbidden},                 // sandboxed iframe
		{"", "cross-site", http.StatusForbidden},                     // no-cors cross-site GET
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1700/download", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("origin=%q site=%q: got %d want %d", c.origin, c.site, rec.Code, c.want)
		}
	}
}
