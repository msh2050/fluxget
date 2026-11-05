package stream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func writeFile(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }

// ── DASH ──────────────────────────────────────────────────────────────────────

func runDASH(ctx context.Context, pub Publisher, id string, req Request) error {
	start := time.Now()
	body, err := fetchText(ctx, req.URL, req.Headers)
	if err != nil {
		return fmt.Errorf("fetch DASH manifest: %w", err)
	}
	streams, err := ParseMPD(body, req.URL)
	if err != nil {
		return err
	}
	var video *DASHStream
	if !req.AudioOnly {
		video = DASHPickVideo(streams, req.MaxHeight)
	}
	audio := DASHBestAudio(streams)
	if video == nil && audio == nil {
		return fmt.Errorf("DASH manifest has no usable audio/video streams")
	}
	if video != nil {
		debugLog.Printf("[%s] DASH video %dx%d %d kbps %s", short(id), video.Width, video.Height, video.Bandwidth/1000, video.Codecs)
	}

	wd, err := newWorkDir(id, req.DestDir)
	if err != nil {
		return err
	}
	defer wd.close(ctx)

	prog := newProgress(pub, id, start)
	sf := newSegmentFetcher(id, prog, req.Headers)

	fetch := func(s *DASHStream, name string) (string, error) {
		out := wd.join(name + ".bin")
		if s.SingleFile != "" {
			return out, downloadHTTPFile(ctx, prog, s.SingleFile, out, req.Headers)
		}
		var jobs []segJob
		if s.InitURL != "" {
			jobs = append(jobs, segJob{URL: s.InitURL, Range: s.InitRange})
		}
		for _, seg := range s.Segments {
			jobs = append(jobs, segJob{URL: seg.URL, Range: seg.Range})
		}
		files, err := sf.fetchAll(ctx, jobs, wd.join(name))
		if err != nil {
			return "", err
		}
		return out, catFiles(files, out)
	}

	// Fetch audio concurrently with video.
	type res struct {
		path string
		err  error
	}
	audioCh := make(chan res, 1)
	if audio != nil && video != nil {
		go func() {
			p, err := fetch(audio, "audio")
			audioCh <- res{p, err}
		}()
	}

	var primary, secondary string
	if video != nil {
		if primary, err = fetch(video, "video"); err != nil {
			wd.fail("video")
			return fmt.Errorf("DASH video: %w", err)
		}
		if audio != nil {
			r := <-audioCh
			if r.err != nil {
				wd.fail("audio")
				return fmt.Errorf("DASH audio: %w", r.err)
			}
			secondary = r.path
		}
	} else {
		if primary, err = fetch(audio, "audio"); err != nil {
			wd.fail("audio")
			return fmt.Errorf("DASH audio: %w", err)
		}
	}

	out, err := assemble(ctx, assembleSpec{
		id: id, destDir: req.DestDir, base: titleBase(req.Title),
		video: primary, audio: secondary, audioOnly: video == nil,
	})
	if err != nil {
		wd.fail("mux")
		return err
	}
	return publishComplete(pub, id, req.URL, out, start)
}

// ── YouTube adaptive (direct CDN URLs from the player response) ───────────────

// pickFormats chooses the video/audio pair to download from the extension's
// format list, honouring MaxHeight and preferring container-compatible pairs
// (H.264+AAC → MP4, VP9/AV1+Opus → WebM/MKV).
func pickFormats(formats []Format, maxHeight int, audioOnly bool) (video, audio *Format, progressive *Format) {
	for i := range formats {
		f := &formats[i]
		switch {
		case f.Progressive && f.URL != "":
			if (maxHeight == 0 || f.Height <= maxHeight) && (progressive == nil || f.Height > progressive.Height) {
				progressive = f
			}
		case f.isVideo() && !audioOnly:
			if maxHeight > 0 && f.Height > maxHeight {
				continue
			}
			if video == nil || f.Height > video.Height || (f.Height == video.Height && f.Bitrate > video.Bitrate) {
				video = f
			}
		}
	}
	family := func(f *Format) string {
		if f == nil {
			return ""
		}
		if strings.Contains(strings.ToLower(f.MimeType), "webm") {
			return "webm"
		}
		return "mp4"
	}
	want := family(video)
	for i := range formats {
		f := &formats[i]
		if !f.isAudio() || f.Progressive {
			continue
		}
		if audio == nil {
			audio = f
			continue
		}
		fMatch, aMatch := family(f) == want, family(audio) == want
		if (fMatch && !aMatch) || (fMatch == aMatch && f.Bitrate > audio.Bitrate) {
			audio = f
		}
	}
	return video, audio, progressive
}

