// Package stream provides native HLS/DASH parsing and segment downloading,
// eliminating the yt-dlp dependency for direct manifest URLs.
package stream

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ErrDRM is returned when a manifest is protected by a DRM scheme (Widevine,
// PlayReady, FairPlay, SAMPLE-AES…). Such streams cannot be decrypted without a
// licence, so we fail fast with a clear message instead of producing garbage.
var ErrDRM = errors.New("stream is DRM-protected (Widevine/PlayReady/FairPlay) and cannot be downloaded")

// HLSVariant is one quality level from an HLS master playlist.
type HLSVariant struct {
	URL        string
	Bandwidth  int
	Width      int
	Height     int
	Codecs     string
	AudioGroup string // AUDIO="..." — separate audio rendition group, if any
}

// HLSRendition is one alternative rendition declared via #EXT-X-MEDIA
// (TYPE=AUDIO or TYPE=SUBTITLES).
type HLSRendition struct {
	Type     string // "AUDIO" or "SUBTITLES"
	GroupID  string
	URI      string // media playlist URL ("" = muxed into the variant)
	Language string
	Name     string
	Default  bool
}

// HLSSubtitle is one subtitle rendition declared in an HLS master playlist via
// #EXT-X-MEDIA:TYPE=SUBTITLES. URI points to a subtitle media playlist (or a
// direct .vtt).
type HLSSubtitle struct {
	URI      string // subtitle media playlist or .vtt URL
	Language string // BCP-47 / ISO-639 code, e.g. "ar", "en"
	Name     string // human label, e.g. "العربية"
	Default  bool   // DEFAULT=YES
}

// HLSKey holds AES-128 encryption info from #EXT-X-KEY.
type HLSKey struct {
	URI string // URL to fetch the 16-byte AES-128 key
	IV  []byte // 16-byte IV; nil means derive from sequence number
}

// ByteRange is an HTTP byte range (#EXT-X-BYTERANGE / #EXT-X-MAP BYTERANGE).
// Length 0 means "whole resource".
type ByteRange struct {
	Offset int64
	Length int64
}

// Header returns the HTTP Range header value, or "" when the range is unset.
func (b ByteRange) Header() string {
	if b.Length <= 0 {
		return ""
	}
	return fmt.Sprintf("bytes=%d-%d", b.Offset, b.Offset+b.Length-1)
}

// HLSSegment is one media segment from an HLS media playlist.
type HLSSegment struct {
	URL       string
	Duration  float64
	Key       *HLSKey   // nil = unencrypted
	SeqNum    int       // segment sequence number (for IV derivation)
	InitURL   string    // #EXT-X-MAP initialization segment (fMP4); "" for MPEG-TS
	InitRange ByteRange // byte range of the init segment, if any
	Range     ByteRange // #EXT-X-BYTERANGE sub-range, if any
}

// HLSMediaInfo carries playlist-level facts gathered while parsing.
type HLSMediaInfo struct {
	Live bool // no #EXT-X-ENDLIST — a live/event playlist
}

// ParseHLSMaster parses an HLS master playlist and returns all variants.
// Returns an empty slice (not an error) if body is a media playlist.
func ParseHLSMaster(body, baseURL string) ([]HLSVariant, error) {
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		return nil, nil
	}
	var (
		variants []HLSVariant
		pending  *HLSVariant
	)
	scanner := newLineScanner(body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs := parseHLSAttrs(line[len("#EXT-X-STREAM-INF:"):])
			v := HLSVariant{Codecs: attrs["CODECS"], AudioGroup: attrs["AUDIO"]}
			v.Bandwidth, _ = strconv.Atoi(attrs["BANDWIDTH"])
			if v.Bandwidth == 0 {
				v.Bandwidth, _ = strconv.Atoi(attrs["AVERAGE-BANDWIDTH"])
			}
			if res, ok := attrs["RESOLUTION"]; ok {
				if parts := strings.SplitN(strings.ToLower(res), "x", 2); len(parts) == 2 {
					v.Width, _ = strconv.Atoi(parts[0])
					v.Height, _ = strconv.Atoi(parts[1])
				}
			}
			pending = &v
		case pending != nil && line != "" && !strings.HasPrefix(line, "#"):
			u, err := resolveURL(baseURL, line)
			if err != nil {
				return nil, fmt.Errorf("hls: bad variant URL %q: %w", line, err)
			}
			pending.URL = u
			variants = append(variants, *pending)
			pending = nil
		}
	}
	return variants, nil
}

