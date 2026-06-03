package cmd

import (
	"github.com/msh2050/fluxget/internal/config"
	"github.com/msh2050/fluxget/internal/stream"
)

// applyEngineSettings pushes runtime-tunable settings into the engine packages.
// Call after settings are loaded or changed so the live engine reflects them.
func applyEngineSettings(s *config.Settings) {
	if s == nil {
		return
	}
	stream.SetSegmentWorkers(config.Resolve[int](s.Network.StreamSegmentWorkers))
	stream.SetDownloadSubtitles(config.Resolve[bool](s.General.DownloadSubtitles))
}

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
	HLSConns     int    `json:"hlsConns"`
	Subtitles    bool   `json:"subtitles"`

	// yt-dlp engine
	YtdlpPath     string `json:"ytdlpPath"`
	YtdlpQuality  string `json:"ytdlpQuality"`
	YtdlpFormat   string `json:"ytdlpFormat"`

	// Browser extension
	Port     int  `json:"port"`
	AutoPair bool `json:"autoPair"`

	// HuggingFace
	HFToken string `json:"hfToken"`
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
		HLSConns:       config.Resolve[int](s.Network.StreamSegmentWorkers),
		Subtitles:      config.Resolve[bool](s.General.DownloadSubtitles),
		YtdlpPath:      "/usr/local/bin/yt-dlp",
		YtdlpQuality:   "720",
		YtdlpFormat:    "mp4",
		Port:           1700,
		AutoPair:       false,
		HFToken:        config.Resolve[string](s.Extension.HFToken),
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
	if f.HLSConns > 0 {
		setInt(&s.Network.StreamSegmentWorkers, f.HLSConns)
	}
	setBool(&s.General.DownloadSubtitles, f.Subtitles)
	if f.MaxConcurrent > 0 {
		setInt(&s.Network.MaxConcurrentDownloads, f.MaxConcurrent)
	}
	setBool(&s.Network.SequentialDownload, !f.MirrorDisc)
	if f.UserAgent != "" {
		setStr(&s.Network.UserAgent, f.UserAgent)
	}
	setStr(&s.Network.ProxyURL, f.ProxyURL)
	setStr(&s.Extension.HFToken, f.HFToken)
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
