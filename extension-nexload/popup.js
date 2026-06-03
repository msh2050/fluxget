"use strict";

const BASE_URL = "http://127.0.0.1:1700";

const dot        = document.getElementById("dot");
const statusText = document.getElementById("statusText");
const videoList  = document.getElementById("videoList");
const emptyState = document.getElementById("emptyState");
const videoCount = document.getElementById("videoCount");

let currentTabId    = null;
let currentTabUrl   = "";
let currentTabTitle = "";
let detectedItems   = [];
let interceptEnabled = true;

// ── Helpers ──────────────────────────────────────────────────────────────────
function fmtSize(bytes) {
  if (!bytes || bytes < 1024) return "";
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + " KB";
  return (bytes / 1024 / 1024).toFixed(1) + " MB";
}

function detectFmt(url, mime) {
  const u = url.toLowerCase();
  const m = (mime || "").toLowerCase();
  if (u.includes(".m3u8") || m.includes("mpegurl")) return "HLS";
  if (u.includes(".mpd")  || m.includes("dash+xml")) return "DASH";
  if (u.includes(".mp4") || m.includes("mp4"))  return "MP4";
  if (u.includes(".webm") || m.includes("webm")) return "WebM";
  if (u.includes(".mkv"))  return "MKV";
  if (u.includes(".flv"))  return "FLV";
  return "FMT";
}

function badgeClass(fmt) {
  if (fmt === "HLS")  return "fmt-hls";
  if (fmt === "DASH") return "fmt-dash";
  if (fmt === "MP4" || fmt === "WebM" || fmt === "MKV") return "fmt-mp4";
  return "fmt-fmt";
}

function guessTitle(url) {
  try {
    const path = new URL(url).pathname;
    const name = path.split("/").filter(Boolean).pop() || "";
    return decodeURIComponent(name.split("?")[0]).replace(/\.[^.]+$/, "").replace(/[-_]/g, " ").trim() || url.split("/")[2];
  } catch (_) { return url.slice(0, 40); }
}

// Parse HLS master playlist and return variants sorted best-first.
// Returns [] if it's a media playlist (no #EXT-X-STREAM-INF).
async function fetchHLSVariants(url) {
  try {
    const res = await fetch(url);
    const body = await res.text();
    if (!body.includes("#EXT-X-STREAM-INF")) return [];
    const lines = body.split("\n");
    const variants = [];
    let pending = null;
    for (const line of lines) {
      const l = line.trim();
      if (l.startsWith("#EXT-X-STREAM-INF:")) {
        const bw  = (l.match(/BANDWIDTH=(\d+)/)  || [])[1];
        const res = (l.match(/RESOLUTION=\d+x(\d+)/) || [])[1];
        pending = { bandwidth: bw ? parseInt(bw) : 0, height: res ? parseInt(res) : 0 };
      } else if (pending && l && !l.startsWith("#")) {
        try { pending.url = new URL(l, url).href; } catch { pending.url = l; }
        variants.push(pending);
        pending = null;
      }
    }
    variants.sort((a, b) => b.bandwidth - a.bandwidth);
    return variants;
  } catch { return []; }
}

