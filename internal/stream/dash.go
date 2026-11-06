package stream

import (
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ── MPD XML structs ───────────────────────────────────────────────────────────

type mpd struct {
	XMLName                   xml.Name    `xml:"MPD"`
	Type                      string      `xml:"type,attr"` // "static" (VOD) or "dynamic" (live)
	MediaPresentationDuration string      `xml:"mediaPresentationDuration,attr"`
	BaseURL                   string      `xml:"BaseURL"`
	Periods                   []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	ID             string              `xml:"id,attr"`
	Duration       string              `xml:"duration,attr"`
	BaseURL        string              `xml:"BaseURL"`
	SegTemplate    *mpdSegmentTemplate `xml:"SegmentTemplate"`
	AdaptationSets []mpdAdaptationSet  `xml:"AdaptationSet"`
}

type mpdContentProtection struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
}

type mpdAdaptationSet struct {
	MimeType          string                 `xml:"mimeType,attr"`
	ContentType       string                 `xml:"contentType,attr"`
	Codecs            string                 `xml:"codecs,attr"`
	Lang              string                 `xml:"lang,attr"`
	BaseURL           string                 `xml:"BaseURL"`
	SegTemplate       *mpdSegmentTemplate    `xml:"SegmentTemplate"`
	SegList           *mpdSegmentList        `xml:"SegmentList"`
	SegBase           *mpdSegmentBase        `xml:"SegmentBase"`
	ContentProtection []mpdContentProtection `xml:"ContentProtection"`
	Roles             []mpdDescriptor        `xml:"Role"`
	Representations   []mpdRepresentation    `xml:"Representation"`
}

type mpdDescriptor struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

type mpdRepresentation struct {
	ID                string                 `xml:"id,attr"`
	Bandwidth         int                    `xml:"bandwidth,attr"`
	Width             int                    `xml:"width,attr"`
	Height            int                    `xml:"height,attr"`
	Codecs            string                 `xml:"codecs,attr"`
	MimeType          string                 `xml:"mimeType,attr"`
	BaseURL           string                 `xml:"BaseURL"`
	SegTemplate       *mpdSegmentTemplate    `xml:"SegmentTemplate"`
	SegList           *mpdSegmentList        `xml:"SegmentList"`
	SegBase           *mpdSegmentBase        `xml:"SegmentBase"`
	ContentProtection []mpdContentProtection `xml:"ContentProtection"`
}

type mpdSegmentTemplate struct {
	Initialization string `xml:"initialization,attr"`
	Media          string `xml:"media,attr"`
	StartNumber    *int   `xml:"startNumber,attr"`
	Timescale      int64  `xml:"timescale,attr"`
	Duration       int64  `xml:"duration,attr"`
	Timeline       []mpdS `xml:"SegmentTimeline>S"`
	InitElem       *struct {
		SourceURL string `xml:"sourceURL,attr"`
	} `xml:"Initialization"`
}

type mpdSegmentList struct {
	Initialization *struct {
		SourceURL string `xml:"sourceURL,attr"`
		Range     string `xml:"range,attr"`
	} `xml:"Initialization"`
	SegmentURLs []struct {
		Media      string `xml:"media,attr"`
		MediaRange string `xml:"mediaRange,attr"`
	} `xml:"SegmentURL"`
}

type mpdSegmentBase struct {
	IndexRange     string `xml:"indexRange,attr"`
	Initialization *struct {
		Range string `xml:"range,attr"`
	} `xml:"Initialization"`
}

// mpdS is an <S> element in SegmentTimeline: t=start_time, d=duration, r=repeat_count.
type mpdS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int    `xml:"r,attr"`
}

// ── Public types ──────────────────────────────────────────────────────────────

// DASHSegment is one fetchable piece of a DASH representation.
type DASHSegment struct {
	URL   string
	Range ByteRange // zero value = whole resource
}

// DASHStream represents one video or audio representation extracted from an MPD.
type DASHStream struct {
	Type      string // "video", "audio", "text" or "image"
	MimeType  string
	Codecs    string
	Lang      string
	Width     int
	Height    int
	Bandwidth int
	InitURL   string    // initialization segment URL ("" when none)
	InitRange ByteRange // byte range of the init segment, if any
	SegURLs   []string  // media segment URLs in order (convenience mirror of Segments)
	Segments  []DASHSegment
	// SingleFile is set when the representation is one progressive file
	// (SegmentBase / bare BaseURL) — download it as a single HTTP resource.
	SingleFile string
}

