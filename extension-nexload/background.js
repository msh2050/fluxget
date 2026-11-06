"use strict";

// ── Config ────────────────────────────────────────────────────────────────────
const FLUXGET_PORT = 1700;
const BASE_URL     = `http://127.0.0.1:${FLUXGET_PORT}`;

// File extensions FluxGet intercepts from the browser's own downloader.
// IDM's default list (no still images — "Save image as…" stays in the browser).
const INTERCEPT_EXTS = new Set([
  "zip","rar","7z","gz","tgz","tar","bz2","xz","zst","lz","lzh","cab","arj","iso","img","dmg","bin",
  "exe","msi","msu","deb","rpm","pkg","apk","xapk","appimage","flatpak","snap",
  "mp4","mkv","avi","mov","flv","wmv","webm","m4v","ts","mpeg","mpg","3gp","ogv","rm","rmvb","asf","qt",
  "mp3","aac","flac","ogg","opus","wav","m4a","wma","aif","ra","mpa",
  "pdf","doc","docx","xls","xlsx","ppt","pptx","pps","odt","ods","epub",
  "tif","tiff","torrent",
  "m3u8","mpd",
]);

// MIME types that always go to FluxGet (never let browser handle them)
const INTERCEPT_MIME = /^(video\/|audio\/|application\/(zip|x-zip-compressed|x-rar|vnd\.rar|x-7z|x-iso9660|vnd\.android|x-deb|vnd\.debian|x-rpm|x-msdownload|x-msi|x-apple-diskimage|x-mpegurl|vnd\.apple\.mpegurl|dash\+xml|x-tar|gzip|x-gzip|x-bzip2|x-xz|zstd|pdf))/i;

// Don't intercept tiny files (icons, small JSON/CSV exports) when the size is known.
const MIN_INTERCEPT_BYTES = 512 * 1024;

