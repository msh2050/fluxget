package stream

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── HLS ───────────────────────────────────────────────────────────────────────

func runHLS(ctx context.Context, pub Publisher, id string, req Request) error {
	start := time.Now()
	body, err := fetchText(ctx, req.URL, req.Headers)
	if err != nil {
		return fmt.Errorf("fetch HLS manifest: %w", err)
	}
	if !strings.Contains(body, "#EXTM3U") {
		return fmt.Errorf("URL did not return an HLS playlist \u2014 it may have expired")
	}

	mediaURL, mediaBody := req.URL, body
	var audioRend *HLSRendition
	var subtitles []HLSSubtitle

	variants, err := ParseHLSMaster(body, req.URL)
	if err != nil {
		return err
	}
	renditions := ParseHLSRenditions(body, req.URL)
	if len(variants) > 0 {
		if subtitlesEnabled() && !req.AudioOnly {
			subtitles = ParseHLSSubtitles(body, req.URL)
		}
		v := HLSPickVariant(variants, req.MaxHeight)
		audioRend = HLSAudioFor(v, renditions)
		mediaURL = v.URL
		debugLog.Printf("[%s] HLS variant %dx%d %d kbps (cap %dp) audio-group=%q \u2192 %s",
			short(id), v.Width, v.Height, v.Bandwidth/1000, req.MaxHeight, v.AudioGroup, redactURL(mediaURL))

		if req.AudioOnly && audioRend != nil {
			// Separate audio rendition — no need to touch the video at all.
			mediaURL, audioRend = audioRend.URI, nil
		}
		if mediaBody, err = fetchText(ctx, mediaURL, req.Headers); err != nil {
			return fmt.Errorf("fetch HLS media playlist: %w", err)
		}
	}

	segs, info, err := ParseHLSMediaInfo(mediaBody, mediaURL)
	if err != nil {
		return err
	}
	if info.Live {
		return fmt.Errorf("%w (HLS playlist has no #EXT-X-ENDLIST)", errLiveStream)
	}
	if len(segs) == 0 {
		return fmt.Errorf("HLS playlist has no segments")
	}

	wd, err := newWorkDir(id, req.DestDir)
	if err != nil {
		return err
	}
	defer wd.close(ctx)

	prog := newProgress(pub, id, start)
	sf := newSegmentFetcher(id, prog, req.Headers)

	// Audio and subtitles are fetched alongside the video.
	type trackResult struct {
		path string
		err  error
	}
	audioCh := make(chan trackResult, 1)
	if audioRend != nil {
		go func() {
			p, err := hlsAudioTrack(ctx, sf, *audioRend, req.Headers, wd)
			audioCh <- trackResult{p, err}
		}()
	} else {
		audioCh <- trackResult{}
	}
	subsCh := make(chan []subTrack, 1)
	go func() { subsCh <- downloadSubtitles(ctx, id, subtitles, req.Headers, wd) }()

	videoPath, err := buildHLSTrack(ctx, sf, segs, wd.join("video"), wd.join("video.bin"))
	if err != nil {
		wd.fail("video")
		return err
	}
	ar := <-audioCh
	if ar.err != nil {
		wd.fail("audio")
		return fmt.Errorf("audio rendition: %w", ar.err)
	}
	subs := <-subsCh
	prog.maybePublish(true)

	out, err := assemble(ctx, assembleSpec{
		id: id, destDir: req.DestDir, base: titleBase(req.Title),
		video: videoPath, audio: ar.path, subs: subs, audioOnly: req.AudioOnly,
	})
	if err != nil {
		wd.fail("mux")
		return err
	}
	return publishComplete(pub, id, req.URL, out, start)
}

