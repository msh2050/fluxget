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
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed appicon48.png
var windowIcon []byte

// apiPaths are the backend routes proxied to the FluxGet HTTP server on :1700.
var apiPaths = []string{
	"/events", "/list", "/history", "/pause", "/resume",
	"/delete", "/download", "/stream", "/health", "/open-file", "/open-folder",
	"/update-url", "/settings", "/gui/settings", "/ytdlp", "/ui",
}

func backendProxy() func(http.Handler) http.Handler {
	target, _ := url.Parse("http://localhost:1700")
	// Rewrite (not the deprecated Director): unlike Director it never adds
	// X-Forwarded-For and strips inbound X-Forwarded-* headers. The backend's
	// open-file/open-folder guard rejects requests carrying those even over
	// loopback, so the proxied call must look like a plain local client.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("X-Real-IP")
			// The webview's own origin is trusted; drop it so the backend's
			// cross-origin guard treats this as a local client.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Sec-Fetch-Site")
		},
	}
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
		BackgroundColour:  &options.RGBA{R: 11, G: 11, B: 12, A: 255},
		HideWindowOnClose: true,
		// Prevent a second tray icon / engine: if FluxGet is already running
		// (e.g. GNOME session-restore plus the XDG autostart entry both fire on
		// login), focus the existing window instead of starting a new instance.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "io.fluxget.app",
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				wailsruntime.WindowShow(app.ctx)
				wailsruntime.WindowUnminimise(app.ctx)
			},
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []interface{}{app},
		Linux: &linux.Options{
			ProgramName:         "fluxget-gui",
			Icon:                windowIcon,
			WindowIsTranslucent: false,
			WebviewGpuPolicy:    linux.WebviewGpuPolicyOnDemand,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
