// FluxGet — document.js
// Runs in the PAGE context (MAIN world), not the extension context.
// Captures streaming data before the page can hide it, then sends it
// to content.js via window.postMessage using the __fluxget sentinel.
(function () {
  "use strict";
  if (window.__fluxget_doc__) return;
  window.__fluxget_doc__ = true;

  const TAG = "__fluxget";
  const IS_YOUTUBE = /(^|\.)youtube(-nocookie)?\.com$/i.test(location.hostname);

  function post(obj) {
    // Same-window only: content.js accepts messages whose source is this window.
    window.postMessage(Object.assign({ [TAG]: true }, obj), location.origin === "null" ? "*" : location.origin);
  }

  // ── YouTube: player response ─────────────────────────────────
  // ytInitialPlayerResponse is only present on the first page load. YouTube's
  // SPA navigation instead fetches /youtubei/v1/player, so we also parse those
  // responses (see the fetch hook below) — otherwise the formats of the
  // *previous* video would be downloaded after clicking a related video.
  let lastVideoId = "";
  function publishPlayerResponse(ipr) {
    if (!ipr || !ipr.streamingData) return false;
    const sd   = ipr.streamingData;
    const info = ipr.videoDetails || {};
    if (!info.videoId) return false;
    // Ignore preview/ad responses for a different video than the page shows.
    const pageVid = new URLSearchParams(location.search).get("v");
    if (pageVid && info.videoId !== pageVid && location.pathname === "/watch") return false;
    lastVideoId = info.videoId;
    const mapFmt = (f, progressive) => ({
      itag:        f.itag,
      mimeType:    f.mimeType  || "",
      quality:     f.qualityLabel || f.audioQuality || "",
      bitrate:     f.bitrate   || 0,
      width:       f.width     || 0,
      height:      f.height    || 0,
      fps:         f.fps       || 0,
      url:         f.url       || "",   // "" when the URL is signatureCipher-protected
      contentLength: Number(f.contentLength || 0),
      progressive: !!progressive,      // muxed audio+video (legacy "formats" list)
      audioTrack:  f.audioTrack ? (f.audioTrack.displayName || "") : "",
    });
    const formats = [
      ...(sd.adaptiveFormats || []).map(f => mapFmt(f, false)),
      ...(sd.formats         || []).map(f => mapFmt(f, true)),
    ];
    post({
      type:     "yt_player",
      hls:      sd.hlsManifestUrl  || "",
      dash:     sd.dashManifestUrl || "",
      formats,
      title:    info.title        || document.title,
      duration: Number(info.lengthSeconds || 0),
      videoId:  info.videoId      || "",
      author:   info.author       || "",
      isLive:   !!info.isLiveContent && !!sd.hlsManifestUrl,
    });
    return true;
  }

  if (IS_YOUTUBE) {
    let ytAttempts = 0;
    const ytTimer = setInterval(() => {
      if (publishPlayerResponse(window.ytInitialPlayerResponse) || ++ytAttempts >= 40) clearInterval(ytTimer);
    }, 300);

    try {
      let _ytIPR = window.ytInitialPlayerResponse;
      Object.defineProperty(window, "ytInitialPlayerResponse", {
        configurable: true,
        get() { return _ytIPR; },
        set(v) { _ytIPR = v; setTimeout(() => publishPlayerResponse(v), 0); },
      });
    } catch (_) {}

    // SPA navigation fires yt-navigate-finish; the player response for the new
    // video is in the page's player element shortly after.
    document.addEventListener("yt-navigate-finish", () => {
      setTimeout(() => {
        try {
          const p = document.querySelector("#movie_player");
          const r = p && typeof p.getPlayerResponse === "function" ? p.getPlayerResponse() : null;
          if (r && r.videoDetails && r.videoDetails.videoId !== lastVideoId) publishPlayerResponse(r);
        } catch (_) {}
      }, 800);
    });
  }

  // ── Generic: intercept fetch() ────────────────────────────────
  // Catches any .m3u8 / .mpd response the page fetches itself.
  const _fetch = window.fetch;
  window.fetch = async function (...args) {
    let reqUrl = "";
    try {
      const a0 = args[0];
      reqUrl = typeof a0 === "string" ? a0 : (a0 instanceof Request ? a0.url : (a0 && a0.href) || String(a0 || ""));
      reqUrl = new URL(reqUrl, location.href).href;
    } catch (_) {}
    // Skip non-http schemes (magnet:, blob:, data:, etc.)
    if (!/^https?:\/\//i.test(reqUrl)) return _fetch.apply(this, args);
    const res = await _fetch.apply(this, args);
    try {
      if (IS_YOUTUBE && /\/youtubei\/v1\/player(\?|$)/.test(reqUrl)) {
        res.clone().json().then(publishPlayerResponse).catch(() => {});
        return res;
      }
      const ct = res.headers.get("content-type") || "";
      if (isManifestUrl(reqUrl) || isManifestCT(ct) || maybeManifestUrl(reqUrl)) {
        res.clone().text().then(body => {
          if (looksLikeManifest(body)) {
            post({ type: "manifest_intercepted", url: res.url || reqUrl, contentType: ct, hasSubs: hasSubtitleTracks(body) });
          }
        }).catch(() => {});
      }
    } catch (_) {}
    return res;
  };

  // ── Generic: intercept XMLHttpRequest ─────────────────────────
  const _open = XMLHttpRequest.prototype.open;
  const _send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    try { this.__fg_url = new URL(String(url), location.href).href; } catch (_) { this.__fg_url = String(url); }
    return _open.call(this, method, url, ...rest);
  };
  XMLHttpRequest.prototype.send = function (...args) {
    this.addEventListener("load", function () {
      try {
        const url = this.responseURL || this.__fg_url || "";
        const ct  = this.getResponseHeader("content-type") || "";
        if (!(isManifestUrl(url) || isManifestCT(ct) || maybeManifestUrl(url))) return;
        // responseText throws for arraybuffer/blob/json response types.
        const textual = this.responseType === "" || this.responseType === "text";
        const body = textual ? this.responseText : "";
        if (!textual || looksLikeManifest(body)) {
          post({ type: "manifest_intercepted", url, contentType: ct, hasSubs: textual && hasSubtitleTracks(body) });
        }
      } catch (_) {}
    });
    return _send.apply(this, args);
  };

  // ── MSE / SourceBuffer: detect DASH/HLS in Media Source ───────
  if (typeof MediaSource !== "undefined" && MediaSource.prototype && MediaSource.prototype.addSourceBuffer) {
    const _addSB = MediaSource.prototype.addSourceBuffer;
    MediaSource.prototype.addSourceBuffer = function (mimeType) {
      post({ type: "mse_mime", mimeType: String(mimeType || "") });
      return _addSB.call(this, mimeType);
    };
  }

  // ── Player library hooks (JWPlayer / Video.js) ────────────────
  // Players store HLS/DASH manifests in their setup config before they become
  // blob: URLs. Hook the globals whenever they get assigned.
  function extractSources(list) {
    for (const s of list || []) {
      const file = s && (s.file || s.src);
      if (!file) continue;
      if (isManifestUrl(file) || isManifestCT(s.type || "")) {
        let abs = file;
        try { abs = new URL(file, location.href).href; } catch (_) {}
        post({ type: "manifest_intercepted", url: abs, contentType: s.type || "application/x-mpegURL", label: s.label || "" });
      }
    }
  }

  function wrapJW(jw) {
    if (!jw || jw.__fg_wrapped) return jw;
    const wrapped = function (...args) {
      const player = jw.apply(this, args);
      if (player && typeof player.setup === "function" && !player.__fg_setup) {
        const _setup = player.setup.bind(player);
        player.setup = function (cfg) {
          try {
            const items = (cfg && cfg.playlist) || [cfg];
            for (const it of items) {
              if (!it) continue;
              extractSources(it.sources);
              if (it.file) extractSources([{ file: it.file }]);
            }
          } catch (_) {}
          return _setup(cfg);
        };
        player.__fg_setup = true;
      }
      return player;
    };
    Object.assign(wrapped, jw);
    wrapped.__fg_wrapped = true;
    return wrapped;
  }

  function wrapVJS(vjs) {
    if (!vjs || vjs.__fg_wrapped) return vjs;
    const wrapped = function (id, opts, ...rest) {
      try { if (opts && opts.sources) extractSources(opts.sources); } catch (_) {}
      const player = vjs(id, opts, ...rest);
      try {
        if (player && typeof player.src === "function" && !player.__fg_src) {
          const _src = player.src.bind(player);
          player.src = function (s) {
            if (s) extractSources(Array.isArray(s) ? s : [typeof s === "string" ? { src: s } : s]);
            return _src(s);
          };
          player.__fg_src = true;
        }
      } catch (_) {}
      return player;
    };
    Object.assign(wrapped, vjs);
    wrapped.__fg_wrapped = true;
    return wrapped;
  }

  function hookGlobal(name, wrap) {
    try {
      let current = wrap(window[name]);
      Object.defineProperty(window, name, {
        configurable: true,
        get() { return current; },
        set(v) { current = wrap(v); },
      });
    } catch (_) {}
  }
  hookGlobal("jwplayer", wrapJW);
  hookGlobal("videojs", wrapVJS);

  // ── Helpers ───────────────────────────────────────────────────
  function isManifestUrl(url) {
    return /\.(m3u8|mpd)(\?|#|$)/i.test(url);
  }
  // Extension-less manifest paths (/playlist, /master, /manifest…) — only
  // reported once the body confirms it really is a playlist.
  function maybeManifestUrl(url) {
    return /\/(playlist|master|manifest|index|chunklist)[^/]*?(\?|#|$)/i.test(url) && !/youtube\.com\/playlist/i.test(url);
  }
  function isManifestCT(ct) {
    return /mpegurl|dash\+xml/i.test(ct);
  }
  function looksLikeManifest(body) {
    if (!body) return false;
    const head = body.slice(0, 2048).trimStart();
    return head.startsWith("#EXTM3U") || /<MPD[\s>]/.test(head);
  }
  function hasSubtitleTracks(body) {
    return /#EXT-X-MEDIA:[^\n]*TYPE=SUBTITLES/i.test(body || "") || /contentType="text"|mimeType="(text\/vtt|application\/ttml)/i.test(body || "");
  }
})();