func runAdaptive(ctx context.Context, pub Publisher, id string, req Request) error {
	start := time.Now()
	video, audio, progressive := pickFormats(req.Formats, req.MaxHeight, req.AudioOnly)

	// Signature-ciphered formats carry no URL; only yt-dlp can resolve them.
	if (video != nil && video.URL == "") || (audio != nil && audio.URL == "") {
		if progressive == nil || req.AudioOnly {
			return fmt.Errorf("%w: format URLs are signature-protected", errNeedsYtDlp)
		}
		video, audio = nil, nil
	}
	if video == nil && audio == nil && progressive == nil {
		return fmt.Errorf("%w: no usable formats in adaptive list", errNeedsYtDlp)
	}

	wd, err := newWorkDir(id, req.DestDir)
	if err != nil {
		return err
	}
	defer wd.close(ctx)
	prog := newProgress(pub, id, start)

	var vpath, apath string
	switch {
	case req.AudioOnly && audio != nil:
		vpath = wd.join("audio.bin")
		err = downloadHTTPFile(ctx, prog, audio.URL, vpath, req.Headers)
	case video != nil:
		vpath = wd.join("video.bin")
		if audio != nil {
			apath = wd.join("audio.bin")
		}
		errCh := make(chan error, 1)
		go func() {
			if apath == "" {
				errCh <- nil
				return
			}
			errCh <- downloadHTTPFile(ctx, prog, audio.URL, apath, req.Headers)
		}()
		err = downloadHTTPFile(ctx, prog, video.URL, vpath, req.Headers)
		if aerr := <-errCh; err == nil {
			err = aerr
		}
	default:
		vpath = wd.join("video.bin")
		err = downloadHTTPFile(ctx, prog, progressive.URL, vpath, req.Headers)
	}
	if err != nil {
		wd.fail("adaptive")
		return err
	}

	out, err := assemble(ctx, assembleSpec{
		id: id, destDir: req.DestDir, base: titleBase(req.Title),
		video: vpath, audio: apath, audioOnly: req.AudioOnly,
	})
	if err != nil {
		wd.fail("mux")
		return err
	}
	return publishComplete(pub, id, fallbackURL(req), out, start)
}

// ── Direct media file ─────────────────────────────────────────────────────────

func runDirect(ctx context.Context, pub Publisher, id string, req Request) error {
	start := time.Now()
	ext, base := directName(req.URL, req.Title)
	out := outputPath(req.DestDir, base, ext)
	part := out + ".part"
	prog := newProgress(pub, id, start)
	if err := downloadHTTPFile(ctx, prog, req.URL, part, req.Headers); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, out); err != nil {
		return err
	}
	return publishComplete(pub, id, req.URL, out, start)
}

// directName picks the output extension and base name for a direct file:
// the URL's own extension wins, then the title's, then ".mp4".
func directName(rawURL, title string) (ext, base string) {
	plainExt := func(p string) string {
		e := strings.ToLower(filepath.Ext(p))
		if len(e) < 2 || len(e) > 6 {
			return ""
		}
		for _, c := range e[1:] {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return ""
			}
		}
		return e
	}
	urlPath := strings.SplitN(strings.SplitN(rawURL, "?", 2)[0], "#", 2)[0]
	ext = plainExt(urlPath)
	if ext == "" {
		ext = plainExt(title)
	}
	if ext == "" {
		ext = ".mp4"
	}
	t := title
	if strings.EqualFold(filepath.Ext(t), ext) {
		t = t[:len(t)-len(ext)]
	}
	return ext, titleBase(t)
}