// Video MIME types captured from network (not intercepted as downloads, but shown as button)
const VIDEO_MIME_RE = /^(video|audio)\/|application\/(x-mpegurl|vnd\.apple\.mpegurl|dash\+xml|vnd\.ms-sstr\+xml)/i;
const VIDEO_EXT_RE  = /\.(mp4|webm|mkv|m3u8|mpd|flv|avi|mov|m4v|m4a|mp3|ogg|opus)(\?|#|$)/i;
// HLS/DASH manifest paths that don't use standard extensions (IDMNetMon pattern: ^/playlist/\w+/|^/hls/)
const MANIFEST_PATH_RE = /\/(playlist|master|manifest|chunklist)[^/]*?\.(m3u8|mpd)(\?|$)|\/(hls|dash)\/[^?]*\.(m3u8|mpd)(\?|$)/i;

// Skip individual adaptive segments — IDMNetMon's segment skip logic.
const SKIP_FRAG_RE = /\.(ts|m4s|cmfa|cmfv|fmp4|aac|vtt|webvtt|key)(\?|#|$)/i;

function isYouTubeCDNFragment(url) {
  const u = url.toLowerCase();
  if (u.includes(".m4s") && (u.includes("/avf/") || u.includes("/sep/"))) return true;
  const path = u.split("?")[0];
  if (path.endsWith(".mp4") && (u.includes("/avf/") || u.includes("/parcel/"))) return true;
  if (u.includes("/api/manifest/init_segment")) return true;
  // googlevideo videoplayback range requests are SABR/DASH fragments, not files
  if (u.includes("googlevideo.com/videoplayback")) return true;
  return false;
}

// IDMNetMon site dispatch table order (decompiled IDMNetMon.dll 35974-36059):
// Lumberjack → MPD → YouTube → Facebook → Vimeo → Instagram → OK/mycdn → Hydrax → generic.
const KNOWN_VIDEO_HOSTS = [
  "youtube.com", "youtu.be", "youtube-nocookie.com",
  "youtube.googleapis.com", "drive.google.com", "docs.google.com",
  "facebook.com", "workplace.com", "fb.watch",
  "instagram.com",
  "vimeo.com",
  "ok.ru", "mycdn.me", "vk.com", "vkvideo.ru",
  "soundcloud.com",
  "hydrax.to",
  "udemy.com",
  "twitch.tv",
  "dailymotion.com", "tiktok.com",
  "twitter.com", "x.com",
  "reddit.com",
  "bilibili.com", "bilibili.tv",
  "nicovideo.jp",
  "bandcamp.com",
  "rumble.com",
  "odysee.com",
  "streamable.com",
  "mixcloud.com",
  "kick.com",
  "yandex.ru", "yandex.com", "dzen.ru",
  "rutube.ru",
  "9gag.com", "imgur.com", "pinterest.com", "linkedin.com", "tumblr.com",
];

// ── Per-tab state (persisted: the MV3 worker is killed after ~30 s idle) ─────
// tab → { captured: [{url,type,size,ts,referer,hasSubs}], page: {hls,dash,formats,title,videoId,pageUrl} }
const tabCache = new Map();
const MAX_CAPTURED = 60;

async function getTab(tabId) {
  if (tabId == null || tabId < 0) return { captured: [], page: null };
  if (tabCache.has(tabId)) return tabCache.get(tabId);
  let st = { captured: [], page: null };
  try {
    const key = `tab_${tabId}`;
    const got = await chrome.storage.session.get(key);
    if (got[key]) st = got[key];
  } catch {}
  tabCache.set(tabId, st);
  return st;
}

const saveTimers = new Map();
function saveTab(tabId) {
  if (tabId == null || tabId < 0) return;
  clearTimeout(saveTimers.get(tabId));
  saveTimers.set(tabId, setTimeout(() => {
    saveTimers.delete(tabId);
    const st = tabCache.get(tabId);
    if (!st) return;
    // Formats carry long signed URLs; keep the stored copy bounded.
    chrome.storage.session.set({ [`tab_${tabId}`]: st }).catch(() => {});
  }, 300));
}

async function clearTab(tabId) {
  tabCache.set(tabId, { captured: [], page: null });
  try { await chrome.storage.session.remove(`tab_${tabId}`); } catch {}
}

async function addCaptured(tabId, entry) {
  const st = await getTab(tabId);
  const existing = st.captured.find(v => v.url === entry.url);
  if (existing) {
    Object.assign(existing, Object.fromEntries(Object.entries(entry).filter(([, v]) => v)));
  } else {
    st.captured.push(entry);
    if (st.captured.length > MAX_CAPTURED) st.captured.splice(0, st.captured.length - MAX_CAPTURED);
  }
  saveTab(tabId);
  updateTabBadge(tabId, st);
}

// hostname → Cookie header captured live from onBeforeSendHeaders.
const capturedDomainCookies = new Map();

// Settings (loaded from chrome.storage.local)
let settings = {
  interceptDownloads: true,
  autoDownloadVideo: false,
  port: FLUXGET_PORT,
  destDir: "",
};

// ── Quality picker builder (IDM-style) ───────────────────────────────────────
// Each entry: label, height, codec, type, maxHeight (for /stream), and the
// direct formats to use when YouTube exposed un-ciphered URLs.
function buildPickerOptions(formats) {
  const videoByHeight = new Map();
  let bestAudio = null;
  for (const f of formats) {
    const mime = (f.mimeType || "").toLowerCase();
    if (f.height > 0 && mime.startsWith("video/")) {
      const cur = videoByHeight.get(f.height);
      const score = x => (x.url ? 1e12 : 0) + (x.progressive ? 0 : 1e11) + (x.bitrate || 0);
      if (!cur || score(f) > score(cur)) videoByHeight.set(f.height, f);
    } else if (mime.startsWith("audio/")) {
      const better = !bestAudio || (!!f.url && !bestAudio.url) || (!!f.url === !!bestAudio.url && (f.bitrate || 0) > (bestAudio.bitrate || 0));
      if (better) bestAudio = f;
    }
  }
  const codec = f => {
    const m = (f.mimeType || "").toLowerCase();
    if (m.includes("av01")) return "AV1";
    if (m.includes("vp9") || m.includes("vp09") || m.includes("webm")) return "VP9";
    if (m.includes("avc") || m.includes("mp4")) return "H.264";
    return "";
  };
  const options = [];
  for (const [height, f] of [...videoByHeight.entries()].sort((a, b) => b[0] - a[0])) {
    // Direct when this height is muxed, or when both video+audio URLs are known.
    const direct = !!f.url && (f.progressive || !!bestAudio?.url);
    options.push({
      label: f.quality || `${height}p`,
      height, maxHeight: height, type: "video",
      codec: codec(f),
      size: (f.contentLength || 0) + (f.progressive ? 0 : (bestAudio?.contentLength || 0)),
      direct,
    });
  }
  if (bestAudio) {
    const kbps = bestAudio.bitrate ? Math.round(bestAudio.bitrate / 1000) + "k" : "";
    options.push({
      label: `Audio only${kbps ? " " + kbps : ""}`,
      height: 0, maxHeight: 0, type: "audio", audioOnly: true,
      codec: (bestAudio.mimeType || "").toLowerCase().includes("opus") ? "Opus" : "AAC",
      size: bestAudio.contentLength || 0,
      direct: !!bestAudio.url,
    });
  }
  return options;
}

// ── Icon / badge ──────────────────────────────────────────────────────────────
let backendUp = false;
function setConnected(ok) {
  backendUp = ok;
  chrome.action.setBadgeText({ text: ok ? "" : "OFF" });
  chrome.action.setBadgeBackgroundColor({ color: ok ? "#1a7f3c" : "#c0392b" });
  chrome.action.setTitle({ title: ok ? "FluxGet (connected)" : "FluxGet (not running — start: fluxget server start)" });
}

function updateTabBadge(tabId, st) {
  if (!backendUp) return;
  const n = (st.captured || []).length + (st.page && (st.page.hls || st.page.dash || (st.page.formats || []).length) ? 1 : 0);
  chrome.action.setBadgeText({ tabId, text: n ? String(Math.min(n, 99)) : "" }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ tabId, color: "#0070c8" }).catch(() => {});
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
      signal: AbortSignal.timeout(10000),
    });
    setConnected(true);
    if (!r.ok) {
      const msg = (await r.text().catch(() => "")).trim();
      notify("FluxGet", `Request failed (${r.status})${msg ? ": " + msg.slice(0, 160) : ""}`);
      return null;
    }
    return r.json().catch(() => ({}));
  } catch (_) {
    setConnected(false);
    return null;
  }
}