// ParseHLSRenditions returns every #EXT-X-MEDIA rendition in a master playlist.
func ParseHLSRenditions(body, baseURL string) []HLSRendition {
	var out []HLSRendition
	scanner := newLineScanner(body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attrs := parseHLSAttrs(line[len("#EXT-X-MEDIA:"):])
		r := HLSRendition{
			Type:     strings.ToUpper(attrs["TYPE"]),
			GroupID:  attrs["GROUP-ID"],
			Language: attrs["LANGUAGE"],
			Name:     attrs["NAME"],
			Default:  strings.EqualFold(attrs["DEFAULT"], "YES"),
		}
		if uri := attrs["URI"]; uri != "" {
			if resolved, err := resolveURL(baseURL, uri); err == nil {
				r.URI = resolved
			}
		}
		out = append(out, r)
	}
	return out
}

// ParseHLSSubtitles extracts subtitle renditions from an HLS master playlist
// (#EXT-X-MEDIA:TYPE=SUBTITLES). Returns nil for media playlists.
func ParseHLSSubtitles(body, baseURL string) []HLSSubtitle {
	var subs []HLSSubtitle
	for _, r := range ParseHLSRenditions(body, baseURL) {
		if r.Type != "SUBTITLES" || r.URI == "" {
			continue
		}
		subs = append(subs, HLSSubtitle{URI: r.URI, Language: r.Language, Name: r.Name, Default: r.Default})
	}
	return subs
}

// HLSAudioFor picks the audio rendition a variant should be paired with: the
// DEFAULT=YES entry of its AUDIO group, else the first entry with a URI.
// Returns nil when the variant's audio is muxed into its own segments.
func HLSAudioFor(v HLSVariant, renditions []HLSRendition) *HLSRendition {
	if v.AudioGroup == "" {
		return nil
	}
	var first *HLSRendition
	for i := range renditions {
		r := &renditions[i]
		if r.Type != "AUDIO" || r.GroupID != v.AudioGroup || r.URI == "" {
			continue
		}
		if r.Default {
			return r
		}
		if first == nil {
			first = r
		}
	}
	return first
}

// ParseHLSMedia parses an HLS media playlist and returns all segments in order.
// It handles #EXT-X-KEY (AES-128), #EXT-X-MAP, #EXT-X-BYTERANGE and tracks
// sequence numbers. DRM key methods return ErrDRM.
func ParseHLSMedia(body, baseURL string) ([]HLSSegment, error) {
	segs, _, err := ParseHLSMediaInfo(body, baseURL)
	return segs, err
}

// ParseHLSMediaInfo is ParseHLSMedia plus playlist-level info.
func ParseHLSMediaInfo(body, baseURL string) ([]HLSSegment, HLSMediaInfo, error) {
	var (
		segments     []HLSSegment
		pendingDur   float64
		pendingRange ByteRange
		hasRange     bool
		nextOffset   int64 // byte-range offset continuation within one URL
		currentKey   *HLSKey
		currentInit  string
		initRange    ByteRange
		seqNum       int
		info         = HLSMediaInfo{Live: true}
	)
	scanner := newLineScanner(body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case line == "#EXT-X-ENDLIST":
			info.Live = false
			continue

		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
			if strings.EqualFold(strings.TrimPrefix(line, "#EXT-X-PLAYLIST-TYPE:"), "VOD") {
				info.Live = false
			}
			continue

		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seqNum, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			continue

		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := parseHLSAttrs(line[len("#EXT-X-MAP:"):])
			if uri := attrs["URI"]; uri != "" {
				currentInit, _ = resolveURL(baseURL, uri)
			}
			initRange = ByteRange{}
			if br := attrs["BYTERANGE"]; br != "" {
				initRange, _ = parseByteRange(br, 0)
			}
			continue

		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs := parseHLSAttrs(line[len("#EXT-X-KEY:"):])
			method := strings.ToUpper(attrs["METHOD"])
			keyFormat := attrs["KEYFORMAT"]
			switch {
			case method == "NONE":
				currentKey = nil
			case method == "AES-128" && (keyFormat == "" || strings.EqualFold(keyFormat, "identity")):
				keyURI, _ := resolveURL(baseURL, attrs["URI"])
				var iv []byte
				if ivStr := attrs["IV"]; ivStr != "" {
					ivStr = strings.TrimPrefix(strings.TrimPrefix(ivStr, "0x"), "0X")
					iv, _ = hex.DecodeString(ivStr)
					if len(iv) != 16 {
						iv = nil
					}
				}
				currentKey = &HLSKey{URI: keyURI, IV: iv}
			default:
				// SAMPLE-AES, SAMPLE-AES-CTR, or a Widevine/FairPlay/PlayReady
				// KEYFORMAT — real DRM, not decryptable without a licence server.
				return nil, info, ErrDRM
			}
			continue

		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			br, err := parseByteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"), nextOffset)
			if err == nil {
				pendingRange, hasRange = br, true
			}
			continue

		case strings.HasPrefix(line, "#EXTINF:"):
			raw := line[len("#EXTINF:"):]
			if comma := strings.IndexByte(raw, ','); comma >= 0 {
				raw = raw[:comma]
			}
			pendingDur, _ = strconv.ParseFloat(strings.TrimSpace(raw), 64)
			continue

		case line == "" || strings.HasPrefix(line, "#"):
			continue
		}

		u, err := resolveURL(baseURL, line)
		if err != nil {
			return nil, info, fmt.Errorf("hls: bad segment URL %q: %w", line, err)
		}
		seg := HLSSegment{URL: u, Duration: pendingDur, SeqNum: seqNum, InitURL: currentInit, InitRange: initRange}
		if hasRange {
			// A BYTERANGE without an explicit offset continues from the end of
			// the previous sub-range (RFC 8216 §4.3.2.2) — parseByteRange was
			// given nextOffset for exactly that.
			seg.Range = pendingRange
			nextOffset = pendingRange.Offset + pendingRange.Length
		}
		if currentKey != nil {
			keyCopy := *currentKey
			if keyCopy.IV == nil {
				// Derive IV from sequence number (RFC 8216 §5.2)
				iv := make([]byte, 16)
				binary.BigEndian.PutUint64(iv[8:], uint64(seqNum))
				keyCopy.IV = iv
			}
			seg.Key = &keyCopy
		}
		segments = append(segments, seg)
		pendingDur = 0
		hasRange = false
		pendingRange = ByteRange{}
		seqNum++
	}
	return segments, info, nil
}

