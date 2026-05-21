"use strict";

// ── Config ────────────────────────────────────────────────────────────────────
const FLUXGET_PORT = 1700;
const BASE_URL     = `http://127.0.0.1:${FLUXGET_PORT}`;

// File extensions FluxGet intercepts from the browser's own downloader.
// Matches IDM's default intercept list from the decompiled IDMNetMon analysis.
const INTERCEPT_EXTS = new Set([
  "zip","rar","7z","gz","tar","bz2","xz","zst","cab","iso","img","dmg",
  "exe","msi","deb","rpm","pkg","apk","appimage",
  "mp4","mkv","avi","mov","flv","wmv","webm","m4v","ts","mpeg","mpg","3gp","ogv",
  "mp3","aac","flac","ogg","opus","wav","m4a","wma",
  "pdf","doc","docx","xls","xlsx","ppt","pptx","odt","ods",
  "jpg","jpeg","png","gif","webp","bmp","tiff","svg",
  "m3u8","mpd",
]);

// MIME types that always go to FluxGet (never let browser handle them)
const INTERCEPT_MIME = /^(video\/|audio\/|application\/(zip|x-rar|x-7z|octet-stream|x-iso9660|vnd\.android|x-deb|x-rpm|x-msdownload|x-apple-diskimage|x-mpegurl|dash\+xml)|image\/(?!webp))/i;

