package stream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msh2050/fluxget/internal/types"
)

type nopPub struct {
	mu     sync.Mutex
	events []types.DownloadEvent
}

func (p *nopPub) Publish(m types.DownloadEvent) error {
	p.mu.Lock()
	p.events = append(p.events, m)
	p.mu.Unlock()
	return nil
}

// ── DASH ──────────────────────────────────────────────────────────────────────

func TestExpandTemplate(t *testing.T) {
	cases := []struct{ tmpl, want string }{
		{"seg-$Number$.m4s", "seg-42.m4s"},
		{"seg-$Number%05d$.m4s", "seg-00042.m4s"},
		{"$RepresentationID$/$Time$.m4s", "v1/9000.m4s"},
		{"$RepresentationID$/t$Time%010d$.m4s", "v1/t0000009000.m4s"},
		{"b$Bandwidth$-$$-$Number$", "b800000-$-42"},
	}
	for _, c := range cases {
		if got := expandTemplate(c.tmpl, "v1", 800000, 42, 9000); got != c.want {
			t.Errorf("expandTemplate(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

func TestParseISODuration(t *testing.T) {
	cases := map[string]float64{
		"PT1H2M3.5S": 3723.5,
		"PT634.566S": 634.566,
		"P1DT1S":     86401,
		"PT0S":       0,
		"garbage":    0,
	}
	for in, want := range cases {
		if got := parseISODuration(in); got != want {
			t.Errorf("parseISODuration(%q) = %v, want %v", in, got, want)
		}
	}
}

const fixedDurMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT20.5S">
 <Period>
  <AdaptationSet mimeType="video/mp4">
   <SegmentTemplate initialization="$RepresentationID$/init.mp4" media="$RepresentationID$/seg-$Number%03d$.m4s" startNumber="1" timescale="1000" duration="4000"/>
   <Representation id="720" bandwidth="2000000" width="1280" height="720"/>
   <Representation id="1080" bandwidth="4000000" width="1920" height="1080"/>
  </AdaptationSet>
  <AdaptationSet mimeType="audio/mp4" lang="en">
   <SegmentTemplate initialization="a/init.mp4" media="a/$Number$.m4s" timescale="1000" duration="4000"/>
   <Representation id="a" bandwidth="128000"/>
  </AdaptationSet>
  <AdaptationSet contentType="text" mimeType="application/mp4" codecs="stpp">
   <SegmentTemplate media="t/$Number$.m4s" timescale="1000" duration="4000"/>
   <Representation id="sub" bandwidth="9000000"/>
  </AdaptationSet>
 </Period>
</MPD>`

func TestParseMPDFixedDuration(t *testing.T) {
	streams, err := ParseMPD(fixedDurMPD, "https://cdn.example/v/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	v := DASHBestVideo(streams)
	if v == nil || v.Height != 1080 {
		t.Fatalf("best video = %+v", v)
	}
	// 20.5s / 4s = 5.125 → 6 segments (not the old 50000 placeholders)
	if len(v.SegURLs) != 6 {
		t.Fatalf("got %d segments, want 6", len(v.SegURLs))
	}
	if v.SegURLs[0] != "https://cdn.example/v/1080/seg-001.m4s" || v.InitURL != "https://cdn.example/v/1080/init.mp4" {
		t.Errorf("urls: init=%s seg0=%s", v.InitURL, v.SegURLs[0])
	}
	if capped := DASHPickVideo(streams, 720); capped == nil || capped.Height != 720 {
		t.Errorf("DASHPickVideo(720) = %+v", capped)
	}
	if a := DASHBestAudio(streams); a == nil || a.Lang != "en" {
		t.Errorf("audio = %+v", a)
	}
	for _, s := range streams {
		if s.Bandwidth == 9000000 && s.Type != "text" {
			t.Errorf("subtitle adaptation set classified as %q", s.Type)
		}
	}
}

func TestParseMPDTimelineRepeatToEnd(t *testing.T) {
	mpdBody := `<MPD type="static" mediaPresentationDuration="PT10S"><Period>
 <AdaptationSet mimeType="video/mp4">
  <SegmentTemplate timescale="10" media="s-$Time$.m4s" initialization="i.mp4">
   <SegmentTimeline><S t="0" d="20" r="-1"/></SegmentTimeline>
  </SegmentTemplate>
  <Representation id="v" bandwidth="1" height="360"/>
 </AdaptationSet></Period></MPD>`
	streams, err := ParseMPD(mpdBody, "https://h/x.mpd")
	if err != nil {
		t.Fatal(err)
	}
	segs := streams[0].SegURLs
	if len(segs) != 5 || !strings.HasSuffix(segs[4], "s-80.m4s") {
		t.Fatalf("segments = %v", segs)
	}
}

func TestParseMPDSegmentListAndDRM(t *testing.T) {
	list := `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4">
 <Representation id="v" bandwidth="1" height="480"><BaseURL>video.mp4</BaseURL>
  <SegmentList><Initialization range="0-99"/><SegmentURL mediaRange="100-199"/><SegmentURL mediaRange="200-299"/></SegmentList>
 </Representation></AdaptationSet></Period></MPD>`
	streams, err := ParseMPD(list, "https://h/dir/m.mpd")
	if err != nil {
		t.Fatal(err)
	}
	s := streams[0]
	if s.InitURL != "https://h/dir/video.mp4" || s.InitRange.Header() != "bytes=0-99" || len(s.Segments) != 2 || s.Segments[1].Range.Header() != "bytes=200-299" {
		t.Fatalf("segment list parsed wrong: %+v", s)
	}

	drm := `<MPD type="static"><Period><AdaptationSet mimeType="video/mp4">
 <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/>
 <SegmentTemplate media="$Number$.m4s" duration="1" timescale="1"/>
 <Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`
	if _, err := ParseMPD(drm, "https://h/m.mpd"); !errors.Is(err, ErrDRM) {
		t.Fatalf("want ErrDRM, got %v", err)
	}
	if _, err := ParseMPD(`<MPD type="dynamic"><Period/></MPD>`, ""); !errors.Is(err, errLiveStream) {
		t.Fatalf("want errLiveStream, got %v", err)
	}
}

// ── HLS ───────────────────────────────────────────────────────────────────────

const masterWithAudio = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Arabic",LANGUAGE="ar",URI="audio/ar.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="subs/en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e",AUDIO="aud"
v360.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS="avc1.640028",AUDIO="aud"
v1080.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,AUDIO="aud"
v720.m3u8
`

func TestHLSMasterAudioGroups(t *testing.T) {
	variants, err := ParseHLSMaster(masterWithAudio, "https://h/p/master.m3u8")
	if err != nil || len(variants) != 3 {
		t.Fatalf("variants=%v err=%v", variants, err)
	}
	if best := HLSBestVariant(variants); best.Height != 1080 {
		t.Errorf("best = %dp", best.Height)
	}
	v := HLSPickVariant(variants, 720)
	if v.Height != 720 {
		t.Errorf("pick(720) = %dp", v.Height)
	}
	a := HLSAudioFor(v, ParseHLSRenditions(masterWithAudio, "https://h/p/master.m3u8"))
	if a == nil || a.URI != "https://h/p/audio/en.m3u8" {
		t.Fatalf("audio rendition = %+v", a)
	}
	if subs := ParseHLSSubtitles(masterWithAudio, "https://h/p/master.m3u8"); len(subs) != 1 || subs[0].Language != "en" {
		t.Errorf("subs = %+v", subs)
	}
}

func TestHLSMediaByteRangeKeysAndLive(t *testing.T) {
	body := `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-MAP:URI="main.mp4",BYTERANGE="700@0"
#EXT-X-KEY:METHOD=AES-128,URI="k.bin"
#EXTINF:4,
#EXT-X-BYTERANGE:1000@700
main.mp4
#EXTINF:4,
#EXT-X-BYTERANGE:500
main.mp4
#EXT-X-ENDLIST
`
	segs, info, err := ParseHLSMediaInfo(body, "https://h/x/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if info.Live || len(segs) != 2 {
		t.Fatalf("live=%v segs=%d", info.Live, len(segs))
	}
	if segs[0].Range.Header() != "bytes=700-1699" || segs[1].Range.Header() != "bytes=1700-2199" {
		t.Errorf("ranges: %q %q", segs[0].Range.Header(), segs[1].Range.Header())
	}
	if segs[0].InitRange.Header() != "bytes=0-699" {
		t.Errorf("init range %q", segs[0].InitRange.Header())
	}
	if segs[1].Key == nil || segs[1].Key.IV[15] != 8 {
		t.Errorf("IV must derive from media sequence (8), got %v", segs[1].Key)
	}

	if _, _, err := ParseHLSMediaInfo("#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://x\"\n#EXTINF:1,\na.ts\n", ""); !errors.Is(err, ErrDRM) {
		t.Errorf("SAMPLE-AES should be ErrDRM, got %v", err)
	}
	_, live, _ := ParseHLSMediaInfo("#EXTM3U\n#EXTINF:1,\na.ts\n", "")
	if !live.Live {
		t.Error("playlist without ENDLIST should be live")
	}
}

// ── AES ───────────────────────────────────────────────────────────────────────

func TestDecryptAES128PerSegmentPadding(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 16)
	iv := make([]byte, 16)
	iv[15] = 3
	plain := bytes.Repeat([]byte{0x47, 1, 2, 3}, 47) // 188-byte TS packet
	// PKCS#7 pad + encrypt, as an HLS packager does for every segment.
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	got, err := decryptAES128(ct, key, iv)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("decrypt mismatch: err=%v len=%d", err, len(got))
	}
}

// ── Ranged / segment downloads ────────────────────────────────────────────────

func rangeServer(t *testing.T, payload []byte, ranges bool, failEvery int) *httptest.Server {
	var n atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failEvery > 0 && n.Add(1)%int64(failEvery) == 0 {
			// Drop the connection mid-body to exercise resume-on-retry.
			w.Header().Set("Content-Length", "1000000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:10])
			return
		}
		if !ranges {
			http.ServeContent(w, &http.Request{Method: r.Method, Header: http.Header{}}, "", time.Time{}, bytes.NewReader(payload))
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	}))
}

func TestDownloadHTTPFileRangedAndPlain(t *testing.T) {
	payload := make([]byte, 5<<20+123)
	_, _ = rand.Read(payload)
	for _, tc := range []struct {
		name      string
		ranges    bool
		failEvery int
	}{{"ranged", true, 0}, {"plain", false, 0}, {"ranged-flaky", true, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rangeServer(t, payload, tc.ranges, tc.failEvery)
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "out.bin")
			prog := newProgress(&nopPub{}, "test", time.Now())
			if err := downloadHTTPFile(context.Background(), prog, srv.URL+"/f", dest, nil); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(dest)
			if !bytes.Equal(got, payload) {
				t.Fatalf("content mismatch: got %d bytes want %d", len(got), len(payload))
			}
		})
	}
}