// parseByteRange parses "<length>[@<offset>]". When the offset is omitted the
// sub-range starts at prevEnd.
func parseByteRange(s string, prevEnd int64) (ByteRange, error) {
	s = strings.TrimSpace(s)
	lenStr, offStr, hasOff := strings.Cut(s, "@")
	n, err := strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil || n <= 0 {
		return ByteRange{}, fmt.Errorf("hls: bad BYTERANGE %q", s)
	}
	off := prevEnd
	if hasOff {
		off, err = strconv.ParseInt(strings.TrimSpace(offStr), 10, 64)
		if err != nil || off < 0 {
			return ByteRange{}, fmt.Errorf("hls: bad BYTERANGE %q", s)
		}
	}
	return ByteRange{Offset: off, Length: n}, nil
}

// HLSBestVariant returns the variant with the highest resolution, using
// bandwidth as the tie-breaker (and as the only key when resolution is absent).
func HLSBestVariant(variants []HLSVariant) HLSVariant {
	return HLSPickVariant(variants, 0)
}

// HLSPickVariant returns the best variant whose height is <= maxHeight
// (0 = no limit). If every variant exceeds maxHeight, the smallest is returned.
func HLSPickVariant(variants []HLSVariant, maxHeight int) HLSVariant {
	if len(variants) == 0 {
		return HLSVariant{}
	}
	better := func(a, b HLSVariant) bool { // is a better than b?
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		return a.Bandwidth > b.Bandwidth
	}
	var best *HLSVariant
	for i := range variants {
		v := &variants[i]
		if maxHeight > 0 && v.Height > maxHeight {
			continue
		}
		if best == nil || better(*v, *best) {
			best = v
		}
	}
	if best != nil {
		return *best
	}
	// Nothing fits the cap — take the lowest available.
	low := variants[0]
	for _, v := range variants[1:] {
		if better(low, v) {
			low = v
		}
	}
	return low
}

// parseHLSAttrs parses a comma-separated HLS attribute list into a map.
// Handles quoted values like CODECS="avc1.42E01E,mp4a.40.2".
func parseHLSAttrs(s string) map[string]string {
	attrs := make(map[string]string)
	for s != "" {
		s = strings.TrimSpace(s)
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToUpper(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val = s[1:]
				s = ""
			} else {
				val = s[1 : end+1]
				s = s[end+2:]
				s = strings.TrimPrefix(strings.TrimSpace(s), ",")
			}
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				val = s
				s = ""
			} else {
				val = s[:comma]
				s = s[comma+1:]
			}
		}
		attrs[key] = strings.TrimSpace(val)
	}
	return attrs
}

// newLineScanner returns a line scanner with a buffer large enough for very
// long playlist lines (signed URLs can exceed bufio's 64 KB default).
func newLineScanner(body string) *bufio.Scanner {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}

// resolveURL resolves ref relative to base. If ref is absolute, it is returned as-is.
func resolveURL(base, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "data:") {
		return ref, nil
	}
	if base == "" {
		return ref, nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return ref, nil
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return u.ResolveReference(r).String(), nil
}