// Video MIME types captured from network (not intercepted as downloads, but shown as button)
const VIDEO_MIME_RE = /^(video|audio)\/|application\/(x-mpegurl|vnd\.apple\.mpegurl|dash\+xml|vnd\.ms-sstr\+xml)/i;
const VIDEO_EXT_RE  = /\.(mp4|webm|mkv|m3u8|mpd|flv|avi|mov|m4v|m4a|mp3|ogg|opus)(\?|#|$)/i;
// HLS/DASH manifest paths that don't use standard extensions (IDMNetMon pattern: ^/playlist/\w+/|^/hls/)
const MANIFEST_PATH_RE = /\/(playlist|master|index|manifest|stream)(\.m3u8|\.mpd)?(\?|$)/i;

// Skip individual adaptive segments — matches IDMNetMon's segment skip logic:
//  • .ts/.m4s/.cmfa/.cmfv/.fmp4 (common HLS/DASH chunk extensions)
//  • YouTube CDN: M4S in /avf/ or /sep/ path + filename starts "segment-"
//  • YouTube CDN: MP4 in /avf/ or /parcel/ path
//  • /api/manifest/init_segment (YouTube DASH init)
const SKIP_FRAG_RE = /\.(ts|m4s|cmfa|cmfv|fmp4)(\?|#|$)/i;

function isYouTubeCDNFragment(url) {
  const u = url.toLowerCase();
  // M4S segments in YouTube's adaptive paths
  if (u.includes(".m4s") && (u.includes("/avf/") || u.includes("/sep/"))) return true;
  // MP4 chunks in adaptive fragmented paths
  const path = u.split("?")[0];
  if (path.endsWith(".mp4") && (u.includes("/avf/") || u.includes("/parcel/"))) return true;
  // YouTube DASH init segment
  if (u.includes("/api/manifest/init_segment")) return true;
  return false;
}

// IDMNetMon site dispatch table (exact order from decompiled IDMNetMon.dll lines 35974-36059):
//  1. application/vnd.lumberjack.manifest  → Lumberjack handler
//  2. MPD extension                         → DASH handler
//  3. youtube.com / youtube-nocookie.com / googlevideo.com /
//     drive.google.com / docs.google.com / youtube.googleapis.com → YouTubeParser
//  4. facebook.com / workplace.com          → FacebookParser
//  5. vimeo.com                             → VimeoParser
//  6. instagram.com                         → InstagramParser
//  7. ok.ru / mycdn.me                      → OKParser
//  8. hydrax.to (/playlist/ or /hls/ path) → HydraxParser
//  9. Generic fallback
//
// CDN domains (direct media — not page-level parsers, use native downloader):
//  fbcdn.net, vimeocdn.com, *-vimeo.akamaized.net, *-adaptive.akamaized.net,
//  cdninstagram.com, vkuser.net, *-hls-media.sndcdn.com

// KNOWN_VIDEO_HOSTS: page-level hosts where we trigger video extraction.
// CDN domains are intentionally excluded — those serve direct URLs, not pages.
const KNOWN_VIDEO_HOSTS = [
  // Google / YouTube
  "youtube.com", "youtu.be", "youtube-nocookie.com",
  "googlevideo.com", "youtube.googleapis.com",
  "drive.google.com", "docs.google.com",
  // Facebook / Meta
  "facebook.com", "workplace.com", "fb.watch",
  // Instagram
  "instagram.com",
  // Vimeo
  "vimeo.com",
  // ok.ru / VK ecosystem
  "ok.ru", "mycdn.me", "vk.com", "vkuser.net",
  // SoundCloud (CDN: *.sndcdn.com)
  "soundcloud.com", "sndcdn.com",
  // Hydrax
  "hydrax.to",
  // Udemy (uses HLS: \.(m3u8|mpd)(\?|$))
  "udemy.com",
  // Twitch (Twitch uses HLS: ^/playlist/\w+/(\d+)|^/hls/\w+/\w+)
  "twitch.tv", "twitch.com",
  // Others commonly intercepted by IDM / yt-dlp
  "dailymotion.com", "tiktok.com",
  "twitter.com", "x.com", "t.co",
  "reddit.com",
  "bilibili.com", "bilivideo.com",
  "nicovideo.jp", "dmc.nico",
  "bandcamp.com",
  "rumble.com",
  "odysee.com",
  "peertube.social",
  "streamable.com",
  "mixcloud.com",
  // Yandex (strm.yandex.ru)
  "yandex.ru", "yandex.com",
];

// ── State ─────────────────────────────────────────────────────────────────────
// tabId → [{ url, type, size, ts, quality }]  captured network videos
const capturedVideos = new Map();

// tabId → { dashUrl, hlsUrl, formats, title }  from document.js
const pageVideoData  = new Map();

// hostname → cookie string captured live from onBeforeSendHeaders.
// Mirrors IDM's approach: intercept the actual Cookie headers the browser sends
// for CDN segment requests (e.g. lh3.googleusercontent.com) as the video plays,
// so FluxGet can use authentic Google/CDN session cookies for downloading.
const capturedDomainCookies = new Map();

// Settings (loaded from chrome.storage.local)
let settings = {
  interceptDownloads: true,  // intercept browser file downloads
  autoDownloadVideo: false,  // auto-send video without prompt
  port: FLUXGET_PORT,
  destDir: "",
};

// ── Quality picker builder (IDM-style) ───────────────────────────────────────
// Builds a deduplicated list of quality options from ytInitialPlayerResponse formats.
// Each entry has: label, height, codec, videoUrl, audioUrl, ytFormat (fallback)
function buildPickerOptions(formats) {
  const videoByHeight = new Map();  // height → best video format
  let bestAudio = null;

  for (const f of formats) {
    const mime = f.mimeType || "";
    if (f.height > 0) {
      const existing = videoByHeight.get(f.height);
      const better = !existing
        || (f.url && !existing.url)               // prefer direct URL
        || (f.url && existing.url && (f.bitrate || 0) > (existing.bitrate || 0));
      if (better) videoByHeight.set(f.height, f);
    } else if (mime.startsWith("audio/")) {
      if (!bestAudio || (f.bitrate || 0) > (bestAudio.bitrate || 0)) bestAudio = f;
    }
  }

  const codec = f => {
    const m = (f.mimeType || "").toLowerCase();
    if (m.includes("mp4") || m.includes("avc") || m.includes("h.264")) return "MP4";
    if (m.includes("webm") || m.includes("vp9") || m.includes("vp8")) return "WebM";
    if (m.includes("av01") || m.includes("av1")) return "AV1";
    return "";
  };

  const ytFmt = (height) =>
    `bestvideo[height<=${height}]+bestaudio/bestvideo[height<=${height}]+bestaudio/best[height<=${height}]`;

  const options = [];
  const sorted = [...videoByHeight.entries()].sort((a, b) => b[0] - a[0]);

  for (const [height, f] of sorted) {
    const label = f.quality || `${height}p`;
    options.push({
      label,
      height,
      codec:    codec(f),
      videoUrl: f.url || "",
      audioUrl: bestAudio?.url || "",
      ytFormat: f.url ? "" : ytFmt(height),
      type:     "video",
    });
  }

  // Audio only
  if (bestAudio) {
    const audioBitrate = bestAudio.bitrate ? Math.round(bestAudio.bitrate / 1000) + "k" : "";
    const audioCodec   = (bestAudio.mimeType || "").toLowerCase().includes("opus") ? "Opus" : "AAC";
    options.push({
      label:    `Audio only${audioBitrate ? " " + audioBitrate : ""}`,
      height:   0,
      codec:    audioCodec,
      videoUrl: "",
      audioUrl: bestAudio.url || "",
      ytFormat: bestAudio.url ? "" : "bestaudio/best",
      type:     "audio",
    });
  }

  return options;
}

// ── Icon / badge ──────────────────────────────────────────────────────────────
function setConnected(ok) {
  chrome.action.setBadgeText({ text: ok ? "" : "OFF" });
  chrome.action.setBadgeBackgroundColor({ color: ok ? "#1a7f3c" : "#c0392b" });
  chrome.action.setTitle({ title: ok ? "FluxGet (connected)" : "FluxGet (not running — start fluxget --port 1700)" });
}

// ── Health check ──────────────────────────────────────────────────────────────
async function ping() {
  try {
    const r = await fetch(`${BASE_URL}/health`, { signal: AbortSignal.timeout(3000) });
    setConnected(r.ok);
    return r.ok;
  } catch (_) {
    setConnected(false);
    return false;
  }
}

// ── Send to FluxGet backend ───────────────────────────────────────────────────
async function apiPost(path, body) {
  try {
    const r = await fetch(`${BASE_URL}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(8000),
    });
    setConnected(r.ok);
    return r.ok ? r.json().catch(() => ({})) : null;
  } catch (_) {
    setConnected(false);
    return null;
  }
}

// Regular file download → /download (multi-connection HTTP engine)
function sendDownload(url, filename, headers) {
  return apiPost("/download", { url, filename: filename || "", headers: headers || {}, skip_approval: true });
}

// getCookiesForURL returns the browser's Cookie header string for a given URL.
// Uses the url parameter (not domain) so parent-domain cookies are included.
async function getCookiesForURL(url) {
  try {
    const cookies = await chrome.cookies.getAll({ url });
    if (cookies.length === 0) return "";
    return cookies.map(c => `${c.name}=${c.value}`).join("; ");
  } catch {
    return "";
  }
}

// sniffHLSSegmentCookies fetches an HLS manifest in the browser's authenticated
// context (credentials:include), follows one level of indirection for master→media
// playlists, discovers the domain(s) hosting actual video segments, and returns
// browser cookies for those domains. This handles CDNs like lh3.googleusercontent.com
// where segments are on a different host than the manifest.
async function sniffHLSSegmentCookies(manifestUrl) {
  const baseHost = new URL(manifestUrl).hostname;
  const seen = new Set([baseHost]);
  const cookieParts = [];

  const collectCookiesFor = async (segUrl) => {
    try {
      const h = new URL(segUrl).hostname;
      if (seen.has(h)) return;
      seen.add(h);
      // Prefer live-captured headers (exact cookies browser sent while playing)
      const live = capturedDomainCookies.get(h);
      if (live) { cookieParts.push(live); return; }
      // Fallback: chrome.cookies API (works for non-third-party cookies)
      const c = await getCookiesForURL(segUrl);
      if (c) cookieParts.push(c);
    } catch {}
  };

  const firstNonCommentURL = (text, base) => {
    for (const line of text.split("\n")) {
      const t = line.trim();
      if (!t || t.startsWith("#")) continue;
      try { return t.startsWith("http") ? t : new URL(t, base).href; } catch {}
    }
    return null;
  };

  try {
    const r = await fetch(manifestUrl, { credentials: "include", cache: "force-cache" });
    if (!r.ok) return "";
    const text = await r.text();
    const firstUrl = firstNonCommentURL(text, manifestUrl);
    if (!firstUrl) return "";

    if (/\.m3u8(\?|#|$)/i.test(firstUrl)) {
      // Master playlist — follow the first variant to find segment domain
      await collectCookiesFor(firstUrl);
      try {
        const vr = await fetch(firstUrl, { credentials: "include", cache: "force-cache" });
        if (vr.ok) {
          const segUrl = firstNonCommentURL(await vr.text(), firstUrl);
          if (segUrl) await collectCookiesFor(segUrl);
        }
      } catch {}
    } else {
      // Already a segment URL
      await collectCookiesFor(firstUrl);
    }
  } catch {}

  return cookieParts.join("; ");
}

// Video / stream → /stream (uses FluxGet's native HLS/DASH engine or yt-dlp fallback)
// Cookie strategy (mirrors IDM's onBeforeSendHeaders approach):
//  1. Live-captured cookies from onBeforeSendHeaders (most accurate — includes
//     partitioned/SameSite cookies the chrome.cookies API misses)
//  2. Fallback: chrome.cookies.getAll({url}) for both manifest and segment domains
// The user must play the video for a few seconds so the browser makes real
// requests to the CDN and we can capture the authenticated Cookie headers.
async function sendStream(url, title, headers, formats) {
  const allHeaders = { ...(headers || {}) };
  const cookieParts = [];

  // Manifest domain cookies — try live capture first, then cookies API
  try {
    const manifestHost = new URL(url).hostname;
    const live = capturedDomainCookies.get(manifestHost);
    if (live) {
      cookieParts.push(live);
    } else {
      const c = await getCookiesForURL(url);
      if (c) cookieParts.push(c);
    }
  } catch {}

  // Segment domain cookies (may differ from manifest domain, e.g. turbosplayer CDN)
  if (isManifestURL(url)) {
    const segCookie = await sniffHLSSegmentCookies(url);
    if (segCookie) cookieParts.push(segCookie);
  }

  if (cookieParts.length > 0) allHeaders.Cookie = cookieParts.join("; ");
  return apiPost("/stream", { url, title: title || "", headers: allHeaders, formats: formats || [] });
}

// ── Notification helper ───────────────────────────────────────────────────────
function notify(title, message) {
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/icon48.png",
    title,
    message,
  });
}

// ── chrome.downloads interception (THE KEY IDM FEATURE) ──────────────────────
// When the browser starts any download, we cancel it immediately and route
// to FluxGet. This gives us multi-connection speed + resume capability.
chrome.downloads.onCreated.addListener((item) => {
  if (!settings.interceptDownloads) return;

  // Skip downloads that look internal / extension-initiated
  if (!item.url || item.url.startsWith("blob:") || item.url.startsWith("data:")) return;
  if (!item.url.startsWith("http")) return;

  // Decide whether to intercept based on MIME or file extension
  const mime = (item.mime || "").toLowerCase();
  const ext  = (item.filename || item.url).split("?")[0].split(".").pop().toLowerCase();

  const shouldIntercept = INTERCEPT_MIME.test(mime) || INTERCEPT_EXTS.has(ext);
  if (!shouldIntercept) return;

  const filename = item.filename
    ? item.filename.split(/[\\/]/).pop()
    : item.url.split("/").pop().split("?")[0] || "download";

  // Cancel the browser download immediately
  chrome.downloads.cancel(item.id, () => {
    chrome.downloads.erase({ id: item.id });
  });

  // Build Referer header — CDNs return 403 without it.
  // chrome.downloads gives us item.referrer (the page URL) directly.
  // Fall back to the captured webRequest referer if item.referrer is empty.
  const tabId = item.tabId >= 0 ? item.tabId : null;
  const captured = tabId != null ? (capturedVideos.get(tabId) || []) : [];
  const capEntry = captured.find(v => v.url === item.url);
  const referer  = item.referrer || capEntry?.referer || "";
  const dlHeaders = referer ? { Referer: referer } : {};

  // Route to the right FluxGet endpoint
  if (VIDEO_EXT_RE.test(filename) || VIDEO_MIME_RE.test(mime) || isManifestURL(item.url)) {
    sendStream(item.url, filename, dlHeaders);
  } else {
    sendDownload(item.url, filename, dlHeaders);
  }

  notify("FluxGet", `Downloading: ${filename}`);
});

// ── Network video capture (like IDM's socket-level monitor) ──────────────────
// Watches all HTTP responses and captures video/stream URLs seen in network traffic.
chrome.webRequest.onResponseStarted.addListener(
  (details) => {
    if (details.tabId < 0) return;
    const url = details.url;

    // Skip individual segments — we want the manifest, not 6000 .ts chunks.
    // SKIP_FRAG_RE catches .ts/.m4s extension chunks.
    // isYouTubeCDNFragment() matches IDMNetMon's exact M4S/MP4 segment skip logic
    // (M4S in /avf/ or /sep/ paths + segment- prefix; MP4 in /avf/ or /parcel/ paths).
    if (SKIP_FRAG_RE.test(url) || isYouTubeCDNFragment(url)) return;

    // Skip YouTube internal UI/API paths — not real video content
    if (/youtube\.com\/s\//.test(url)) return;

    const hdrs = details.responseHeaders || [];
    const ct   = (hdrs.find(h => h.name.toLowerCase() === "content-type") || {}).value || "";
    const cl   = parseInt((hdrs.find(h => h.name.toLowerCase() === "content-length") || {}).value || "0");

    if (!VIDEO_MIME_RE.test(ct) && !VIDEO_EXT_RE.test(url) && !MANIFEST_PATH_RE.test(url)) return;

    // Capture the document URL as Referer — CDNs like streamruby block without it
    const referer = details.documentUrl || details.initiator || "";

    const list = capturedVideos.get(details.tabId) || [];
    if (!list.find(v => v.url === url)) {
      list.push({ url, type: ct, size: cl, ts: Date.now(), referer });
      if (list.length > 50) list.splice(0, list.length - 50);
    }
    capturedVideos.set(details.tabId, list);
  },
  { urls: ["<all_urls>"], types: ["media", "xmlhttprequest", "other"] },
  ["responseHeaders"]
);

// Live capture of CDN request cookies — mirrors IDM's onBeforeSendHeaders approach.
// As the browser plays a video it fetches HLS/DASH segments; we intercept those
// requests and store the Cookie header per-hostname so FluxGet can use the exact
// same authenticated headers when it downloads the same segments later.
// Requires "extraHeaders" to access the Cookie header (Chrome MV3 observational mode).
chrome.webRequest.onBeforeSendHeaders.addListener(
  (details) => {
    if (details.tabId < 0) return;
    try {
      const cookieHdr = (details.requestHeaders || []).find(
        h => h.name.toLowerCase() === "cookie"
      );
      if (cookieHdr?.value) {
        const host = new URL(details.url).hostname;
        capturedDomainCookies.set(host, cookieHdr.value);
      }
    } catch {}
  },
  { urls: ["<all_urls>"], types: ["media", "xmlhttprequest", "other"] },
  ["requestHeaders", "extraHeaders"]
);

chrome.tabs.onRemoved.addListener(id => {
  capturedVideos.delete(id);
  pageVideoData.delete(id);
});
chrome.tabs.onUpdated.addListener((id, info) => {
  if (info.status === "loading") {
    capturedVideos.delete(id);
    pageVideoData.delete(id);
  }
});

// ── URL resolution helpers ────────────────────────────────────────────────────
function getBestCaptured(tabId) {
  const list = capturedVideos.get(tabId) || [];
  if (!list.length) return null;
  // Prefer manifest URLs (m3u8/mpd) over raw MP4 chunks
  const manifests = list.filter(v => isManifestURL(v.url) || /mpegurl|dash\+xml/i.test(v.type));
  if (manifests.length) return manifests[manifests.length - 1].url;
  // Otherwise pick the largest file
  return [...list].sort((a, b) => (b.size || 0) - (a.size || 0))[0].url;
}

function isManifestURL(url) {
  return /\.(m3u8|mpd)(\?|#|$)/i.test(url);
}

function isKnownVideoHost(url) {
  try {
    const host = new URL(url).hostname.toLowerCase();
    return KNOWN_VIDEO_HOSTS.some(h => host === h || host.endsWith("." + h));
  } catch (_) { return false; }
}

function resolveVideoUrl(msg, sender) {
  const tabId  = sender.tab?.id;
  const tabUrl = sender.tab?.url || "";

  // 1. BEST: DASH/HLS manifest captured from document.js (ytInitialPlayerResponse)
  const pd = tabId != null ? pageVideoData.get(tabId) : null;
  if (pd?.dash) return pd.dash;
  if (pd?.hls)  return pd.hls;

  // 2. Network-captured manifest/video URL
  if (tabId != null) {
    const captured = getBestCaptured(tabId);
    if (captured) return captured;
  }

  // 3. Known platform page → pass to yt-dlp fallback in the backend
  if (isKnownVideoHost(tabUrl)) return tabUrl;

  // 4. Explicit URL from message, or iframe src
  return msg.url || tabUrl;
}

// ── Context menus ─────────────────────────────────────────────────────────────
chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: "fg_link",       title: "⬇ Download link with FluxGet",               contexts: ["link"] });
    chrome.contextMenus.create({ id: "fg_video",      title: "⬇ Download video with FluxGet",              contexts: ["video", "audio"] });
    chrome.contextMenus.create({ id: "fg_page_video", title: "⬇ Download video on this page (FluxGet)",    contexts: ["page"] });
    chrome.contextMenus.create({ id: "fg_selection",  title: "⬇ Download selected URL with FluxGet",       contexts: ["selection"] });
    chrome.contextMenus.create({ id: "fg_open_ui",    title: "🖥 Open FluxGet dashboard",                  contexts: ["action"] });
  });
});

chrome.contextMenus.onClicked.addListener((info, tab) => {
  const tabId = tab?.id;
  switch (info.menuItemId) {
    case "fg_link": {
      const url = info.linkUrl || "";
      if (!url) return;
      const pageRef = tab?.url ? { Referer: tab.url } : {};
      if (isManifestURL(url) || isKnownVideoHost(url)) {
        sendStream(url, url.split("/").pop().split("?")[0] || "video", pageRef);
      } else {
        sendDownload(url, url.split("/").pop().split("?")[0] || "file", pageRef);
      }
      break;
    }
    case "fg_video": {
      const url = getBestCaptured(tabId) || info.srcUrl || info.pageUrl || tab?.url;
      sendStream(url, tab?.title || "");
      break;
    }
    case "fg_page_video": {
      const pd  = tabId != null ? pageVideoData.get(tabId) : null;
      const url = (pd?.dash || pd?.hls) || getBestCaptured(tabId) || tab?.url;
      sendStream(url, tab?.title || "", {}, pd?.formats);
      break;
    }
    case "fg_selection": {
      const url = (info.selectionText || "").trim();
      if (!url.startsWith("http")) return;
      const pageRef = tab?.url ? { Referer: tab.url } : {};
      isKnownVideoHost(url) || isManifestURL(url)
        ? sendStream(url, "", pageRef)
        : sendDownload(url, url.split("/").pop().split("?")[0] || "file", pageRef);
      break;
    }
    case "fg_open_ui": {
      chrome.storage.local.get("fluxget_settings", (res) => {
        const token = (res.fluxget_settings || {}).token || "";
        const url   = token ? `${BASE_URL}/ui?token=${token}` : `${BASE_URL}/ui`;
        chrome.tabs.create({ url });
      });
      break;
    }
  }
});

// ── Messages from content.js ──────────────────────────────────────────────────
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  const tabId = sender.tab?.id;

  // document.js → content.js → background: YouTube player data
  if (msg.action === "yt_player" && tabId != null) {
    pageVideoData.set(tabId, {
      dash:    msg.dash    || "",
      hls:     msg.hls     || "",
      formats: msg.formats || [],
      title:   msg.title   || "",
    });
    const pickerOptions = buildPickerOptions(msg.formats || []);
    chrome.tabs.sendMessage(tabId, {
      action:  "video_qualities",
      options: pickerOptions,
      title:   msg.title,
    }).catch(() => {});
    return;
  }

  // content.js requests quality data on load (fixes race condition)
  if (msg.action === "get_quality_data") {
    const pd = tabId != null ? pageVideoData.get(tabId) : null;
    sendResponse({
      options: pd ? buildPickerOptions(pd.formats || []) : [],
      title:   pd?.title || "",
    });
    return true;
  }

  // document.js intercepted a manifest (fetch/XHR)
  if (msg.action === "manifest_intercepted" && tabId != null) {
    const existing = capturedVideos.get(tabId) || [];
    if (!existing.find(v => v.url === msg.url)) {
      existing.push({ url: msg.url, type: msg.contentType || "", size: 0, ts: Date.now() });
      capturedVideos.set(tabId, existing);
    }
    return;
  }

  // Content.js floating button clicked → unified routing
  if (msg.action === "download_video") {
    const tabUrl  = sender.tab?.url || "";
    const title   = sender.tab?.title || msg.title || "";
    const pd      = tabId != null ? pageVideoData.get(tabId) : null;

    // Priority 1: YouTube adaptive — direct CDN URLs + ffmpeg mux
    if (pd && pd.formats && pd.formats.length > 0 && (pd.dash || pd.hls)) {
      sendStream(pd.dash || pd.hls, title, {}, pd.formats);
      return;
    }
    // Priority 2: Captured HLS/DASH manifest from network
    const captured = getBestCaptured(tabId);
    if (captured && isManifestURL(captured)) {
      const referer = (capturedVideos.get(tabId) || []).find(v => v.url === captured)?.referer || "";
      sendStream(captured, title, referer ? { Referer: referer } : {});
      return;
    }
    // Priority 3: Captured direct video URL
    if (captured) {
      const referer = (capturedVideos.get(tabId) || []).find(v => v.url === captured)?.referer || "";
      sendStream(captured, title, referer ? { Referer: referer } : {});
      return;
    }
    // Priority 4: Known yt-dlp platform page
    if (isKnownVideoHost(tabUrl)) {
      sendStream(tabUrl, title, {});
      return;
    }
    // Priority 5: YouTube DASH/HLS manifest without formats
    if (pd && (pd.dash || pd.hls)) {
      sendStream(pd.dash || pd.hls, title, {});
      return;
    }
    // No stream found — notify user to use popup
    notify("FluxGet", "No stream detected yet. Open the FluxGet popup after the video starts playing.");
    return;
  }

  // Content.js: user picked a specific quality from IDM-style picker
  if (msg.action === "download_quality") {
    const pd = tabId != null ? pageVideoData.get(tabId) : null;
    if (msg.videoUrl && msg.audioUrl) {
      // Both direct URLs available — native adaptive mux
      sendStream(msg.pageUrl || (pd?.dash || ""), msg.title || "", {}, msg.formats || []);
    } else if (msg.videoUrl) {
      // Pre-muxed direct URL (e.g. 720p MP4 combined)
      sendStream(msg.videoUrl, msg.title || "", {});
    } else if (msg.ytFormat) {
      // No direct URL — use yt-dlp with specific format selector
      const pageUrl = msg.pageUrl || sender.tab?.url || "";
      apiPost("/stream", {
        url:      pageUrl,
        title:    msg.title || "",
        ytFormat: msg.ytFormat,
      });
    }
    return;
  }

  // Generic file download from content.js
  if (msg.action === "download") {
    sendDownload(msg.url, msg.filename || "", msg.headers || {});
    return;
  }

  // Popup: get list of captured videos for current tab
  // msg.tabId must be provided by the popup (sender.tab is undefined for popups)
  if (msg.action === "get_captured") {
    const lookupId = msg.tabId ?? tabId;
    sendResponse({
      captured: capturedVideos.get(lookupId) || [],
      pageData: pageVideoData.get(lookupId) || null,
    });
    return true;
  }
});

// ── Keepalive / health ────────────────────────────────────────────────────────
setConnected(false);
ping();
chrome.alarms.create("fg_keepalive", { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(a => { if (a.name === "fg_keepalive") ping(); });

// ── Settings ──────────────────────────────────────────────────────────────────
chrome.storage.local.get("fluxget_settings", (res) => {
  if (res.fluxget_settings) Object.assign(settings, res.fluxget_settings);
});
chrome.storage.onChanged.addListener((changes) => {
  if (changes.fluxget_settings) Object.assign(settings, changes.fluxget_settings.newValue || {});
});