// ── Render ────────────────────────────────────────────────────────────────────
function renderList() {
  // Remove old items (keep emptyState)
  Array.from(videoList.children).forEach(el => {
    if (el !== emptyState) el.remove();
  });

  if (detectedItems.length === 0) {
    emptyState.style.display = "";
    videoCount.textContent = "";
    return;
  }
  emptyState.style.display = "none";
  videoCount.textContent = detectedItems.length + " video" + (detectedItems.length > 1 ? "s" : "");

  detectedItems.forEach(async (item, idx) => {
    const fmt  = detectFmt(item.url, item.type);
    const isHLS  = fmt === "HLS";
    const isDASH = fmt === "DASH";
    const title = item.title || guessTitle(item.url);

    const row = document.createElement("div");
    row.className = "video-item";

    // Thumbnail
    const thumb = document.createElement("div");
    thumb.className = "thumb";
    if (item.thumb) {
      const img = document.createElement("img");
      img.src = item.thumb;
      img.onerror = () => { img.style.display = "none"; };
      thumb.appendChild(img);
    } else {
      thumb.innerHTML = `<span class="thumb-icon">🎬</span>`;
    }
    if (item.duration) {
      const dur = document.createElement("div");
      dur.className = "duration";
      dur.textContent = item.duration;
      thumb.appendChild(dur);
    }
    row.appendChild(thumb);

    // Info
    const info = document.createElement("div");
    info.className = "video-info";

    const titleEl = document.createElement("div");
    titleEl.className = "video-title";
    titleEl.textContent = title;
    titleEl.title = item.url;
    info.appendChild(titleEl);

    const meta = document.createElement("div");
    meta.className = "video-meta";

    const badge = document.createElement("span");
    badge.className = `fmt-badge ${badgeClass(fmt)}`;
    badge.textContent = fmt;
    meta.appendChild(badge);

    // Quality selector — real variants for HLS, hidden for direct files
    const sel = document.createElement("select");
    sel.className = "quality-select";

    // Placeholder while fetching
    const bestOpt = document.createElement("option");
    bestOpt.value = "";
    bestOpt.textContent = "Best";
    sel.appendChild(bestOpt);

    if (isHLS) {
      // Async: fetch real variants from manifest
      fetchHLSVariants(item.url).then(variants => {
        if (variants.length > 1) {
          sel.innerHTML = "";
          variants.forEach((v, i) => {
            const o = document.createElement("option");
            o.value = v.url;
            o.textContent = v.height ? `${v.height}p` : `Stream ${i + 1}`;
            if (i === 0) o.textContent += " (Best)";
            sel.appendChild(o);
          });
        }
        // If only 1 variant or media playlist — keep "Best" (no choice needed)
      });
    } else if (!isDASH) {
      // Direct file — no quality choice
      sel.style.display = "none";
    }

    meta.appendChild(sel);

    if (item.size) {
      const sz = document.createElement("span");
      sz.style.cssText = "color:#555;font-size:10px;";
      sz.textContent = fmtSize(item.size);
      meta.appendChild(sz);
    }

    info.appendChild(meta);
    row.appendChild(info);

    // Download button
    const dlBtn = document.createElement("button");
    dlBtn.className = "dl-btn";
    dlBtn.innerHTML = "⬇ Download";
    dlBtn.addEventListener("click", () => {
      // For HLS with real variant selected, override item URL with specific variant
      const selectedVariantUrl = isHLS && sel.value ? sel.value : "";
      const itemToDownload = selectedVariantUrl
        ? { ...item, url: selectedVariantUrl }
        : item;
      downloadItem(itemToDownload, fmt, "");
    });
    row.appendChild(dlBtn);

    // Subtitles button — HLS only (subtitles are declared in the master playlist)
    if (isHLS) {
      const subBtn = document.createElement("button");
      subBtn.className = "dl-btn";
      subBtn.innerHTML = "💬 Subs";
      subBtn.title = "Download subtitle tracks only (.srt)";
      subBtn.addEventListener("click", () => {
        const title = item.title || currentTabTitle || guessTitle(item.url);
        const headers = {};
        if (item.referer) headers["Referer"] = item.referer;
        fetch(`${BASE_URL}/stream`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ url: item.url, title, headers, subtitles: true }),
        }).catch(() => {});
        subBtn.innerHTML = "✓ Subs";
        setTimeout(() => { subBtn.innerHTML = "💬 Subs"; }, 1500);
      });
      row.appendChild(subBtn);
    }

    // More (⋮)
    const more = document.createElement("button");
    more.className = "more-btn";
    more.textContent = "⋮";
    more.title = item.url;
    more.addEventListener("click", () => {
      navigator.clipboard.writeText(item.url).catch(() => {});
      more.textContent = "✓";
      setTimeout(() => { more.textContent = "⋮"; }, 1500);
    });
    row.appendChild(more);

    videoList.appendChild(row);
  });
}

// ── Download ──────────────────────────────────────────────────────────────────
async function downloadItem(item, fmt, ytFormat) {
  const isManifest  = fmt === "HLS" || fmt === "DASH";
  const isKnownHost = isKnownVideoSite(currentTabUrl);

  // Title: prefer item title (set from page/tab title), then tab title, then guessed
  const title = item.title || currentTabTitle || guessTitle(item.url);

  // Referer: required for CDN-protected streams
  const headers = {};
  if (item.referer) headers["Referer"] = item.referer;

  // YouTube adaptive: send formats[] for native video+audio mux
  if (item._ytFmts && item._ytFmts.length > 0) {
    await fetch(`${BASE_URL}/stream`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: item.url, title, formats: item._ytFmts, headers }),
    });
    return;
  }

  const body = { url: item.url, title, ytFormat: ytFormat || "", headers };

  if (isManifest || isKnownHost) {
    await fetch(`${BASE_URL}/stream`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } else {
    await fetch(`${BASE_URL}/download`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: item.url, filename: title, headers }),
    });
  }
}

function isKnownVideoSite(url) {
  const KNOWN = ["youtube.com", "youtu.be", "vimeo.com", "twitch.tv", "dailymotion.com",
    "tiktok.com", "facebook.com", "instagram.com", "twitter.com", "x.com",
    "reddit.com", "bilibili.com", "soundcloud.com", "rumble.com", "ok.ru"];
  try {
    const host = new URL(url).hostname.toLowerCase();
    return KNOWN.some(h => host === h || host.endsWith("." + h));
  } catch (_) { return false; }
}

