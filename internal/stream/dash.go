package stream

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// ── MPD XML structs ───────────────────────────────────────────────────────────

type mpd struct {
	XMLName  xml.Name    `xml:"MPD"`
	BaseURL  string      `xml:"BaseURL"`
	Periods  []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	BaseURL        string             `xml:"BaseURL"`
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	MimeType        string               `xml:"mimeType,attr"`
	ContentType     string               `xml:"contentType,attr"`
	Lang            string               `xml:"lang,attr"`
	BaseURL         string               `xml:"BaseURL"`
	SegTemplate     *mpdSegmentTemplate  `xml:"SegmentTemplate"`
	Representations []mpdRepresentation  `xml:"Representation"`
}

type mpdRepresentation struct {
	ID          string               `xml:"id,attr"`
	Bandwidth   int                  `xml:"bandwidth,attr"`
	Width       int                  `xml:"width,attr"`
	Height      int                  `xml:"height,attr"`
	Codecs      string               `xml:"codecs,attr"`
	MimeType    string               `xml:"mimeType,attr"`
	BaseURL     string               `xml:"BaseURL"`
	SegTemplate *mpdSegmentTemplate  `xml:"SegmentTemplate"`
}

type mpdSegmentTemplate struct {
	Initialization string    `xml:"initialization,attr"`
	Media          string    `xml:"media,attr"`
	StartNumber    int       `xml:"startNumber,attr"`
	Timescale      int64     `xml:"timescale,attr"`
	Duration       int64     `xml:"duration,attr"`
	Timeline       []mpdS    `xml:"SegmentTimeline>S"`
}

// mpdS is an <S> element in SegmentTimeline: t=start_time, d=duration, r=repeat_count.
type mpdS struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int   `xml:"r,attr"`
}

// ── Public types ──────────────────────────────────────────────────────────────

// DASHStream represents one video or audio stream extracted from an MPD.
type DASHStream struct {
	Type      string // "video" or "audio"
	MimeType  string
	Codecs    string
	Width     int
	Height    int
	Bandwidth int
	InitURL   string   // initialization segment URL
	SegURLs   []string // media segment URLs in order
}

// ParseMPD parses a DASH MPD XML document and returns all streams.
// baseURL is the URL the MPD was fetched from, used to resolve relative URLs.
func ParseMPD(body, baseURL string) ([]DASHStream, error) {
	var m mpd
	if err := xml.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("dash: invalid MPD XML: %w", err)
	}

	// Resolve base URLs from the document
	docBase := resolveBase(baseURL, m.BaseURL)

	var streams []DASHStream
	for _, period := range m.Periods {
		periodBase := resolveBase(docBase, period.BaseURL)

		for _, as := range period.AdaptationSets {
			asBase := resolveBase(periodBase, as.BaseURL)
			contentType := asContentType(as)

			for _, rep := range as.Representations {
				repBase := resolveBase(asBase, rep.BaseURL)
				mime := firstNonEmpty(rep.MimeType, as.MimeType)

				// SegmentTemplate: prefer representation-level, fall back to adaptation set
				tmpl := rep.SegTemplate
				if tmpl == nil {
					tmpl = as.SegTemplate
				}
				if tmpl == nil {
					continue
				}

				initURL := expandTemplate(tmpl.Initialization, rep.ID, rep.Bandwidth, 0, 0)
				if initURL != "" {
					initURL, _ = resolveURL(repBase, initURL)
				}

				segURLs, err := buildSegmentURLs(tmpl, repBase, rep.ID, rep.Bandwidth)
				if err != nil {
					return nil, err
				}

				streams = append(streams, DASHStream{
					Type:      contentType,
					MimeType:  mime,
					Codecs:    rep.Codecs,
					Width:     rep.Width,
					Height:    rep.Height,
					Bandwidth: rep.Bandwidth,
					InitURL:   initURL,
					SegURLs:   segURLs,
				})
			}
		}
	}
	return streams, nil
}

// DASHBestVideo returns the video stream with the highest bandwidth.
func DASHBestVideo(streams []DASHStream) *DASHStream {
	var best *DASHStream
	for i := range streams {
		s := &streams[i]
		if s.Type != "video" {
			continue
		}
		if best == nil || s.Bandwidth > best.Bandwidth {
			best = s
		}
	}
	return best
}