func TestSegmentFetcherFailsOnMissingMiddleSegment(t *testing.T) {
	ts := bytes.Repeat([]byte{0x47}, 188)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/s5.ts" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(ts)
	}))
	defer srv.Close()
	var jobs []segJob
	for i := 0; i < 10; i++ {
		jobs = append(jobs, segJob{URL: srv.URL + "/s" + strconv.Itoa(i) + ".ts"})
	}
	sf := newSegmentFetcher("t", newProgress(&nopPub{}, "t", time.Now()), nil)
	_, err := sf.fetchAll(context.Background(), jobs, t.TempDir())
	if statusCode(err) != http.StatusNotFound {
		t.Fatalf("a missing middle segment must fail the download (old engine silently dropped it); got %v", err)
	}
}

func TestSegmentFetcherRejectsErrorPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "<!DOCTYPE html><html>login</html>")
	}))
	defer srv.Close()
	sf := newSegmentFetcher("t", newProgress(&nopPub{}, "t", time.Now()), nil)
	_, err := sf.fetchAll(context.Background(), []segJob{{URL: srv.URL + "/a.ts"}, {URL: srv.URL + "/b.ts"}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "web page") {
		t.Fatalf("want web-page error, got %v", err)
	}
}

// ── Naming ────────────────────────────────────────────────────────────────────