// ParseMPD parses a DASH MPD XML document and returns all streams.
// baseURL is the URL the MPD was fetched from, used to resolve relative URLs.
// DRM-protected representations make it return ErrDRM; live ("dynamic")
// manifests are rejected because their segment list is a moving window.
func ParseMPD(body, baseURL string) ([]DASHStream, error) {
	var m mpd
	if err := xml.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("dash: invalid MPD XML: %w", err)
	}
	if strings.EqualFold(m.Type, "dynamic") {
		return nil, errLiveStream
	}

	docBase := resolveBase(baseURL, strings.TrimSpace(m.BaseURL))
	totalDur := parseISODuration(m.MediaPresentationDuration)

	var streams []DASHStream
	protected := 0
	for _, period := range m.Periods {
		periodBase := resolveBase(docBase, strings.TrimSpace(period.BaseURL))
		periodDur := parseISODuration(period.Duration)
		if periodDur == 0 && len(m.Periods) == 1 {
			periodDur = totalDur
		}

		for _, as := range period.AdaptationSets {
			asBase := resolveBase(periodBase, strings.TrimSpace(as.BaseURL))
			contentType := asContentType(as)

			for _, rep := range as.Representations {
				if isDRMProtected(as.ContentProtection) || isDRMProtected(rep.ContentProtection) {
					protected++
					continue
				}
				repBase := resolveBase(asBase, strings.TrimSpace(rep.BaseURL))
				s := DASHStream{
					Type:      contentType,
					MimeType:  firstNonEmpty(rep.MimeType, as.MimeType),
					Codecs:    firstNonEmpty(rep.Codecs, as.Codecs),
					Lang:      as.Lang,
					Width:     rep.Width,
					Height:    rep.Height,
					Bandwidth: rep.Bandwidth,
				}

				tmpl := mergeTemplates(rep.SegTemplate, as.SegTemplate, period.SegTemplate)
				list := rep.SegList
				if list == nil {
					list = as.SegList
				}
				switch {
				case tmpl != nil && tmpl.Media != "":
					initTmpl := tmpl.Initialization
					if initTmpl == "" && tmpl.InitElem != nil {
						initTmpl = tmpl.InitElem.SourceURL
					}
					if initTmpl != "" {
						s.InitURL, _ = resolveURL(repBase, expandTemplate(initTmpl, rep.ID, rep.Bandwidth, 0, 0))
					}
					urls, err := buildSegmentURLs(tmpl, repBase, rep.ID, rep.Bandwidth, periodDur)
					if err != nil {
						return nil, err
					}
					for _, u := range urls {
						s.Segments = append(s.Segments, DASHSegment{URL: u})
					}
				case list != nil && len(list.SegmentURLs) > 0:
					if list.Initialization != nil {
						src := list.Initialization.SourceURL
						if src == "" {
							src = repBase
						}
						s.InitURL, _ = resolveURL(repBase, src)
						s.InitRange = parseDashRange(list.Initialization.Range)
					}
					for _, su := range list.SegmentURLs {
						u := repBase
						if su.Media != "" {
							u, _ = resolveURL(repBase, su.Media)
						}
						s.Segments = append(s.Segments, DASHSegment{URL: u, Range: parseDashRange(su.MediaRange)})
					}
				case strings.TrimSpace(rep.BaseURL) != "" || rep.SegBase != nil || as.SegBase != nil:
					// SegmentBase or bare BaseURL: one self-contained file (sidx-indexed
					// fMP4/WebM). Download it whole — parallel ranged HTTP is far
					// faster than walking the sidx index segment by segment.
					s.SingleFile = repBase
				default:
					continue
				}
				for _, seg := range s.Segments {
					s.SegURLs = append(s.SegURLs, seg.URL)
				}
				streams = append(streams, s)
			}
		}
	}
	if len(streams) == 0 && protected > 0 {
		return nil, ErrDRM
	}
	return streams, nil
}

// errLiveStream marks a live manifest (HLS without ENDLIST / dynamic MPD).
var errLiveStream = fmt.Errorf("stream is a live broadcast")

