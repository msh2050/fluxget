//go:build linux

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
)

const autostartDesktop = `[Desktop Entry]
Name=FluxGet
Comment=IDM-inspired download manager
Exec=fluxget-gui
Icon=fluxget
Terminal=false
Type=Application
Categories=Network;FileTransfer;
StartupNotify=true
StartupWMClass=fluxget-gui
`

// SetAutoStart enables or disables the FluxGet GUI on login by managing the
// XDG autostart entry (~/.config/autostart/fluxget-gui.desktop). The GUI embeds
// the engine, so launching it is all that is needed.
//
// We deliberately do NOT enable the systemd `fluxget` user service: it would run
// a second headless engine that competes for port 1700 with the GUI's embedded
// engine and keeps downloading in the background after the GUI is closed. We
// always disable that service here to undo any prior state that enabled it.
func SetAutoStart(enable bool) error {
	// Always make sure the redundant headless daemon is stopped + disabled.
	// (Users who genuinely want a headless server can still enable it manually
	// with `systemctl --user enable --now fluxget`.)
	_ = exec.Command("systemctl", "--user", "disable", "--now", "fluxget").Run()

	// XDG autostart entry for the GUI
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	autoDir := filepath.Join(home, ".config", "autostart")
	desktopPath := filepath.Join(autoDir, "fluxget-gui.desktop")

	if enable {
		if err := os.MkdirAll(autoDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(desktopPath, []byte(autostartDesktop), 0o644)
	}
	if err := os.Remove(desktopPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
