// Package webui serves the embedded FluxGet web dashboard at /ui.
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var page []byte

// Handler returns an HTTP handler that serves the dashboard page.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	}
}