// getCookiesForURL returns the browser's Cookie header for a URL (includes
// parent-domain and partitioned cookies the page would send).
async function getCookiesForURL(url) {
  try {
    const cookies = await chrome.cookies.getAll({ url });
    return cookies.map(c => `${c.name}=${c.value}`).join("; ");
  } catch {
    return "";
  }
}

async function headersFor(url, referer) {
  const h = {};
  if (referer) h.Referer = referer;
  try {
    const host = new URL(url).hostname;
    const cookie = capturedDomainCookies.get(host) || await getCookiesForURL(url);
    if (cookie) h.Cookie = cookie;
  } catch {}
  h["User-Agent"] = navigator.userAgent;
  return h;
}

// Regular file download → /download (multi-connection HTTP engine)
async function sendDownload(url, filename, referer) {
  const headers = await headersFor(url, referer);
  return apiPost("/download", { url, filename: filename || "", headers, skip_approval: true });
}

// sniffSegmentCookies fetches a manifest in the browser's authenticated
// context, follows one master→media hop and returns cookies for the segment
// host(s) when they differ from the manifest host (e.g. googleusercontent CDNs).
async function sniffSegmentCookies(manifestUrl) {
  const seen = new Set([new URL(manifestUrl).hostname]);
  const parts = [];
  const collect = async (u) => {
    try {
      const h = new URL(u).hostname;
      if (seen.has(h)) return;
      seen.add(h);
      const c = capturedDomainCookies.get(h) || await getCookiesForURL(u);
      if (c) parts.push(c);
    } catch {}
  };
  const firstURL = (text, base) => {
    for (const line of text.split("\n")) {
      const t = line.trim();
      if (!t || t.startsWith("#")) continue;
      try { return new URL(t, base).href; } catch {}
    }
    return null;
  };
  try {
    const r = await fetch(manifestUrl, { credentials: "include", signal: AbortSignal.timeout(8000) });
    if (!r.ok) return "";
    const first = firstURL(await r.text(), manifestUrl);
    if (!first) return "";
    await collect(first);
    if (/\.m3u8(\?|#|$)/i.test(first)) {
      const vr = await fetch(first, { credentials: "include", signal: AbortSignal.timeout(8000) });
      if (vr.ok) {
        const seg = firstURL(await vr.text(), first);
        if (seg) await collect(seg);
      }
    }
  } catch {}
  return parts.join("; ");
}

// Video / stream → /stream (native HLS/DASH engine, yt-dlp fallback).
// opts: { referer, pageUrl, formats, maxHeight, audioOnly, subtitles, ytFormat }
async function sendStream(url, title, opts = {}) {
  const headers = await headersFor(url, opts.referer);
  if (isManifestURL(url)) {
    const seg = await sniffSegmentCookies(url);
    if (seg) headers.Cookie = headers.Cookie ? `${headers.Cookie}; ${seg}` : seg;
  }
  const body = {
    url,
    title: title || "",
    headers,
    formats: opts.formats || [],
    pageUrl: opts.pageUrl || "",
  };
  if (opts.maxHeight) body.maxHeight = opts.maxHeight;
  if (opts.audioOnly) body.audioOnly = true;
  if (opts.subtitles) body.subtitles = true;
  if (opts.ytFormat) body.ytFormat = opts.ytFormat;
  if (settings.destDir) body.path = settings.destDir;
  return apiPost("/stream", body);
}

// Resolve the best URL to pull subtitles from: the HLS master is where
// subtitle renditions are declared.
async function subtitleSourceURL(tabId, fallback) {
  const st = await getTab(tabId);
  if (st.page?.hls) return st.page.hls;
  const withSubs = st.captured.find(v => v.hasSubs);
  if (withSubs) return withSubs.url;
  const best = bestCaptured(st);
  if (best && isManifestURL(best.url)) return best.url;
  return best?.url || fallback || "";
}

// ── Notification helper ───────────────────────────────────────────────────────
function notify(title, message) {
  chrome.notifications.create({ type: "basic", iconUrl: "icons/icon48.png", title, message });
}

// ── chrome.downloads interception (THE KEY IDM FEATURE) ──────────────────────
// URLs we hand back to the browser after FluxGet refused/was unreachable —
// never intercept those a second time.
const passThrough = new Set();

chrome.downloads.onCreated.addListener(async (item) => {
  if (!settings.interceptDownloads) return;
  const url = item.finalUrl || item.url || "";
  if (!/^https?:/i.test(url)) return;              // blob:, data:, file:, extension pages
  if (passThrough.delete(url) || passThrough.delete(item.url)) return;
  if (item.byExtensionId === chrome.runtime.id) return;
  if (item.totalBytes > 0 && item.totalBytes < MIN_INTERCEPT_BYTES) return;

  const mime = (item.mime || "").toLowerCase();
  const name = (item.filename || url.split("?")[0]).split(/[\\/]/).pop();
  const ext  = name.includes(".") ? name.split(".").pop().toLowerCase() : "";
  if (!(INTERCEPT_MIME.test(mime) || INTERCEPT_EXTS.has(ext))) return;

  // Never swallow a download when FluxGet is down.
  if (!backendUp && !(await ping())) return;

  // Cancel the browser download; FluxGet takes it from here.
  try { await chrome.downloads.cancel(item.id); } catch {}
  chrome.downloads.erase({ id: item.id }).catch(() => {});

  const referer = item.referrer || "";
  const filename = name || "download";
  const res = (VIDEO_EXT_RE.test(filename) || VIDEO_MIME_RE.test(mime) || isManifestURL(url))
    ? await sendStream(url, filename, { referer })
    : await sendDownload(url, filename, referer);

  if (res) {
    notify("FluxGet", `Downloading: ${filename}`);
  } else {
    // FluxGet rejected it or went away mid-flight: give it back to the browser.
    passThrough.add(url);
    chrome.downloads.download({ url, filename: filename.replace(/[\\/:*?"<>|]/g, "_") }).catch(() => passThrough.delete(url));
  }
});

// ── Network video capture (like IDM's socket-level monitor) ──────────────────
chrome.webRequest.onResponseStarted.addListener(
  (details) => {
    if (details.tabId < 0) return;
    const url = details.url;
    if (SKIP_FRAG_RE.test(url) || isYouTubeCDNFragment(url)) return;
    if (/youtube\.com\/(s|api|youtubei)\//.test(url)) return;

    const hdrs = details.responseHeaders || [];
    const hv = n => (hdrs.find(h => h.name.toLowerCase() === n) || {}).value || "";
    const ct = hv("content-type");
    let size = parseInt(hv("content-length") || "0", 10) || 0;
    const cr = hv("content-range");
    if (cr && cr.includes("/")) size = parseInt(cr.split("/").pop(), 10) || size;

    const isManifest = isManifestURL(url) || /mpegurl|dash\+xml/i.test(ct) || MANIFEST_PATH_RE.test(url);
    const isMedia = VIDEO_MIME_RE.test(ct) || VIDEO_EXT_RE.test(url);
    if (!isManifest && !isMedia) return;
    // Ignore tiny media responses (preview sprites, ad pings, beacons).
    if (!isManifest && size > 0 && size < 300 * 1024) return;

    addCaptured(details.tabId, {
      url, type: ct, size, ts: Date.now(),
      referer: details.documentUrl || details.initiator || "",
    });
  },
  { urls: ["<all_urls>"], types: ["media", "xmlhttprequest", "other"] },
  ["responseHeaders"]
);

// Live capture of CDN request cookies (IDM's onBeforeSendHeaders approach):
// includes partitioned/SameSite cookies chrome.cookies can't see.
chrome.webRequest.onBeforeSendHeaders.addListener(
  (details) => {
    if (details.tabId < 0) return;
    try {
      const cookieHdr = (details.requestHeaders || []).find(h => h.name.toLowerCase() === "cookie");
      if (!cookieHdr?.value) return;
      const host = new URL(details.url).hostname;
      if (capturedDomainCookies.get(host) === cookieHdr.value) return; // unchanged: no storage write
      capturedDomainCookies.set(host, cookieHdr.value);
      chrome.storage.session.set({ [`cdncookie_${host}`]: cookieHdr.value }).catch(() => {});
    } catch {}
  },
  { urls: ["<all_urls>"], types: ["media", "xmlhttprequest", "other"] },
  ["requestHeaders", "extraHeaders"]
);

chrome.tabs.onRemoved.addListener(id => {
  tabCache.delete(id);
  chrome.storage.session.remove(`tab_${id}`).catch(() => {});
});
chrome.tabs.onUpdated.addListener((id, info) => {
  // A new page load invalidates captured media. (Stale YouTube SPA data is
  // additionally guarded by freshPage()'s videoId check.)
  if (info.status === "loading") clearTab(id);
});

// ── URL resolution helpers ────────────────────────────────────────────────────
function bestCaptured(st) {
  const list = st.captured || [];
  if (!list.length) return null;
  const manifests = list.filter(v => isManifestURL(v.url) || /mpegurl|dash\+xml/i.test(v.type || ""));
  if (manifests.length) {
    // A master playlist (declares variants/subtitles) beats a media playlist.
    const master = [...manifests].reverse().find(v => v.isMaster || v.hasSubs);
    return master || manifests[manifests.length - 1];
  }
  return [...list].sort((a, b) => (b.size || 0) - (a.size || 0))[0];
}

function isManifestURL(url) {
  return /\.(m3u8|mpd)(\?|#|$)/i.test(url || "");
}

function isKnownVideoHost(url) {
  try {
    const host = new URL(url).hostname.toLowerCase();
    return KNOWN_VIDEO_HOSTS.some(h => host === h || host.endsWith("." + h));
  } catch (_) { return false; }
}

function youTubeVideoId(url) {
  try {
    const u = new URL(url);
    if (u.hostname === "youtu.be") return u.pathname.slice(1);
    if (u.pathname.startsWith("/shorts/")) return u.pathname.split("/")[2] || "";
    return u.searchParams.get("v") || "";
  } catch { return ""; }
}

// Page data is only trusted when it belongs to the video the tab shows now
// (YouTube is an SPA: the tab URL changes without a reload).
function freshPage(st, tabUrl) {
  const pd = st.page;
  if (!pd) return null;
  if (pd.videoId) {
    const vid = youTubeVideoId(tabUrl);
    if (vid && vid !== pd.videoId) return null;
  }
  return pd;
}

// Unified "download the video on this tab" routing.
async function downloadVideoOnTab(tab, opts = {}) {
  const tabId = tab?.id;
  const tabUrl = tab?.url || "";
  const title = opts.title || tab?.title || "";
  const st = await getTab(tabId);
  const pd = freshPage(st, tabUrl);
  const common = { pageUrl: tabUrl, maxHeight: opts.maxHeight || 0, audioOnly: !!opts.audioOnly };

  // 1. YouTube & friends: page data from the player response.
  if (pd && ((pd.formats || []).some(f => f.url) || pd.hls || pd.dash)) {
    const hasDirect = (pd.formats || []).some(f => f.url);
    if (hasDirect) return sendStream(pd.hls || pd.dash || tabUrl, title, { ...common, formats: pd.formats });
    if (pd.hls) return sendStream(pd.hls, title, common);
  }
  // 2. Platform page yt-dlp understands (ciphered YouTube formats, Vimeo, X…).
  if (isKnownVideoHost(tabUrl)) return sendStream(tabUrl, title, common);
  // 3. Captured manifest / media from the network.
  const cap = bestCaptured(st);
  if (cap) return sendStream(cap.url, title, { ...common, referer: cap.referer || tabUrl });
  // 4. Explicit element source.
  if (opts.srcUrl && /^https?:/i.test(opts.srcUrl)) return sendStream(opts.srcUrl, title, { ...common, referer: tabUrl });

  notify("FluxGet", "No stream detected yet — start playing the video, then try again.");
  return null;
}

// ── Context menus ─────────────────────────────────────────────────────────────
chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: "fg_link",       title: "⬇ Download link with FluxGet",            contexts: ["link"] });
    chrome.contextMenus.create({ id: "fg_video",      title: "⬇ Download video with FluxGet",           contexts: ["video", "audio"] });
    chrome.contextMenus.create({ id: "fg_page_video", title: "⬇ Download video on this page (FluxGet)", contexts: ["page"] });
    chrome.contextMenus.create({ id: "fg_audio",      title: "♪ Download audio only (FluxGet)",         contexts: ["video", "audio", "page"] });
    chrome.contextMenus.create({ id: "fg_subtitles",  title: "⬇ Download subtitles only (SRT)",         contexts: ["video", "audio", "page"] });
    chrome.contextMenus.create({ id: "fg_selection",  title: "⬇ Download selected URL with FluxGet",    contexts: ["selection"] });
    chrome.contextMenus.create({ id: "fg_open_ui",    title: "Open FluxGet dashboard",                       contexts: ["action"] });
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const tabId = tab?.id;
  const ref = tab?.url || "";
  switch (info.menuItemId) {
    case "fg_link":
    case "fg_selection": {
      const url = (info.menuItemId === "fg_link" ? info.linkUrl : (info.selectionText || "").trim()) || "";
      if (!/^https?:\/\//i.test(url)) return;
      const name = decodeURIComponent(url.split("?")[0].split("/").pop() || "") || "file";
      if (isManifestURL(url) || isKnownVideoHost(url) || VIDEO_EXT_RE.test(url)) {
        sendStream(url, name, { referer: ref, pageUrl: isKnownVideoHost(url) ? url : "" });
      } else {
        sendDownload(url, name, ref);
      }
      break;
    }
    case "fg_video":
    case "fg_page_video":
      downloadVideoOnTab(tab, { srcUrl: info.srcUrl });
      break;
    case "fg_audio":
      downloadVideoOnTab(tab, { srcUrl: info.srcUrl, audioOnly: true });
      break;
    case "fg_subtitles": {
      const url = await subtitleSourceURL(tabId, info.srcUrl || ref);
      if (!url) { notify("FluxGet", "No subtitle source detected yet. Play the video, then try again."); break; }
      sendStream(url, tab?.title || "", { referer: ref, subtitles: true });
      break;
    }
    case "fg_open_ui": {
      const res = await chrome.storage.local.get("fluxget_settings");
      const token = (res.fluxget_settings || {}).token || "";
      chrome.tabs.create({ url: token ? `${BASE_URL}/ui?token=${encodeURIComponent(token)}` : `${BASE_URL}/ui` });
      break;
    }
  }
});

// ── HLS master inspection (fetched here: content scripts are CORS-bound) ─────
async function inspectHLS(url) {
  try {
    const r = await fetch(url, { credentials: "include", signal: AbortSignal.timeout(8000) });
    const body = await r.text();
    if (!body.includes("#EXT-X-STREAM-INF")) return { variants: [], hasSubs: /\.vtt/i.test(body) };
    const variants = [];
    let pending = null;
    for (const raw of body.split("\n")) {
      const l = raw.trim();
      if (l.startsWith("#EXT-X-STREAM-INF:")) {
        pending = {
          bandwidth: parseInt((l.match(/BANDWIDTH=(\d+)/) || [])[1] || "0", 10),
          height: parseInt((l.match(/RESOLUTION=\d+x(\d+)/i) || [])[1] || "0", 10),
        };
      } else if (pending && l && !l.startsWith("#")) {
        variants.push(pending);
        pending = null;
      }
    }
    // One entry per height (highest bandwidth).
    const byH = new Map();
    for (const v of variants) {
      const cur = byH.get(v.height);
      if (!cur || v.bandwidth > cur.bandwidth) byH.set(v.height, v);
    }
    return {
      variants: [...byH.values()].sort((a, b) => b.height - a.height || b.bandwidth - a.bandwidth),
      hasSubs: /#EXT-X-MEDIA:[^\n]*TYPE=SUBTITLES/i.test(body),
    };
  } catch {
    return { variants: [], hasSubs: false };
  }
}

// ── Messages from content.js / popup ──────────────────────────────────────────
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  const tabId = msg.tabId ?? sender.tab?.id;
  // Messages that claim page data must come from our own content script.
  const fromContent = sender.id === chrome.runtime.id && !!sender.tab;

  (async () => {
    switch (msg.action) {
      case "yt_player": {
        if (!fromContent) return;
        const st = await getTab(tabId);
        st.page = {
          hls: msg.hls || "", dash: msg.dash || "",
          formats: (msg.formats || []).slice(0, 80),
          title: msg.title || "", videoId: msg.videoId || "", pageUrl: sender.tab.url || "",
        };
        saveTab(tabId);
        updateTabBadge(tabId, st);
        chrome.tabs.sendMessage(tabId, {
          action: "video_qualities",
          options: buildPickerOptions(st.page.formats),
          title: st.page.title,
          videoId: st.page.videoId,
        }, { frameId: sender.frameId }).catch(() => {});
        return;
      }
      case "get_quality_data": {
        const st = await getTab(tabId);
        const pd = freshPage(st, sender.tab?.url || "");
        sendResponse({ options: pd ? buildPickerOptions(pd.formats || []) : [], title: pd?.title || "", videoId: pd?.videoId || "" });
        return;
      }
      case "manifest_intercepted": {
        if (!fromContent || !/^https?:/i.test(msg.url || "")) return;
        addCaptured(tabId, {
          url: msg.url, type: msg.contentType || "", size: 0, ts: Date.now(),
          referer: sender.tab.url || "", hasSubs: !!msg.hasSubs,
        });
        return;
      }
      case "inspect_hls": {
        sendResponse(await inspectHLS(msg.url));
        return;
      }
      case "download_video": {
        downloadVideoOnTab(sender.tab, { title: msg.title });
        return;
      }
      case "download_quality": {
        // User picked a quality in the floating panel.
        const tab = sender.tab;
        if (msg.manifestUrl) {
          sendStream(msg.manifestUrl, msg.title || tab?.title || "", {
            referer: tab?.url || "", pageUrl: tab?.url || "",
            maxHeight: msg.maxHeight || 0, audioOnly: !!msg.audioOnly,
          });
        } else {
          downloadVideoOnTab(tab, { title: msg.title, maxHeight: msg.maxHeight || 0, audioOnly: !!msg.audioOnly });
        }
        return;
      }
      case "download_subtitles": {
        const url = msg.url || await subtitleSourceURL(tabId, sender.tab?.url || "");
        if (!url) { notify("FluxGet", "No subtitle source detected yet. Play the video for a few seconds, then try again."); return; }
        sendStream(url, msg.title || sender.tab?.title || "", { referer: sender.tab?.url || "", subtitles: true });
        return;
      }
      case "download": {
        if (/^https?:/i.test(msg.url || "")) sendDownload(msg.url, msg.filename || "", msg.referer || sender.tab?.url || "");
        return;
      }
      case "send_stream": { // popup → explicit stream with options
        const r = await sendStream(msg.url, msg.title || "", msg.opts || {});
        sendResponse({ ok: !!r });
        return;
      }
      case "get_captured": {
        const st = await getTab(tabId);
        const tab = msg.tabId != null ? await chrome.tabs.get(msg.tabId).catch(() => null) : sender.tab;
        sendResponse({ captured: st.captured, pageData: freshPage(st, tab?.url || ""), backendUp });
        return;
      }
    }
  })();
  // Keep the channel open for the async sendResponse callers.
  return ["get_quality_data", "inspect_hls", "get_captured", "send_stream"].includes(msg.action);
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

// Restore CDN cookies captured before the service worker was suspended.
chrome.storage.session.get(null).then(all => {
  for (const [k, v] of Object.entries(all)) {
    if (k.startsWith("cdncookie_") && v) capturedDomainCookies.set(k.slice("cdncookie_".length), v);
  }
}).catch(() => {});
