package cmd

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/msh2050/fluxget/internal/config"
	"github.com/msh2050/fluxget/internal/service"
	"github.com/msh2050/fluxget/internal/utils"
)

func resolveTokenPath() string {
	return filepath.Join(config.GetStateDir(), "token")
}

func resolveRuntimeDir() string {
	return config.GetRuntimeDir()
}

const serverBindHost = "0.0.0.0"

const maxAPIRequestBodyBytes = 8 << 20

// findAvailablePort tries ports starting from 'start' until one is available
func findAvailablePort(start int) (int, net.Listener) {
	bindHost := serverBindHost
	for port := start; port < start+100; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindHost, port))
		if err == nil {
			return port, ln
		}
	}
	return 0, nil
}

func bindServerListener(portFlag int) (int, net.Listener, error) {
	bindHost := serverBindHost
	if portFlag > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindHost, portFlag))
		if err != nil {
			return 0, nil, fmt.Errorf("could not bind to port %d: %w", portFlag, err)
		}
		return portFlag, ln, nil
	}
	port, ln := findAvailablePort(1700)
	if ln == nil {
		return 0, nil, fmt.Errorf("could not find available port")
	}
	return port, ln, nil
}

// saveActivePort writes the active port for local CLI and extension discovery.
func saveActivePort(port int) {
	runtimeDir := resolveRuntimeDir()
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		utils.Debug("Error creating runtime directory for port file: %v", err)
		return
	}

	portFile := filepath.Join(runtimeDir, "port")
	if err := os.WriteFile(portFile, []byte(fmt.Sprintf("%d", port)), 0o644); err != nil {
		utils.Debug("Error writing port file: %v", err)
	}
	utils.Debug("HTTP server listening on port %d", port)
}

// removeActivePort cleans up the port file on exit
func removeActivePort() {
	portFile := filepath.Join(resolveRuntimeDir(), "port")
	if err := os.Remove(portFile); err != nil && !os.IsNotExist(err) {
		utils.Debug("Error removing port file: %v", err)
	}
}

var (
	globalHTTPServer   *http.Server
	globalHTTPServerMu sync.Mutex
	activeServerMode   = "local"
)

// startHTTPServer starts the HTTP server using an existing listener
func startHTTPServer(ln net.Listener, port int, defaultOutputDir string, service service.DownloadService, tokenOverride string) {
	authToken := strings.TrimSpace(tokenOverride)
	if authToken == "" {
		authToken = ensureAuthToken()
	} else {
		persistAuthToken(authToken)
	}

	mux := http.NewServeMux()
	registerHTTPRoutes(mux, port, defaultOutputDir, service)

	// Wrap mux with Auth and CORS (CORS outermost to ensure 401/403 include headers)
	handler := corsMiddleware(authMiddleware(authToken, limitAPIRequestBody(mux)))

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	globalHTTPServerMu.Lock()
	globalHTTPServer = server
	globalHTTPServerMu.Unlock()
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		utils.Debug("HTTP server error: %v", err)
	}
}

func limitAPIRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxAPIRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, PUT, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Access-Control-Allow-Private-Network")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow health check without auth
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Allow OPTIONS for CORS preflight
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}

		// Loopback connections (browser extension on same machine) bypass token
		// auth — but only for non-browser clients, the FluxGet extension, the
		// dashboard itself and the desktop GUI. Without this, any web page the
		// user visits could POST to 127.0.0.1 and download files to arbitrary
		// folders (drive-by request forgery).
		if isLoopback(r) {
			if !loopbackOriginAllowed(r) {
				http.Error(w, "Forbidden: cross-origin request from "+r.Header.Get("Origin"), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization header
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			providedToken := strings.TrimPrefix(authHeader, "Bearer ")
			if len(providedToken) == len(token) && subtle.ConstantTimeCompare([]byte(providedToken), []byte(token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Allow ?token= query param for browser UI access
		if qt := r.URL.Query().Get("token"); qt != "" {
			if len(qt) == len(token) && subtle.ConstantTimeCompare([]byte(qt), []byte(token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func ensureAuthToken() string {
	tokenFile := resolveTokenPath()
	if token, err := readTokenFromFile(tokenFile); err == nil {
		return token
	}

	token := uuid.New().String()
	if err := writeTokenToFile(tokenFile, token); err != nil {
		utils.Debug("Failed to write token file in %s: %v", tokenFile, err)
	}
	return token
}

func persistAuthToken(token string) {
	tokenFile := resolveTokenPath()
	if err := writeTokenToFile(tokenFile, token); err != nil {
		utils.Debug("Failed to write token file in %s: %v", tokenFile, err)
	}
}

func readTokenFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("empty token file: %s", path)
	}
	return token, nil
}

func writeTokenToFile(path string, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// loopbackOriginAllowed decides whether a loopback request may skip token auth
// based on the browser-supplied Origin / Sec-Fetch-Site headers.
func loopbackOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Non-browser clients (CLI, curl, GUI proxy) send no Origin. Browsers
		// omit it on simple GETs; reject those only when they are flagged as
		// cross-site navigations/subresources.
		return !strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
	}
	u, err := url.Parse(origin)
	if err != nil || origin == "null" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "chrome-extension", "moz-extension", "safari-web-extension", "extension", "wails":
		return true
	case "http", "https":
	default:
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(u.Host, r.Host) {
		return true // same-origin: the /ui dashboard
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true // desktop GUI (wails.localhost) and local dev servers
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