func TestSanitizeFilenameByteLimit(t *testing.T) {
	arabic := strings.Repeat("\u0641\u064A\u062F\u064A\u0648 ", 80) // ~880 bytes
	got := sanitizeFilename(arabic + "/x:y")
	if len(got) > maxNameBytes {
		t.Fatalf("name is %d bytes, exceeds NAME_MAX budget", len(got))
	}
	if strings.ContainsAny(got, "/\\:") {
		t.Fatalf("unsafe chars kept: %q", got)
	}
	if sanitizeFilename("  ..  ") != "video" {
		t.Error("empty-ish title should become 'video'")
	}
	if titleBase("clip.mp4") != "clip" {
		t.Error("known media extension should be stripped from titles")
	}
}

func TestPickFormatsHonoursHeightAndFamily(t *testing.T) {
	fs := []Format{
		{MimeType: `video/mp4; codecs="avc1"`, Height: 1080, Bitrate: 4e6, URL: "v1080"},
		{MimeType: `video/webm; codecs="vp9"`, Height: 2160, Bitrate: 9e6, URL: "v2160"},
		{MimeType: `video/mp4; codecs="avc1"`, Height: 720, Bitrate: 2e6, URL: "v720"},
		{MimeType: `audio/webm; codecs="opus"`, Bitrate: 160e3, URL: "opus"},
		{MimeType: `audio/mp4; codecs="mp4a"`, Bitrate: 128e3, URL: "aac"},
	}
	v, a, _ := pickFormats(fs, 1080, false)
	if v.URL != "v1080" || a.URL != "aac" {
		t.Fatalf("pick(1080) = %s + %s, want v1080 + aac (mp4 family)", v.URL, a.URL)
	}
	v, a, _ = pickFormats(fs, 0, false)
	if v.URL != "v2160" || a.URL != "opus" {
		t.Fatalf("pick(best) = %s + %s, want v2160 + opus (webm family)", v.URL, a.URL)
	}
}

func TestDirectName(t *testing.T) {
	cases := []struct{ url, title, ext, base string }{
		{"https://h/a/blob.bin", "blob.bin", ".bin", "blob.bin"[:4]},
		{"https://h/v.mp4?sig=1", "My Clip", ".mp4", "My Clip"},
		{"https://h/stream?id=9", "Show.mkv", ".mkv", "Show"},
		{"https://h/stream?id=9", "Untitled page", ".mp4", "Untitled page"},
	}
	for _, c := range cases {
		ext, base := directName(c.url, c.title)
		if ext != c.ext || base != c.base {
			t.Errorf("directName(%q,%q) = %q,%q want %q,%q", c.url, c.title, ext, base, c.ext, c.base)
		}
	}
}