// isDRMProtected reports whether ContentProtection descriptors name a real DRM
// system. The generic "mp4protection" (CENC) marker always accompanies a DRM
// system entry, so any descriptor at all means the content is encrypted.
func isDRMProtected(cps []mpdContentProtection) bool {
	return len(cps) > 0
}

func mergeTemplates(levels ...*mpdSegmentTemplate) *mpdSegmentTemplate {
	var out *mpdSegmentTemplate
	// Most specific first; fill missing fields from outer levels.
	for _, t := range levels {
		if t == nil {
			continue
		}
		if out == nil {
			c := *t
			out = &c
			continue
		}
		if out.Media == "" {
			out.Media = t.Media
		}
		if out.Initialization == "" {
			out.Initialization = t.Initialization
		}
		if out.InitElem == nil {
			out.InitElem = t.InitElem
		}
		if out.StartNumber == nil {
			out.StartNumber = t.StartNumber
		}
		if out.Timescale == 0 {
			out.Timescale = t.Timescale
		}
		if out.Duration == 0 {
			out.Duration = t.Duration
		}
		if len(out.Timeline) == 0 {
			out.Timeline = t.Timeline
		}
	}
	return out
}

// DASHBestVideo returns the video stream with the highest resolution/bandwidth.
func DASHBestVideo(streams []DASHStream) *DASHStream { return DASHPickVideo(streams, 0) }

// DASHPickVideo returns the best video stream with height <= maxHeight
// (0 = unlimited); if none fit, the smallest one.
func DASHPickVideo(streams []DASHStream, maxHeight int) *DASHStream {
	better := func(a, b *DASHStream) bool {
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		return a.Bandwidth > b.Bandwidth
	}
	var best, lowest *DASHStream
	for i := range streams {
		s := &streams[i]
		if s.Type != "video" {
			continue
		}
		if lowest == nil || better(lowest, s) {
			lowest = s
		}
		if maxHeight > 0 && s.Height > maxHeight {
			continue
		}
		if best == nil || better(s, best) {
			best = s
		}
	}
	if best == nil {
		return lowest
	}
	return best
}

