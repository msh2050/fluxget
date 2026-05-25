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

// SetAutoStart enables or disables FluxGet on login.
// When enabled it:
//   - starts and enables the systemd user service (headless engine on port 1700)
//   - writes ~/.config/autostart/fluxget-gui.desktop so the GUI opens on login
//
// When disabled both are reversed.
func SetAutoStart(enable bool) error {
	// 1. Systemd user service (headless engine)
	action := "disable"
	if enable {
		action = "enable"
	}
	// --now starts/stops the service immediately in addition to enable/disable
	_ = exec.Command("systemctl", "--user", action, "--now", "fluxget").Run()

	// 2. XDG autostart entry for the GUI
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
