//go:build linux

package main

import (
	_ "embed"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed appicon16.png
var trayIcon []byte

// InitTray starts the system tray icon. It must be called in a goroutine
// because systray.Run blocks until the tray exits.
func InitTray(app *App) {
	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("FluxGet")
		systray.SetTooltip("FluxGet — download manager")

		mShow := systray.AddMenuItem("Show FluxGet", "Bring the window to the front")
		mHide := systray.AddMenuItem("Hide", "Hide the window")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit FluxGet completely")

		go func() {
			for {
				select {
				case <-mShow.ClickedCh:
					runtime.WindowShow(app.ctx)
				case <-mHide.ClickedCh:
					runtime.WindowHide(app.ctx)
				case <-mQuit.ClickedCh:
					systray.Quit()
					runtime.Quit(app.ctx)
					return
				}
			}
		}()
	}

	onExit := func() {}

	systray.Run(onReady, onExit)
}
