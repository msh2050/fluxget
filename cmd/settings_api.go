package cmd

import "github.com/msh2050/fluxget/internal/config"

// flatSettings is the simplified settings shape exposed to the GUI.
// Only the fields the frontend actually reads/writes are included.
type flatSettings struct {
	// General
	Dir       string `json:"dir"`
	AutoStart bool   `json:"autoStart"`
	Clipboard bool   `json:"clipboard"`

	// Surge engine
	MaxConns       int     `json:"maxConns"`
	MaxConcurrent  int     `json:"maxConcurrent"`
	SpeedLimit     float64 `json:"speedLimit"`
	SpeedLimitUnit string  `json:"speedLimitUnit"`
	MirrorDisc     bool    `json:"mirrorDisc"`
	UserAgent      string  `json:"userAgent"`
	ProxyURL       string  `json:"proxyUrl"`

	// HLS/DASH engine
	Quality      string `json:"quality"`
	MuxContainer string `json:"muxContainer"`

	// yt-dlp engine
	YtdlpPath     string `json:"ytdlpPath"`
	YtdlpQuality  string `json:"ytdlpQuality"`
	YtdlpFormat   string `json:"ytdlpFormat"`

	// Browser extension
	Port     int  `json:"port"`
	AutoPair bool `json:"autoPair"`
}

func flattenSettings(s *config.Settings) flatSettings {
	if s == nil {
		s = config.DefaultSettings()
	}
	return flatSettings{
		Dir:            config.Resolve[string](s.General.DefaultDownloadDir),
		AutoStart:      config.Resolve[bool](s.General.AutoStart),
		Clipboard:      config.Resolve[bool](s.General.ClipboardMonitor),
		MaxConns:       config.Resolve[int](s.Network.MaxConnectionsPerDownload),
		MaxConcurrent:  config.Resolve[int](s.Network.MaxConcurrentDownloads),
		SpeedLimit:     0,
		SpeedLimitUnit: "MB/s",
		MirrorDisc:     !config.Resolve[bool](s.Network.SequentialDownload),
		UserAgent:      config.Resolve[string](s.Network.UserAgent),
		ProxyURL:       config.Resolve[string](s.Network.ProxyURL),
		Quality:        "best",
		MuxContainer:   "mkv",
		YtdlpPath:      "/usr/local/bin/yt-dlp",
		YtdlpQuality:   "720",
		YtdlpFormat:    "mp4",
		Port:           1700,
		AutoPair:       false,
	}
}

func applyFlatSettings(s *config.Settings, f flatSettings) {
	if s == nil {
		return
	}
	setStr := func(setting **config.Setting, val string) {
		if *setting == nil {
			*setting = &config.Setting{}
		}
		(*setting).Value = val
	}
	setBool := func(setting **config.Setting, val bool) {
		v := "false"
		if val {
			v = "true"
		}
		if *setting == nil {
			*setting = &config.Setting{}
		}
		(*setting).Value = v
	}
	setInt := func(setting **config.Setting, val int) {
		if *setting == nil {
			*setting = &config.Setting{}
		}
		(*setting).Value = intToStr(val)
	}

	if f.Dir != "" {
		setStr(&s.General.DefaultDownloadDir, f.Dir)
	}
	setBool(&s.General.AutoStart, f.AutoStart)
	setBool(&s.General.ClipboardMonitor, f.Clipboard)
	if f.MaxConns > 0 {
		setInt(&s.Network.MaxConnectionsPerDownload, f.MaxConns)
	}
	if f.MaxConcurrent > 0 {
		setInt(&s.Network.MaxConcurrentDownloads, f.MaxConcurrent)
	}
	setBool(&s.Network.SequentialDownload, !f.MirrorDisc)
	if f.UserAgent != "" {
		setStr(&s.Network.UserAgent, f.UserAgent)
	}
	setStr(&s.Network.ProxyURL, f.ProxyURL)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	result := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		result = append([]byte{byte('0' + n%10)}, result...)
		n /= 10
	}
	if neg {
		result = append([]byte{'-'}, result...)
	}
	return string(result)
}
