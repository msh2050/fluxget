package main

import (
	"embed"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

//go:embed all:frontend/dist
var assets embed.FS

// apiPaths are the backend routes proxied to the FluxGet HTTP server on :1700.
var apiPaths = []string{
	"/events", "/list", "/history", "/pause", "/resume",
	"/delete", "/download", "/stream", "/health", "/open-file", "/open-folder",
	"/update-url", "/settings",
}

func backendProxy() func(http.Handler) http.Handler {
	target, _ := url.Parse("http://localhost:1700")
	proxy := httputil.NewSingleHostReverseProxy(target)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range apiPaths {
				if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p+"?") {
					proxy.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:     "FluxGet",
		Width:     1280,
		Height:    800,
		MinWidth:  900,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Middleware: backendProxy(),
		},
		BackgroundColour: &options.RGBA{R: 11, G: 11, B: 12, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Linux: &linux.Options{
			WindowIsTranslucent: false,
			WebviewGpuPolicy:    linux.WebviewGpuPolicyOnDemand,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