// ── Load detected videos ───────────────────────────────────────────────────────
async function loadVideos() {
  return new Promise(resolve => {
    chrome.runtime.sendMessage({ action: "get_captured", tabId: currentTabId }, (res) => {
      if (chrome.runtime.lastError || !res) { resolve([]); return; }

      const items = [];
      const pageData = res.pageData; // YouTube data from document.js

      // YouTube / manifest from document.js interception
      if (pageData && (pageData.dash || pageData.hls || (pageData.formats && pageData.formats.length > 0))) {
        const manifestUrl = pageData.dash || pageData.hls || currentTabUrl;
        const mimeType    = pageData.dash ? "application/dash+xml" : "application/x-mpegURL";
        items.push({
          url:     manifestUrl,
          title:   pageData.title || currentTabTitle,
          type:    mimeType,
          size:    0,
          referer: "",
          _ytFmts: pageData.formats || [],
        });
      }

      // Network-captured videos
      for (const v of (res.captured || [])) {
        if (!items.find(i => i.url === v.url)) {
          items.push({ url: v.url, title: "", type: v.type || "", size: v.size || 0, referer: v.referer || "" });
        }
      }

      // Use tab title as fallback name for all items
      for (const item of items) {
        if (!item.title) item.title = currentTabTitle;
      }

      // Deduplicate: if a master playlist exists, remove variant index playlists
      const hasMaster = items.some(i => /master\.m3u8/i.test(i.url));
      const filtered  = hasMaster
        ? items.filter(i => !/index.*\.m3u8/i.test(i.url))
        : items;

      resolve(filtered);
    });
  });
}

async function refresh() {
  detectedItems = await loadVideos();

  // Try to grab thumbnails from content script
  if (currentTabId) {
    chrome.tabs.sendMessage(currentTabId, { action: "get_thumbnails" }, (res) => {
      if (chrome.runtime.lastError || !res) return;
      (res.thumbs || []).forEach(t => {
        const item = detectedItems.find(i => i.url === t.videoSrc || i.url === currentTabUrl);
        if (item) { item.thumb = t.poster; item.duration = t.duration; }
      });
      renderList();
    });
  }

  renderList();
}

// ── Init ──────────────────────────────────────────────────────────────────────
async function init() {
  // Health check
  fetch(`${BASE_URL}/health`)
    .then(r => r.ok ? r.json() : Promise.reject())
    .then(d => {
      dot.className   = "dot on";
      statusText.textContent = `Connected — port ${d.port || 1700}`;
    })
    .catch(() => {
      dot.className   = "dot off";
      statusText.textContent = "Not running";
    });

  // Get current tab
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab) {
    currentTabId    = tab.id;
    currentTabUrl   = tab.url   || "";
    currentTabTitle = tab.title || "";
  }

  await refresh();

  // Buttons
  document.getElementById("btnRefresh").addEventListener("click", refresh);

  document.getElementById("btnDlAll").addEventListener("click", async () => {
    for (const item of detectedItems) {
      const fmt = detectFmt(item.url, item.type);
      await downloadItem(item, fmt, "");
    }
    const btn = document.getElementById("btnDlAll");
    btn.textContent = `⬇ Sent ${detectedItems.length}`;
    setTimeout(() => { btn.textContent = "⬇ Download All"; }, 2000);
  });

  document.getElementById("btnClear").addEventListener("click", () => {
    detectedItems = [];
    renderList();
  });

  document.getElementById("btnOpenUI").addEventListener("click", () => {
    chrome.storage.local.get("fluxget_settings", (res) => {
      const token = (res.fluxget_settings || {}).token || "";
      const url   = token ? `${BASE_URL}/ui?token=${token}` : `${BASE_URL}/ui`;
      chrome.tabs.create({ url });
    });
  });

  // ── Disable / Enable toggle ──────────────────────────────────────────────────
  const btnToggle = document.getElementById("btnToggle");

  function updateToggleBtn() {
    if (interceptEnabled) {
      btnToggle.textContent = "⏸ Disable";
      btnToggle.className   = "toolbar-btn";
      btnToggle.title       = "Disable FluxGet download interception";
    } else {
      btnToggle.textContent = "▶ Enable";
      btnToggle.className   = "toolbar-btn disabled-state";
      btnToggle.title       = "Enable FluxGet download interception";
    }
  }

  // Load current state from settings
  chrome.storage.local.get("fluxget_settings", (res) => {
    interceptEnabled = (res.fluxget_settings?.interceptDownloads !== false);
    updateToggleBtn();
  });

  btnToggle.addEventListener("click", () => {
    interceptEnabled = !interceptEnabled;
    chrome.storage.local.get("fluxget_settings", (res) => {
      const s = Object.assign({ interceptDownloads: true }, res.fluxget_settings || {});
      s.interceptDownloads = interceptEnabled;
      chrome.storage.local.set({ fluxget_settings: s });
    });
    updateToggleBtn();
  });
}

init();
