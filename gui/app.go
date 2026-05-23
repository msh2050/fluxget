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

	// Patch WebKit/JSC signal handlers to include SA_ONSTACK so Go 1.25's
	// adjustSignalStack2 check does not panic when JSC fires SIGSEGV/SIGBUS/SIGILL
	// on its own GC stack. Called twice: immediately (for handlers already
	// installed at init time) and after a short delay (for lazy JSC init).
	fixWebKitSignalHandlers()
	go func() {
		time.Sleep(500 * time.Millisecond)
		fixWebKitSignalHandlers()
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
}

func (a *App) shutdown(ctx context.Context) {
	if err := fluxcmd.ShutdownEmbedded(); err != nil {
		log.Printf("FluxGet shutdown error: %v", err)
	}
}
