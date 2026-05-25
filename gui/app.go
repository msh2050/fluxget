package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	fluxcmd "github.com/msh2050/fluxget/cmd"
	"github.com/msh2050/fluxget/internal/config"
)

type App struct {
	ctx context.Context
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// JSC installs SIGSEGV/SIGBUS/SIGILL handlers lazily when JS first executes
	// (not at WebView init time), so a one-shot patch at startup misses them.
	// Poll rapidly at first, then slowly forever to catch the installation and
	// any future re-registrations. Each call is 3 sigaction syscalls — negligible.
	go func() {
		for i := 0; i < 500; i++ { // first 5 seconds: every 10ms
			time.Sleep(10 * time.Millisecond)
			fixWebKitSignalHandlers()
		}
		for { // afterwards: every 500ms
			time.Sleep(500 * time.Millisecond)
			fixWebKitSignalHandlers()
		}
	}()

	outputDir := config.GetDownloadsDir()
	if outputDir == "" {
		outputDir = filepath.Join(os.Getenv("HOME"), "Downloads")
	}

	go func() {
		if err := fluxcmd.StartEmbedded(1700, outputDir); err != nil {
			log.Printf("FluxGet engine error: %v", err)
		}
	}()

	go InitTray(a)
}

func (a *App) shutdown(ctx context.Context) {
	if err := fluxcmd.ShutdownEmbedded(); err != nil {
		log.Printf("FluxGet shutdown error: %v", err)
	}
}
