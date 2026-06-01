//go:build linux

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed appicon16.png
var trayIcon []byte

const (
	trayBase     = "http://127.0.0.1:1700"
	maxTraySlots = 5 // how many active downloads to surface in the menu
)

// trayStatus mirrors the fields of engine.types.DownloadStatus that the tray
// needs from GET /list.
type trayStatus struct {
	ID         string  `json:"id"`
	Filename   string  `json:"filename"`
	TotalSize  int64   `json:"total_size"`
	Downloaded int64   `json:"downloaded"`
	Progress   float64 `json:"progress"`
	Speed      float64 `json:"speed"`
	Status     string  `json:"status"`
}

// traySlot is one reusable menu entry for an active download, with its controls.
type traySlot struct {
	parent *systray.MenuItem
	pause  *systray.MenuItem
	resume *systray.MenuItem
	cancel *systray.MenuItem
	id     string
}

// InitTray starts the system tray icon. It must be called in a goroutine
// because systray.Run blocks until the tray exits.
func InitTray(app *App) {
	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("FluxGet")
		systray.SetTooltip("FluxGet — download manager")

		summary := systray.AddMenuItem("No active downloads", "")
		summary.Disable()
		systray.AddSeparator()

		// Pre-allocate a fixed pool of download slots; systray can't remove
		// items, so we show/hide and re-title them as the list changes.
		slots := make([]*traySlot, maxTraySlots)
		for i := range slots {
			parent := systray.AddMenuItem("", "")
			s := &traySlot{
				parent: parent,
				pause:  parent.AddSubMenuItem("⏸  Pause", "Pause this download"),
				resume: parent.AddSubMenuItem("▶  Resume", "Resume this download"),
				cancel: parent.AddSubMenuItem("✕  Cancel", "Remove this download"),
			}
			parent.Hide()
			slots[i] = s
		}
		systray.AddSeparator()

		mShow := systray.AddMenuItem("Show FluxGet", "Bring the window to the front")
		mHide := systray.AddMenuItem("Hide", "Hide the window")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Exit FluxGet completely")

		// Per-slot control handlers — call the local engine API (loopback,
		// no auth). Clicking the row itself opens the window on that download.
		for _, s := range slots {
			s := s
			go func() {
				for {
					select {
					case <-s.pause.ClickedCh:
						trayPost("/pause", s.id)
					case <-s.resume.ClickedCh:
						trayPost("/resume", s.id)
					case <-s.cancel.ClickedCh:
						trayPost("/delete", s.id)
					case <-s.parent.ClickedCh:
						runtime.WindowShow(app.ctx)
					}
				}
			}()
		}

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

		// Poll the engine and refresh the menu.
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				refreshTray(summary, slots)
				<-t.C
			}
		}()
	}

	systray.Run(onReady, func() {})
}

// refreshTray pulls the active downloads and repaints the slot pool + summary.
func refreshTray(summary *systray.MenuItem, slots []*traySlot) {
	var active []trayStatus
	for _, d := range trayList() {
		switch d.Status {
		case "downloading", "paused", "queued":
			active = append(active, d)
		}
	}

	switch {
	case len(active) == 0:
		summary.SetTitle("No active downloads")
	case len(active) == 1:
		summary.SetTitle("1 active download")
	default:
		summary.SetTitle(fmt.Sprintf("%d active downloads", len(active)))
	}

	for i, s := range slots {
		if i >= len(active) {
			s.id = ""
			s.parent.Hide()
			continue
		}
		d := active[i]
		s.id = d.ID
		s.parent.SetTitle(trayLabel(d))
		if d.Status == "paused" {
			s.pause.Hide()
			s.resume.Show()
		} else {
			s.pause.Show()
			s.resume.Hide()
		}
		s.parent.Show()
	}
}

// trayLabel renders one download as "name — 45% · 12.3/100 MB ↓ 2.1 MB/s".
// Total is omitted when unknown (mirrors IDM's unknown-size behaviour).
func trayLabel(d trayStatus) string {
	name := d.Filename
	if len(name) > 32 {
		name = name[:31] + "…"
	}
	var b strings.Builder
	b.WriteString(name)
	if d.Status == "paused" {
		b.WriteString("  (paused)")
	} else if d.Status == "queued" {
		b.WriteString("  (queued)")
	}
	if d.TotalSize > 0 {
		fmt.Fprintf(&b, " — %.0f%% · %s/%s", d.Progress*100, fmtBytes(d.Downloaded), fmtBytes(d.TotalSize))
	} else if d.Downloaded > 0 {
		fmt.Fprintf(&b, " — %s", fmtBytes(d.Downloaded))
	}
	if d.Status == "downloading" && d.Speed > 0 {
		fmt.Fprintf(&b, "  ↓ %s/s", fmtBytes(int64(d.Speed)))
	}
	return b.String()
}

func trayList() []trayStatus {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(trayBase + "/list")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []trayStatus
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func trayPost(path, id string) {
	if id == "" {
		return
	}
	client := http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, trayBase+path+"?id="+id, nil)
	if err != nil {
		return
	}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func fmtBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGT"[exp])
}