// buildHLSTrack downloads (and decrypts) all segments of one media playlist
// and concatenates them, inserting #EXT-X-MAP init segments wherever the map
// changes (fMP4/CMAF needs moov/trex in front of the fragments).
func buildHLSTrack(ctx context.Context, sf *segmentFetcher, segs []HLSSegment, dir, out string) (string, error) {
	var jobs []segJob
	order := make([]int, 0, len(segs)*2) // job indices in output order
	initIdx := map[string]int{}
	prevInit := ""
	for _, s := range segs {
		if s.InitURL != "" && s.InitURL+s.InitRange.Header() != prevInit {
			k := s.InitURL + s.InitRange.Header()
			idx, ok := initIdx[k]
			if !ok {
				idx = len(jobs)
				initIdx[k] = idx
				jobs = append(jobs, segJob{URL: s.InitURL, Range: s.InitRange})
			}
			order = append(order, idx)
			prevInit = k
		}
		order = append(order, len(jobs))
		jobs = append(jobs, segJob{URL: s.URL, Range: s.Range, Key: s.Key})
	}
	// Init segments go first in the job list so the first fetched (and
	// validated) piece is a real media header.
	files, err := sf.fetchAll(ctx, jobs, dir)
	if err != nil {
		return "", err
	}
	paths := make([]string, len(order))
	for i, j := range order {
		paths[i] = files[j]
	}
	if err := catFiles(paths, out); err != nil {
		return "", fmt.Errorf("concat segments: %w", err)
	}
	return out, nil
}

func hlsAudioTrack(ctx context.Context, sf *segmentFetcher, r HLSRendition, headers map[string]string, wd *workDir) (string, error) {
	body, err := fetchText(ctx, r.URI, headers)
	if err != nil {
		return "", err
	}
	segs, _, err := ParseHLSMediaInfo(body, r.URI)
	if err != nil {
		return "", err
	}
	if len(segs) == 0 {
		return "", errors.New("audio playlist has no segments")
	}
	return buildHLSTrack(ctx, sf, segs, wd.join("audio"), wd.join("audio.bin"))
}

// ── Subtitles ─────────────────────────────────────────────────────────────────

func downloadSubtitles(ctx context.Context, id string, subs []HLSSubtitle, headers map[string]string, wd *workDir) []subTrack {
	var tracks []subTrack
	for i, s := range subs {
		vtt, err := downloadSubtitleTrack(ctx, s, headers, wd.path, i)
		if err != nil {
			debugLog.Printf("[%s] subtitle %q (%s) failed: %v", short(id), s.Name, s.Language, err)
			continue
		}
		lang := s.Language
		if lang == "" {
			lang = fmt.Sprintf("s%d", i)
		}
		tracks = append(tracks, subTrack{path: vtt, lang: lang, name: s.Name})
	}
	return tracks
}

// downloadSubtitleTrack fetches a subtitle rendition (an HLS media playlist of
// WebVTT segments, or a direct .vtt/.srt) and writes one WebVTT/SRT file.
func downloadSubtitleTrack(ctx context.Context, sub HLSSubtitle, headers map[string]string, dir string, idx int) (string, error) {
	body, err := fetchText(ctx, sub.URI, headers)
	if err != nil {
		return "", err
	}
	text := body
	if strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") {
		segs, perr := ParseHLSMedia(body, sub.URI)
		if perr != nil || len(segs) == 0 {
			return "", fmt.Errorf("empty subtitle playlist: %v", perr)
		}
		var sb strings.Builder
		for i, s := range segs {
			part, ferr := fetchText(ctx, s.URL, headers)
			if ferr != nil {
				return "", ferr
			}
			if i > 0 {
				part = stripVTTHeader(part)
			}
			sb.WriteString(strings.TrimSpace(part))
			sb.WriteString("\n\n")
		}
		text = sb.String()
	}
	ext := ".vtt"
	if !strings.HasPrefix(strings.TrimSpace(text), "WEBVTT") && isSRT(text) {
		ext = ".srt"
	}
	dest := filepath.Join(dir, fmt.Sprintf("sub%d%s", idx, ext))
	return dest, writeFile(dest, []byte(text))
}