// DASHBestAudio returns the audio stream with the highest bandwidth. When
// several languages exist, the first language listed in the MPD wins (that is
// the original-language track on virtually every service).
func DASHBestAudio(streams []DASHStream) *DASHStream {
	var best *DASHStream
	lang := ""
	for i := range streams {
		if streams[i].Type == "audio" {
			lang = streams[i].Lang
			break
		}
	}
	for i := range streams {
		s := &streams[i]
		if s.Type != "audio" || s.Lang != lang {
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
	classify := func(s string) string {
		s = strings.ToLower(s)
		switch {
		case strings.HasPrefix(s, "video"):
			return "video"
		case strings.HasPrefix(s, "audio"):
			return "audio"
		case strings.HasPrefix(s, "text"), strings.Contains(s, "ttml"), strings.Contains(s, "vtt"):
			return "text"
		case strings.HasPrefix(s, "image"):
			return "image"
		}
		return ""
	}
	if t := classify(as.ContentType); t != "" {
		return t
	}
	if t := classify(as.MimeType); t != "" {
		return t
	}
	for _, r := range as.Representations {
		if t := classify(r.MimeType); t != "" {
			return t
		}
	}
	// Last resort: codecs. stpp/wvtt are subtitles.
	c := strings.ToLower(as.Codecs)
	for _, r := range as.Representations {
		c += "," + strings.ToLower(r.Codecs)
	}
	switch {
	case strings.Contains(c, "stpp"), strings.Contains(c, "wvtt"):
		return "text"
	case strings.Contains(c, "mp4a"), strings.Contains(c, "opus"), strings.Contains(c, "ac-3"), strings.Contains(c, "ec-3"), strings.Contains(c, "flac"):
		if !strings.Contains(c, "avc") && !strings.Contains(c, "hvc") && !strings.Contains(c, "hev") && !strings.Contains(c, "vp") && !strings.Contains(c, "av01") {
			return "audio"
		}
	}
	return "video"
}

// maxSegments guards against absurd manifests (e.g. a bogus duration).
const maxSegments = 200000

func buildSegmentURLs(tmpl *mpdSegmentTemplate, base, repID string, bandwidth int, periodDur float64) ([]string, error) {
	if tmpl.Media == "" {
		return nil, nil
	}
	startNum := 1
	if tmpl.StartNumber != nil {
		startNum = *tmpl.StartNumber
	}
	timescale := tmpl.Timescale
	if timescale <= 0 {
		timescale = 1
	}

	var segs []string
	add := func(num int, t int64) {
		u, _ := resolveURL(base, expandTemplate(tmpl.Media, repID, bandwidth, num, t))
		segs = append(segs, u)
	}

	if len(tmpl.Timeline) > 0 {
		num := startNum
		var t int64
		periodEnd := int64(periodDur * float64(timescale))
		for i, s := range tmpl.Timeline {
			if s.T != nil {
				t = *s.T
			}
			if s.D <= 0 {
				continue
			}
			repeat := s.R
			if repeat < 0 {
				// r=-1: repeat until the next S@t or the end of the period.
				end := periodEnd
				if i+1 < len(tmpl.Timeline) && tmpl.Timeline[i+1].T != nil {
					end = *tmpl.Timeline[i+1].T
				}
				if end <= t {
					repeat = 0
				} else {
					repeat = int(math.Ceil(float64(end-t)/float64(s.D))) - 1
				}
			}
			for r := 0; r <= repeat; r++ {
				add(num, t)
				num++
				t += s.D
				if len(segs) > maxSegments {
					return nil, fmt.Errorf("dash: manifest declares more than %d segments", maxSegments)
				}
			}
		}
		return segs, nil
	}

	if tmpl.Duration > 0 {
		if periodDur <= 0 {
			return nil, fmt.Errorf("dash: fixed-duration SegmentTemplate without a presentation duration")
		}
		segDur := float64(tmpl.Duration) / float64(timescale)
		count := int(math.Ceil(periodDur/segDur - 1e-9))
		if count <= 0 || count > maxSegments {
			return nil, fmt.Errorf("dash: implausible segment count %d", count)
		}
		for i := 0; i < count; i++ {
			add(startNum+i, int64(i)*tmpl.Duration)
		}
	}
	return segs, nil
}

var templateVarRe = regexp.MustCompile(`\$(RepresentationID|Number|Bandwidth|Time)(%0?(\d+)[dioxX])?\$`)

// expandTemplate replaces DASH template identifiers ($Number$, $Time%08d$, …)
// per ISO/IEC 23009-1 §5.3.9.4.4. "$$" is an escaped dollar sign.
func expandTemplate(tmpl, repID string, bandwidth, number int, time int64) string {
	const dollar = "\x00DOLLAR\x00"
	s := strings.ReplaceAll(tmpl, "$$", dollar)
	s = templateVarRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := templateVarRe.FindStringSubmatch(m)
		name, width := sub[1], sub[3]
		var val int64
		switch name {
		case "RepresentationID":
			return repID
		case "Number":
			val = int64(number)
		case "Bandwidth":
			val = int64(bandwidth)
		case "Time":
			val = time
		}
		if width != "" {
			w, _ := strconv.Atoi(width)
			return fmt.Sprintf("%0*d", w, val)
		}
		return strconv.FormatInt(val, 10)
	})
	return strings.ReplaceAll(s, dollar, "$")
}

// parseISODuration parses an xs:duration such as "PT1H2M3.5S" or "P1DT2H"
// into seconds. Returns 0 on failure.
func parseISODuration(s string) float64 {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "P") {
		return 0
	}
	s = s[1:]
	var total float64
	inTime := false
	num := ""
	for _, c := range s {
		switch {
		case c == 'T':
			inTime = true
		case (c >= '0' && c <= '9') || c == '.':
			num += string(c)
		default:
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0
			}
			num = ""
			switch {
			case c == 'Y':
				total += v * 365 * 86400
			case c == 'M' && !inTime:
				total += v * 30 * 86400
			case c == 'W':
				total += v * 7 * 86400
			case c == 'D':
				total += v * 86400
			case c == 'H':
				total += v * 3600
			case c == 'M':
				total += v * 60
			case c == 'S':
				total += v
			default:
				return 0
			}
		}
	}
	return total
}

// parseDashRange parses a DASH "first-last" byte range.
func parseDashRange(s string) ByteRange {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return ByteRange{}
	}
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || last < first {
		return ByteRange{}
	}
	return ByteRange{Offset: first, Length: last - first + 1}
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
