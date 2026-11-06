package stream

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// subTrack is a downloaded subtitle file ready to embed.
type subTrack struct {
	path, lang, name string
}

// assembleSpec describes the final output of a download.
type assembleSpec struct {
	id        string
	destDir   string
	base      string     // sanitized file base name (no extension)
	video     string     // primary input (video, or audio for audio-only); required
	audio     string     // separate audio track ("" when muxed into video)
	subs      []subTrack // optional soft subtitles
	audioOnly bool
}

func findFFmpeg() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("ffmpeg not found \u2014 install ffmpeg to merge audio/video (e.g. sudo apt install ffmpeg)")
}

// assemble remuxes the inputs (no re-encoding) into the final file and
// returns its path. The container is .mp4 (.m4a for audio-only), .mkv when
// subtitles are embedded or when MP4 cannot hold the codecs.
func assemble(ctx context.Context, sp assembleSpec) (string, error) {
	ffmpeg, ffErr := findFFmpeg()
	if ffErr != nil {
		if sp.audio != "" {
			return "", ffErr
		}
		// Without ffmpeg a single track can still be saved as-is.
		ext := ".ts"
		if isFMP4File(sp.video) {
			ext = ".mp4"
		}
		out := outputPath(sp.destDir, sp.base, ext)
		if err := moveFile(sp.video, out); err != nil {
			return "", err
		}
		writeSidecars(ctx, "", sp.subs, strings.TrimSuffix(out, ext))
		return out, nil
	}

	containers := []string{".mp4", ".mkv"}
	switch {
	case sp.audioOnly:
		containers = []string{".m4a", ".mka"}
	case len(sp.subs) > 0:
		containers = []string{".mkv"}
	}

	var lastErr error
	for _, ext := range containers {
		out := outputPath(sp.destDir, sp.base, ext)
		args := muxArgs(sp, ext, out)
		cmd := exec.CommandContext(ctx, ffmpeg, args...)
		output, err := cmd.CombinedOutput()
		if err == nil {
			debugLog.Printf("[%s] ffmpeg %s OK \u2192 %s", short(sp.id), ext, out)
			writeSidecars(ctx, ffmpeg, sp.subs, strings.TrimSuffix(out, ext))
			return out, nil
		}
		_ = os.Remove(out)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = fmt.Errorf("ffmpeg: %w: %s", err, lastLines(string(output), 3))
		debugLog.Printf("[%s] ffmpeg %s failed: %v\n%s", short(sp.id), ext, err, output)
	}
	return "", lastErr
}

func muxArgs(sp assembleSpec, ext, out string) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin",
		// Tolerate the odd broken packet at segment boundaries.
		"-fflags", "+genpts+discardcorrupt", "-i", sp.video}
	if sp.audio != "" {
		args = append(args, "-i", sp.audio)
	}
	for _, s := range sp.subs {
		args = append(args, "-i", s.path)
	}

	switch {
	case sp.audioOnly:
		args = append(args, "-map", "0:a:0")
	case sp.audio != "":
		args = append(args, "-map", "0:v:0?", "-map", "1:a:0")
	default:
		// Explicit v/a maps drop timed-ID3/data streams MP4 can't hold.
		args = append(args, "-map", "0:v?", "-map", "0:a?")
	}
	subBase := 1
	if sp.audio != "" {
		subBase = 2
	}
	for i := range sp.subs {
		args = append(args, "-map", fmt.Sprintf("%d:0", subBase+i))
	}

	args = append(args, "-c", "copy")
	if len(sp.subs) > 0 {
		args = append(args, "-c:s", "srt")
		for i, t := range sp.subs {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+iso639_2(t.lang))
			if t.name != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "title="+t.name)
			}
		}
	}
	if ext == ".mp4" || ext == ".m4a" {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, out)
}

// writeSidecars writes one .srt (or raw .vtt without ffmpeg) per subtitle
// track next to the video: "<base>.<lang>.srt".
func writeSidecars(ctx context.Context, ffmpeg string, subs []subTrack, base string) {
	for _, t := range subs {
		suffix := ""
		if t.lang != "" {
			suffix = "." + sanitizeFilename(t.lang)
		}
		if ffmpeg != "" {
			side := base + suffix + ".srt"
			if out, err := exec.CommandContext(ctx, ffmpeg, "-y", "-hide_banner", "-loglevel", "error", "-i", t.path, side).CombinedOutput(); err == nil {
				continue
			} else {
				debugLog.Printf("srt sidecar (%s) failed: %v\n%s", t.lang, err, out)
			}
		}
		_ = copyFile(t.path, base+suffix+".vtt", 0o644)
	}
}

// moveFile renames, falling back to copy+delete across filesystems (the temp
// dir is often tmpfs while Downloads is on disk).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyFile(src, dst, 0o644); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// iso639_2 maps a BCP-47/ISO-639-1 code to the ISO-639-2 code Matroska
// expects ("ar" → "ara"). Unknown codes pass through.
func iso639_2(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(code, "-_"); i > 0 {
		code = code[:i]
	}
	m := map[string]string{
		"ar": "ara", "en": "eng", "fa": "per", "tr": "tur", "ru": "rus",
		"fr": "fre", "de": "ger", "es": "spa", "it": "ita", "ja": "jpn",
		"ko": "kor", "zh": "chi", "pt": "por", "nl": "dut", "hi": "hin",
		"id": "ind", "th": "tha", "vi": "vie", "pl": "pol", "uk": "ukr",
		"sv": "swe", "no": "nor", "da": "dan", "fi": "fin", "cs": "cze",
		"el": "gre", "he": "heb", "ro": "rum", "hu": "hun", "ur": "urd",
		"bn": "ben", "ms": "may", "ku": "kur", "ta": "tam",
	}
	if v, ok := m[code]; ok {
		return v
	}
	if len(code) == 3 {
		return code
	}
	return "und"
}
