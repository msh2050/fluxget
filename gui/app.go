package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

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

	// Belt-and-suspenders: patch any signal handlers registered before our
	// process-wide sigaction() override (signal_fix_linux.go) became active.
	fixWebKitSignalHandlers()

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