// DASHBestAudio returns the audio stream with the highest bandwidth (or first found).
func DASHBestAudio(streams []DASHStream) *DASHStream {
	var best *DASHStream
	for i := range streams {
		s := &streams[i]
		if s.Type != "audio" {
			continue
		}
		if best == nil || s.Bandwidth > best.Bandwidth {
			best = s
		}
	}
	return best
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func asContentType(as mpdAdaptationSet) string {
	if as.ContentType != "" {
		t := strings.ToLower(as.ContentType)
		if strings.HasPrefix(t, "video") {
			return "video"
		}
		if strings.HasPrefix(t, "audio") {
			return "audio"
		}
	}
	mime := strings.ToLower(as.MimeType)
	if strings.HasPrefix(mime, "video") {
		return "video"
	}
	if strings.HasPrefix(mime, "audio") {
		return "audio"
	}
	// Check representations' mime types
	for _, r := range as.Representations {
		m := strings.ToLower(r.MimeType)
		if strings.HasPrefix(m, "video") {
			return "video"
		}
		if strings.HasPrefix(m, "audio") {
			return "audio"
		}
	}
	return "video"
}

func buildSegmentURLs(tmpl *mpdSegmentTemplate, base, repID string, bandwidth int) ([]string, error) {
	if tmpl.Media == "" {
		return nil, nil
	}

	var segs []string

	if len(tmpl.Timeline) > 0 {
		// SegmentTimeline-based: explicit segment durations
		startNum := tmpl.StartNumber
		if startNum == 0 {
			startNum = 1
		}
		num := startNum
		var t int64
		for _, s := range tmpl.Timeline {
			if s.T > 0 {
				t = s.T
			}
			repeat := s.R
			if repeat < 0 {
				// r=-1 means repeat until end of period — not common; skip
				repeat = 0
			}
			for i := 0; i <= repeat; i++ {
				rawURL := expandTemplate(tmpl.Media, repID, bandwidth, num, t)
				u, _ := resolveURL(base, rawURL)
				segs = append(segs, u)
				num++
				t += s.D
			}
		}
	} else if tmpl.Duration > 0 {
		// Fixed-duration segments: calculate count from segment duration
		// We don't have total duration here; return an empty slice with a note.
		// Callers should fetch the manifest duration separately if needed.
		// For now, generate a reasonable number of segments (caller can stream until 404).
		startNum := tmpl.StartNumber
		if startNum == 0 {
			startNum = 1
		}
		// Generate up to 50000 segment placeholders; downloader will stop on 404.
		for i := startNum; i < startNum+50000; i++ {
			rawURL := expandTemplate(tmpl.Media, repID, bandwidth, i, int64(i-startNum)*tmpl.Duration)
			u, _ := resolveURL(base, rawURL)
			segs = append(segs, u)
		}
	}

	return segs, nil
}

// expandTemplate replaces DASH template variables.
func expandTemplate(tmpl, repID string, bandwidth, number int, time int64) string {
	s := tmpl
	s = strings.ReplaceAll(s, "$RepresentationID$", repID)
	s = strings.ReplaceAll(s, "$Bandwidth$", strconv.Itoa(bandwidth))
	s = strings.ReplaceAll(s, "$Number$", strconv.Itoa(number))
	s = strings.ReplaceAll(s, "$Time$", strconv.FormatInt(time, 10))
	// Padded variants: $Number%06d$
	s = expandPadded(s, "$Number%", number)
	s = expandPadded(s, "$Time%", int(time))
	s = strings.ReplaceAll(s, "$$", "$")
	return s
}

func expandPadded(s, prefix string, val int) string {
	for {
		start := strings.Index(s, prefix)
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "$")
		if end < 1 {
			break
		}
		end += start
		// e.g. "$Number%06d$" → format string is "%06d"
		fmtStr := s[start+1 : end] // e.g. "Number%06d"
		pctIdx := strings.IndexByte(fmtStr, '%')
		if pctIdx < 0 {
			break
		}
		goFmt := "%" + fmtStr[pctIdx+1:] // e.g. "%06d"
		formatted := fmt.Sprintf(goFmt, val)
		s = s[:start] + formatted + s[end+1:]
	}
	return s
}

func resolveBase(parent, child string) string {
	if child == "" {
		return parent
	}
	u, err := resolveURL(parent, child)
	if err != nil {
		return parent
	}
	return u
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
