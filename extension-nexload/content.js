(function () {
  "use strict";
  if (window.__fluxget_injected__) return;
  window.__fluxget_injected__ = true;

  const TAG = "__fluxget";

  // ── Styles ────────────────────────────────────────────────────────────────────
  const STYLE = `
    .fg-btn-wrap {
      position: fixed;
      z-index: 2147483647;
      display: flex;
      align-items: stretch;
      pointer-events: auto;
      user-select: none;
      box-shadow: 0 4px 16px rgba(0,0,0,0.55);
      border-radius: 7px;
    }
    .fg-btn {
      background: rgba(15,15,20,0.92);
      color: #fff;
      border: 1px solid rgba(255,255,255,0.15);
      border-right: none;
      border-radius: 7px 0 0 7px;
      padding: 5px 12px;
      font-size: 12px;
      font-family: -apple-system, 'Segoe UI', sans-serif;
      font-weight: 600;
      cursor: pointer;
      display: flex;
      align-items: center;
      gap: 6px;
      transition: background 0.12s, border-color 0.12s;
      white-space: nowrap;
      pointer-events: auto;
      user-select: none;
      letter-spacing: 0.01em;
    }
    .fg-btn:hover { background: rgba(0,112,200,0.92); border-color: rgba(0,160,255,0.6); }
    .fg-btn svg  { flex-shrink: 0; }
    .fg-close {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 22px;
      background: rgba(15,15,20,0.92);
      color: rgba(255,255,255,0.4);
      border: 1px solid rgba(255,255,255,0.15);
      border-radius: 0 7px 7px 0;
      cursor: pointer;
      font-size: 15px;
      line-height: 1;
      transition: background 0.12s, color 0.12s, border-color 0.12s;
      flex-shrink: 0;
      padding: 0;
      font-family: inherit;
    }
    .fg-close:hover { background: rgba(180,30,30,0.9); color: #fff; border-color: rgba(255,80,80,0.5); }

    .fg-quality-panel {
      position: fixed;
      z-index: 2147483641;
      background: rgba(18,18,24,0.97);
      border: 1px solid rgba(255,255,255,0.18);
      border-radius: 8px;
      padding: 6px 0;
      box-shadow: 0 8px 28px rgba(0,0,0,0.7);
      font-family: -apple-system, 'Segoe UI', sans-serif;
      font-size: 12px;
      min-width: 160px;
    }
    .fg-quality-panel .fg-q-title {
      color: #aaa;
      font-size: 11px;
      font-weight: 600;
      padding: 8px 14px 4px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      max-width: 220px;
    }
    .fg-q-sep { height: 1px; background: rgba(255,255,255,0.1); margin: 4px 0; }
    .fg-q-row {
      display: flex;
      align-items: center;
      gap: 8px;
      width: 100%;
      background: none;
      border: none;
      color: #fff;
      padding: 7px 14px;
      text-align: left;
      cursor: pointer;
      font-size: 12px;
      font-family: inherit;
      transition: background 0.1s;
    }
    .fg-q-row:hover { background: rgba(0,112,200,0.55); }
    .fg-q-audio { color: #a8d8ff; }
    .fg-q-icon { font-size: 11px; opacity: 0.7; width: 12px; flex-shrink: 0; }
    .fg-q-label { flex: 1; font-weight: 500; }
    .fg-quality-badge {
      background: rgba(0,112,200,0.8);
      color: #fff;
      border-radius: 4px;
      font-size: 10px;
      padding: 1px 6px;
      flex-shrink: 0;
      pointer-events: none;
    }
  `;

  const styleEl = document.createElement("style");
  styleEl.textContent = STYLE;
  (document.head || document.documentElement).appendChild(styleEl);

  // ── State ─────────────────────────────────────────────────────────────────────
  const tracked   = new Map(); // video → { btn, panel, rafId }
  const dismissed = new Set(); // videos the user explicitly closed — not re-shown until SPA nav
  let   qualities = [];        // picker options from background.js
  let   pageTitle = "";

  // Fallback quality options when no format list was captured: the backend
  // applies the height cap to whatever it finds (HLS/DASH variant or yt-dlp).
  const FALLBACK_QUALITIES = [
    { label: "Best quality", maxHeight: 0,    type: "video", codec: "Auto" },
    { label: "1080p",        maxHeight: 1080, type: "video", codec: "" },
    { label: "720p",         maxHeight: 720,  type: "video", codec: "" },
    { label: "480p",         maxHeight: 480,  type: "video", codec: "" },
    { label: "Audio only",   maxHeight: 0,    type: "audio", codec: "", audioOnly: true },
  ];
  let qualitiesVideoId = "";

  // On load, request cached quality data (fixes race with document.js)
  chrome.runtime.sendMessage({ action: "get_quality_data" }, (res) => {
    if (chrome.runtime.lastError) return;
    if (res && res.options && res.options.length > 0) {
      qualities = res.options;
      pageTitle = res.title || "";
      qualitiesVideoId = res.videoId || "";
      tracked.forEach(({ btn }) => resetBtn(btn));
    }
  });

  function fmtSize(n) {
    if (!n) return "";
    if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
    if (n >= 1 << 20) return Math.round(n / (1 << 20)) + " MB";
    return Math.round(n / 1024) + " KB";
  }

  // ── SVG icon ──────────────────────────────────────────────────────────────────
  function makeIcon() {
    const NS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(NS, "svg");
    svg.setAttribute("width", "13"); svg.setAttribute("height", "13");
    svg.setAttribute("viewBox", "0 0 16 16"); svg.setAttribute("fill", "currentColor");
    const arrow = document.createElementNS(NS, "path");
    arrow.setAttribute("d", "M8 12l-5-5h3V2h4v5h3z");
    const bar = document.createElementNS(NS, "rect");
    bar.setAttribute("x", "2"); bar.setAttribute("y", "13");
    bar.setAttribute("width", "12"); bar.setAttribute("height", "2"); bar.setAttribute("rx", "1");
    svg.appendChild(arrow); svg.appendChild(bar);
    return svg;
  }

  // ── Quality panel (IDM-style) ─────────────────────────────────────────────────
  function makeQualityPanel(video, anchorBtn, opts, subtitleSource) {
    const panel = document.createElement("div");
    panel.className = "fg-quality-panel";

    const header = document.createElement("div");
    header.className = "fg-q-title";
    header.textContent = (pageTitle || document.title || "").slice(0, 40) || "Select quality";
    panel.appendChild(header);

    const sep = document.createElement("div");
    sep.className = "fg-q-sep";
    panel.appendChild(sep);

    for (const opt of opts) {
      const row = document.createElement("button");
      row.className = "fg-q-row" + (opt.type === "audio" ? " fg-q-audio" : "");

      const icon = document.createElement("span");
      icon.className = "fg-q-icon";
      icon.textContent = opt.type === "audio" ? "♪" : "▶";

      const lbl = document.createElement("span");
      lbl.className = "fg-q-label";
      lbl.textContent = opt.label;

      const badge = document.createElement("span");
      badge.className = "fg-quality-badge";
      badge.textContent = [opt.codec, fmtSize(opt.size)].filter(Boolean).join(" \u00B7 ") || "Auto";

      row.appendChild(icon);
      row.appendChild(lbl);
      row.appendChild(badge);

      row.addEventListener("click", (e) => {
        e.stopPropagation();
        chrome.runtime.sendMessage({
          action:      "download_quality",
          maxHeight:   opt.maxHeight || 0,
          audioOnly:   !!opt.audioOnly || opt.type === "audio",
          manifestUrl: opt.manifestUrl || "",
          title:       pageTitle || document.title || "",
        });
        panel.remove();
        anchorBtn.innerHTML = "";
        anchorBtn.appendChild(makeIcon());
        anchorBtn.appendChild(document.createTextNode(" Sent ✓"));
        setTimeout(() => resetBtn(anchorBtn), 2500);
      });

      panel.appendChild(row);
    }

    // Subtitles-only row — shown ONLY when the master playlist declares subtitle
    // tracks. Downloads all caption tracks as .srt (no video).
    if (subtitleSource) {
      const subSep = document.createElement("div");
      subSep.className = "fg-q-sep";
      panel.appendChild(subSep);

      const subRow = document.createElement("button");
      subRow.className = "fg-q-row";
      const subIcon = document.createElement("span");
      subIcon.className = "fg-q-icon";
      subIcon.textContent = "💬";
      const subLbl = document.createElement("span");
      subLbl.className = "fg-q-label";
      subLbl.textContent = "Subtitles only";
      const subBadge = document.createElement("span");
      subBadge.className = "fg-quality-badge";
      subBadge.textContent = "SRT";
      subRow.appendChild(subIcon);
      subRow.appendChild(subLbl);
      subRow.appendChild(subBadge);
      subRow.addEventListener("click", (e) => {
        e.stopPropagation();
        chrome.runtime.sendMessage({
          action: "download_subtitles",
          url: subtitleSource, // the master playlist (where subtitles live)
          title: pageTitle || document.title || "",
          pageUrl: window.location.href,
        });
        panel.remove();
        anchorBtn.innerHTML = "";
        anchorBtn.appendChild(makeIcon());
        anchorBtn.appendChild(document.createTextNode(" Subs ✓"));
        setTimeout(() => resetBtn(anchorBtn), 2500);
      });
      panel.appendChild(subRow);
    }

    // Close on outside click
    setTimeout(() => {
      document.addEventListener("click", function close(e) {
        if (!panel.contains(e.target)) { panel.remove(); document.removeEventListener("click", close); }
      });
    }, 0);

    document.documentElement.appendChild(panel);
    return panel;
  }

  function positionPanel(panel, anchorBtn) {
    const r = anchorBtn.getBoundingClientRect();
    panel.style.top  = (r.bottom + 4) + "px";
    panel.style.left = r.left + "px";
  }

  // ── Button ────────────────────────────────────────────────────────────────────
  function resetBtn(btn) {
    btn.innerHTML = "";
    btn.appendChild(makeIcon());
    btn.appendChild(document.createTextNode(" FluxGet"));
    if (qualities.length > 0) {
      const badge = document.createElement("span");
      badge.className = "fg-quality-badge";
      badge.textContent = `${qualities.length}Q`;
      btn.appendChild(badge);
    }
  }

  function makeButton(video) {
    const wrap = document.createElement("div");
    wrap.className = "fg-btn-wrap";

    const btn = document.createElement("button");
    btn.className = "fg-btn";
    resetBtn(btn);
    btn.title = "Download with FluxGet";

    btn.addEventListener("click", async (e) => {
      e.stopPropagation();
      e.preventDefault();

      const existing = document.querySelector(".fg-quality-panel");
      if (existing) { existing.remove(); return; }

      let pickerQualities = qualities;
      let subtitleSource = ""; // master playlist URL, set only when it declares subtitles

      // No YouTube data \u2014 inspect a captured HLS master for this tab. The
      // fetch runs in the service worker: content-script fetches are bound by
      // the page's CORS policy and fail on most CDNs.
      if (pickerQualities.length === 0) {
        const captured = await new Promise(resolve =>
          chrome.runtime.sendMessage({ action: "get_captured" }, r => resolve(r || {}))
        );
        const list = captured.captured || [];
        const hlsItem = [...list].reverse().find(v => v.hasSubs || v.isMaster) ||
          [...list].reverse().find(v => /\.m3u8/i.test(v.url) || /mpegurl/i.test(v.type || ""));
        if (hlsItem) {
          const info = await new Promise(resolve =>
            chrome.runtime.sendMessage({ action: "inspect_hls", url: hlsItem.url }, r => resolve(r || {}))
          );
          const variants = info.variants || [];
          // Quality = height cap on the *master* URL, so the backend keeps the
          // variant's separate audio group and the subtitle renditions.
          pickerQualities = variants.map((v, i) => ({
            label:       v.height ? `${v.height}p${i === 0 ? " (Best)" : ""}` : `Stream ${i + 1}`,
            maxHeight:   v.height || 0,
            codec:       v.bandwidth ? `${(v.bandwidth / 1e6).toFixed(1)} Mbps` : "HLS",
            type:        "video",
            manifestUrl: hlsItem.url,
          }));
          if (variants.length) {
            pickerQualities.push({ label: "Audio only", type: "audio", audioOnly: true, codec: "HLS", manifestUrl: hlsItem.url });
          }
          if (info.hasSubs) subtitleSource = hlsItem.url;
        }
      }

      if (pickerQualities.length === 0) pickerQualities = FALLBACK_QUALITIES;
      const panel = makeQualityPanel(video, btn, pickerQualities, subtitleSource);
      positionPanel(panel, btn);
    });

    // Close "×" — dismisses the overlay for this video without downloading.
    // Uses pointerdown+capture so player overlays that intercept click events
    // (common on sites like missav) cannot swallow the dismiss action.
    const closeBtn = document.createElement("button");
    closeBtn.className = "fg-close";
    closeBtn.type = "button";
    closeBtn.textContent = "×";
    closeBtn.title = "Close";

    function doClose(e) {
      e.stopPropagation();
      e.stopImmediatePropagation();
      e.preventDefault();
      dismissed.add(video); // prevent MutationObserver from re-adding it
      const entry = tracked.get(video);
      if (entry) {
        cancelAnimationFrame(entry.rafId);
        tracked.delete(video);
      }
      wrap.remove();
    }
    closeBtn.addEventListener("pointerdown", doClose, { capture: true });
    closeBtn.addEventListener("click",       doClose, { capture: true });

    wrap.appendChild(btn);
    wrap.appendChild(closeBtn);
    document.documentElement.appendChild(wrap);
    return { wrap, btn };
  }

  function positionButton(video, btn) {
    const rect = video.getBoundingClientRect();
    if (rect.width < 80 || rect.height < 50 || rect.bottom < 0 || rect.top > window.innerHeight) {
      btn.style.display = "none";
      return;
    }
    const M = 8;
    btn.style.display = "flex";
    btn.style.top  = Math.max(rect.top  + M, M) + "px";
    btn.style.left = Math.max(rect.left + M, M) + "px";
  }

  function trackVideo(video) {
    if (tracked.has(video)) return;
    if (dismissed.has(video)) return;
    const { wrap, btn } = makeButton(video);

    function loop() {
      if (!document.contains(video)) { wrap.remove(); tracked.delete(video); return; }
      positionButton(video, wrap);
      tracked.get(video).rafId = requestAnimationFrame(loop);
    }

    const entry = { btn, wrap, rafId: null };
    tracked.set(video, entry);
    entry.rafId = requestAnimationFrame(loop);
  }

  function scanForVideos() {
    document.querySelectorAll("video").forEach((v) => {
      if (v.offsetWidth >= 200 && v.offsetHeight >= 120) trackVideo(v);
    });
  }

  // ── URL resolution ─────────────────────────────────────────────────────────────
  function resolveVideoUrl(video) {
    if (video.src && !video.src.startsWith("blob:")) return video.src;
    for (const src of video.querySelectorAll("source")) {
      if (src.src && !src.src.startsWith("blob:")) return src.src;
    }
    return window.location.href;
  }

  // ── Relay: document.js (MAIN world) → background.js ──────────────────────────
  // document.js posts messages tagged with __fluxget; we relay them to the SW.
  window.addEventListener("message", (event) => {
    // Only our own frame's MAIN-world script (document.js); ignore other frames.
    if (event.source !== window || !event.data || !event.data[TAG]) return;
    const msg = event.data;

    if (msg.type === "yt_player") {
      chrome.runtime.sendMessage({
        action:  "yt_player",
        dash:    msg.dash    || "",
        hls:     msg.hls     || "",
        formats: Array.isArray(msg.formats) ? msg.formats : [],
        title:   msg.title   || "",
        videoId: msg.videoId || "",
      });
      return;
    }

    if (msg.type === "manifest_intercepted") {
      chrome.runtime.sendMessage({
        action:      "manifest_intercepted",
        url:         String(msg.url || ""),
        contentType: msg.contentType || "",
        hasSubs:     !!msg.hasSubs,
      });
      return;
    }

    // mse_mime — informational; background doesn't need it yet
  });

  // ── Messages from background.js ───────────────────────────────────────────────
  chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
    if (msg.action === "video_qualities") {
      qualities = msg.options || [];
      pageTitle = msg.title  || "";
      qualitiesVideoId = msg.videoId || "";
      tracked.forEach(({ btn }) => resetBtn(btn));
      return;
    }

    if (msg.action === "get_thumbnails") {
      const thumbs = [];
      document.querySelectorAll("video").forEach(v => {
        if (v.offsetWidth < 100) return;
        thumbs.push({
          videoSrc: v.src || v.currentSrc || "",
          poster:   v.poster || "",
          duration: v.duration ? formatDur(v.duration) : "",
        });
      });
      sendResponse({ thumbs });
      return true;
    }

    if (msg.action === "get_link") {
      const anchor = lastContextTarget && lastContextTarget.closest("a[href]");
      sendResponse({
        href:     anchor ? anchor.href : null,
        referrer: window.location.href,
        text:     anchor ? (anchor.textContent || "").trim().slice(0, 80) : "",
      });
      return true;
    }
  });

  // ── Observe DOM ───────────────────────────────────────────────────────────────
  // Debounced: heavy SPAs mutate the DOM hundreds of times per second.
  let scanTimer = 0;
  new MutationObserver(() => {
    if (scanTimer) return;
    scanTimer = setTimeout(() => { scanTimer = 0; scanForVideos(); }, 400);
  }).observe(document.documentElement, { childList: true, subtree: true });
  scanForVideos();
  window.addEventListener("load", scanForVideos);

  // SPA navigation re-scan
  let lastHref = location.href;
  setInterval(() => {
    if (location.href !== lastHref) {
      lastHref = location.href;
      qualities = [];       // clear stale quality data on navigation
      qualitiesVideoId = "";
      dismissed.clear();    // allow overlay to reappear on new page/video
      tracked.forEach(({ btn }) => resetBtn(btn));
      setTimeout(scanForVideos, 1500);
      // The new video's player data arrives shortly after SPA navigation.
      setTimeout(() => {
        chrome.runtime.sendMessage({ action: "get_quality_data" }, (res) => {
          if (chrome.runtime.lastError || !res || !res.options || !res.options.length) return;
          qualities = res.options;
          pageTitle = res.title || "";
          qualitiesVideoId = res.videoId || "";
          tracked.forEach(({ btn }) => resetBtn(btn));
        });
      }, 2000);
    }
  }, 500);

  function formatDur(s) {
    s = Math.round(s);
    const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
    if (h > 0) return `${h}:${String(m).padStart(2,"0")}:${String(sec).padStart(2,"0")}`;
    return `${m}:${String(sec).padStart(2,"0")}`;
  }

  // ── Right-click target tracking ───────────────────────────────────────────────
  let lastContextTarget = null;
  document.addEventListener("contextmenu", (e) => { lastContextTarget = e.target; }, true);
})();
