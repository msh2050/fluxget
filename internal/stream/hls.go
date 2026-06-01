// Package stream provides native HLS/DASH parsing and segment downloading,
// eliminating the yt-dlp dependency for direct manifest URLs.
package stream

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// HLSVariant is one quality level from an HLS master playlist.
type HLSVariant struct {
	URL       string
	Bandwidth int
	Width     int
	Height    int
}

// HLSKey holds AES-128 encryption info from #EXT-X-KEY.
type HLSKey struct {
	URI string // URL to fetch the 16-byte AES-128 key
	IV  []byte // 16-byte IV; nil means derive from sequence number
}

// HLSSegment is one media segment from an HLS media playlist.
type HLSSegment struct {
	URL      string
	Duration float64
	Key      *HLSKey // nil = unencrypted
	SeqNum   int     // segment sequence number (for IV derivation)
	InitURL  string  // #EXT-X-MAP initialization segment (fMP4); "" for MPEG-TS
}

// ParseHLSMaster parses an HLS master playlist and returns all variants.
// Returns an empty slice (not an error) if body is a media playlist.
func ParseHLSMaster(body, baseURL string) ([]HLSVariant, error) {
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		return nil, nil
	}
	var (
		variants      []HLSVariant
		pendingBW     int
		pendingWidth  int
		pendingHeight int
	)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attrs := parseHLSAttrs(line[len("#EXT-X-STREAM-INF:"):])
			pendingBW, _ = strconv.Atoi(attrs["BANDWIDTH"])
			if res, ok := attrs["RESOLUTION"]; ok {
				parts := strings.SplitN(res, "x", 2)
				if len(parts) == 2 {
					pendingWidth, _ = strconv.Atoi(parts[0])
					pendingHeight, _ = strconv.Atoi(parts[1])
				}
			}
		} else if !strings.HasPrefix(line, "#") && line != "" && pendingBW > 0 {
			u, err := resolveURL(baseURL, line)
			if err != nil {
				return nil, fmt.Errorf("hls: bad segment URL %q: %w", line, err)
			}
			variants = append(variants, HLSVariant{
				URL:       u,
				Bandwidth: pendingBW,
				Width:     pendingWidth,
				Height:    pendingHeight,
			})
			pendingBW, pendingWidth, pendingHeight = 0, 0, 0
		}
	}
	return variants, nil
}

// ParseHLSMedia parses an HLS media playlist and returns all segments in order.
// It handles #EXT-X-KEY for AES-128 encryption and tracks sequence numbers.
func ParseHLSMedia(body, baseURL string) ([]HLSSegment, error) {
	var (
		segments    []HLSSegment
		pendingDur  float64
		currentKey  *HLSKey
		currentInit string // #EXT-X-MAP init segment (fMP4); applies to following segments
		seqNum      int
	)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			seqNum, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			attrs := parseHLSAttrs(line[len("#EXT-X-MAP:"):])
			if uri := attrs["URI"]; uri != "" {
				currentInit, _ = resolveURL(baseURL, uri)
			}
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			attrs := parseHLSAttrs(line[len("#EXT-X-KEY:"):])
			switch method := attrs["METHOD"]; method {
			case "NONE":
				currentKey = nil
			case "AES-128":
				keyURI, _ := resolveURL(baseURL, attrs["URI"])
				var iv []byte
				if ivStr := attrs["IV"]; ivStr != "" {
					ivStr = strings.TrimPrefix(ivStr, "0x")
					ivStr = strings.TrimPrefix(ivStr, "0X")
					iv, _ = hex.DecodeString(ivStr)
				}
				currentKey = &HLSKey{URI: keyURI, IV: iv}
			}
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			raw := line[len("#EXTINF:"):]
			if comma := strings.IndexByte(raw, ','); comma >= 0 {
				raw = raw[:comma]
			}
			pendingDur, _ = strconv.ParseFloat(strings.TrimSpace(raw), 64)
			continue
		}

		if !strings.HasPrefix(line, "#") && line != "" {
			u, err := resolveURL(baseURL, line)
			if err != nil {
				return nil, fmt.Errorf("hls: bad segment URL %q: %w", line, err)
			}
			seg := HLSSegment{URL: u, Duration: pendingDur, SeqNum: seqNum, InitURL: currentInit}
			if currentKey != nil {
				keyCopy := *currentKey
				if keyCopy.IV == nil {
					// Derive IV from sequence number (HLS spec §4.3.2.4)
					iv := make([]byte, 16)
					binary.BigEndian.PutUint64(iv[8:], uint64(seqNum))
					keyCopy.IV = iv
				}
				seg.Key = &keyCopy
			}
			segments = append(segments, seg)
			pendingDur = 0
			seqNum++
		}
	}
	return segments, nil
}

// HLSBestVariant returns the variant with the highest bandwidth.
func HLSBestVariant(variants []HLSVariant) HLSVariant {
	if len(variants) == 0 {
		return HLSVariant{}
	}
	best := variants[0]
	for _, v := range variants[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	return best
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
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			// Quoted value
			close := strings.IndexByte(s[1:], '"')
			if close < 0 {
				val = s[1:]
				s = ""
			} else {
				val = s[1 : close+1]
				s = s[close+2:]
				s = strings.TrimPrefix(s, ",")
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
		attrs[key] = val
	}
	return attrs
}

// resolveURL resolves ref relative to base. If ref is absolute, it is returned as-is.
func resolveURL(base, ref string) (string, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
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