// stripVTTHeader removes the "WEBVTT" header block from a continuation
// segment so the concatenated file has exactly one header.
func stripVTTHeader(s string) string {
	t := strings.TrimLeft(s, "\uFEFF \r\n")
	if !strings.HasPrefix(t, "WEBVTT") {
		return s
	}
	if i := strings.Index(t, "\n\n"); i >= 0 {
		return t[i+2:]
	}
	if i := strings.Index(t, "\r\n\r\n"); i >= 0 {
		return t[i+4:]
	}
	return ""
}

func isSRT(s string) bool {
	return strings.Contains(s, "-->") && strings.Contains(s, ",")
}

// runSubtitlesOnly downloads only the subtitle tracks (.srt), no video. The
// URL may be an HLS master (subtitles read from #EXT-X-MEDIA), a subtitle
// media playlist, or a direct .vtt/.srt.
func runSubtitlesOnly(ctx context.Context, pub Publisher, id string, req Request) error {
	start := time.Now()
	body, err := fetchText(ctx, req.URL, req.Headers)
	if err != nil {
		return fmt.Errorf("fetch subtitle source: %w", err)
	}
	trimmed := strings.TrimSpace(body)
	var subs []HLSSubtitle
	switch {
	case strings.Contains(body, "#EXT-X-STREAM-INF") || strings.Contains(body, "#EXT-X-MEDIA"):
		if subs = ParseHLSSubtitles(body, req.URL); len(subs) == 0 {
			return fmt.Errorf("this stream has no subtitle tracks")
		}
	case strings.HasPrefix(trimmed, "WEBVTT"):
		subs = []HLSSubtitle{{URI: req.URL}}
	case strings.HasPrefix(trimmed, "#EXTM3U"):
		segs, _ := ParseHLSMedia(body, req.URL)
		if len(segs) == 0 || !isVTTURL(segs[0].URL) {
			return fmt.Errorf("%s is a video playlist with no subtitles \u2014 the page's master playlist is required", redactURL(req.URL))
		}
		subs = []HLSSubtitle{{URI: req.URL}}
	default:
		subs = []HLSSubtitle{{URI: req.URL}}
	}

	wd, err := newWorkDir(id, req.DestDir)
	if err != nil {
		return err
	}
	defer wd.close(ctx)

	ffmpeg, _ := findFFmpeg()
	base := titleBase(req.Title)
	saved, last := 0, ""
	for i, s := range subs {
		src, derr := downloadSubtitleTrack(ctx, s, req.Headers, wd.path, i)
		if derr != nil {
			debugLog.Printf("[%s] subtitle %q failed: %v", short(id), s.Language, derr)
			continue
		}
		suffix := ""
		if s.Language != "" {
			suffix = "." + sanitizeFilename(s.Language)
		} else if len(subs) > 1 {
			suffix = fmt.Sprintf(".s%d", i)
		}
		out := outputPath(req.DestDir, base+suffix, ".srt")
		converted := false
		if ffmpeg != "" {
			if o, cerr := exec.CommandContext(ctx, ffmpeg, "-y", "-hide_banner", "-loglevel", "error", "-i", src, out).CombinedOutput(); cerr == nil {
				converted = true
			} else {
				debugLog.Printf("[%s] vtt\u2192srt failed: %v\n%s", short(id), cerr, o)
			}
		}
		if !converted {
			out = outputPath(req.DestDir, base+suffix, filepath.Ext(src))
			if copyFile(src, out, 0o644) != nil {
				continue
			}
		}
		saved++
		last = out
	}
	if saved == 0 {
		return fmt.Errorf("no subtitle tracks could be downloaded")
	}
	return publishComplete(pub, id, req.URL, last, start)
}

func isVTTURL(u string) bool {
	u = strings.ToLower(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return strings.HasSuffix(u, ".vtt") || strings.HasSuffix(u, ".srt") || strings.HasSuffix(u, ".webvtt")
}
