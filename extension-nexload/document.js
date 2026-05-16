// FluxGet — document.js
// Runs in the PAGE context (MAIN world), not the extension context.
// Captures streaming data before the page can hide it, then sends it
// to content.js via window.postMessage using the __fluxget sentinel.
(function () {
  "use strict";
  if (window.__fluxget_doc__) return;
  window.__fluxget_doc__ = true;

  const TAG = "__fluxget";

  function post(obj) {
    window.postMessage(Object.assign({ [TAG]: true }, obj), "*");
  }

  // ── YouTube: ytInitialPlayerResponse ─────────────────────────
  // YouTube injects this global before DOMContentLoaded.
  // It contains dashManifestUrl, hlsManifestUrl, and all adaptiveFormats
  // with direct signed URLs — no API key needed.
  function probeYouTube() {
    const ipr = window.ytInitialPlayerResponse;
    if (!ipr || !ipr.streamingData) return false;
    const sd   = ipr.streamingData;
    const info = ipr.videoDetails || {};
    const formats = [
      ...(sd.adaptiveFormats || []),
      ...(sd.formats         || []),
    ].map(f => ({
      itag:      f.itag,
      mimeType:  f.mimeType  || "",
      quality:   f.qualityLabel || f.audioQuality || "",
      bitrate:   f.bitrate   || 0,
      width:     f.width     || 0,
      height:    f.height    || 0,
      url:       f.url       || "",          // direct CDN URL
      duration:  f.approxDurationMs,
    }));
    post({
      type:     "yt_player",
      manifest: sd.dashManifestUrl || sd.hlsManifestUrl || "",
      hls:      sd.hlsManifestUrl  || "",
      dash:     sd.dashManifestUrl || "",
      formats,
      title:    info.title        || document.title,
      duration: info.lengthSeconds || 0,
      videoId:  info.videoId      || "",
      author:   info.author       || "",
    });
    return true;
  }

  // Poll for ytInitialPlayerResponse (may load slightly after script runs)
  let ytAttempts = 0;
  const ytTimer = setInterval(() => {
    if (probeYouTube() || ++ytAttempts >= 30) clearInterval(ytTimer);
  }, 300);

  // Also hook ytInitialPlayerResponse assignment (YouTube SPA navigation)
  try {
    let _ytIPR = window.ytInitialPlayerResponse;
    Object.defineProperty(window, "ytInitialPlayerResponse", {
      configurable: true,
      get() { return _ytIPR; },
      set(v) {
        _ytIPR = v;
        if (v && v.streamingData) setTimeout(probeYouTube, 0);
      },
    });
  } catch (_) {}

  // ── Generic: intercept fetch() ────────────────────────────────
  // Catches any .m3u8 / .mpd response the page fetches itself.
  const _fetch = window.fetch;
  window.fetch = async function (...args) {
    const reqUrl = typeof args[0] === "string"
      ? args[0]
      : (args[0] instanceof Request ? args[0].url : "");
    // Skip non-http schemes (magnet:, blob:, data:, etc.) — native fetch rejects them too
    if (reqUrl && !/^https?:\/\//i.test(reqUrl)) return _fetch.apply(this, args);
    const res = await _fetch.apply(this, args);
    try {
      const ct = res.headers.get("content-type") || "";
      if (isManifestUrl(reqUrl) || isManifestCT(ct)) {
        res.clone().text().then(body => {
          post({ type: "manifest_intercepted", url: reqUrl, body, contentType: ct });
        }).catch(() => {});
      }
    } catch (_) {}
    return res;
  };

  // ── Generic: intercept XMLHttpRequest ─────────────────────────
  const _open = XMLHttpRequest.prototype.open;
  const _send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    this.__fg_url = String(url);
    return _open.call(this, method, url, ...rest);
  };
  XMLHttpRequest.prototype.send = function (...args) {
    this.addEventListener("load", function () {
      try {
        const url = this.__fg_url || "";
        const ct  = this.getResponseHeader("content-type") || "";
        if (isManifestUrl(url) || isManifestCT(ct)) {
          post({ type: "manifest_intercepted", url, body: this.responseText, contentType: ct });
        }
      } catch (_) {}
    });
    return _send.apply(this, args);
  };

  // ── MSE / SourceBuffer: detect DASH/HLS in Media Source ───────
  // When pages use blob: URLs (Netflix, Prime, etc.), they feed encrypted
  // segments via SourceBuffer. We capture the manifest URL from the
  // MediaSource.addSourceBuffer() call.
  const _addSB = MediaSource.prototype.addSourceBuffer;
  MediaSource.prototype.addSourceBuffer = function (mimeType) {
    post({ type: "mse_mime", mimeType });
    return _addSB.call(this, mimeType);
  };

  // ── JWPlayer interception ─────────────────────────────────────
  // JWPlayer stores HLS/DASH manifests in its setup config before they
  // become blob: URLs. We hook setup() to capture them early.
  function hookJWPlayer() {
    if (!window.jwplayer) return;
    const _jw = window.jwplayer;
    window.jwplayer = function (...args) {
      const player = _jw.apply(this, args);
      if (!player || typeof player.setup !== "function") return player;
      const _setup = player.setup.bind(player);
      player.setup = function (cfg) {
        extractJWConfig(cfg);
        return _setup(cfg);
      };
      return player;
    };
    Object.assign(window.jwplayer, _jw);
  }

  function extractJWConfig(cfg) {
    if (!cfg) return;
    const sources = [];
    const list = cfg.playlist || [cfg];
    for (const item of list) {
      for (const src of (item.sources || [])) {
        if (src.file) sources.push({ file: src.file, type: src.type || "", label: src.label || "" });
      }
      if (item.file) sources.push({ file: item.file, type: "", label: "" });
    }
    for (const s of sources) {
      if (isManifestUrl(s.file) || isManifestCT(s.type)) {
        post({ type: "manifest_intercepted", url: s.file, contentType: s.type || "application/x-mpegURL", label: s.label });
      }
    }
  }

  // ── Video.js / Plyr / Flowplayer interception ─────────────────
  // Intercept videojs() setup — used by many streaming sites
  function hookVideoJS() {
    if (!window.videojs) return;
    const _vjs = window.videojs;
    window.videojs = function (id, opts, ...rest) {
      const player = _vjs(id, opts, ...rest);
      if (opts && opts.sources) {
        for (const s of opts.sources) {
          if (s.src && (isManifestUrl(s.src) || isManifestCT(s.type || ""))) {
            post({ type: "manifest_intercepted", url: s.src, contentType: s.type || "application/x-mpegURL" });
          }
        }
      }
      return player;
    };
    Object.assign(window.videojs, _vjs);
  }

  // Try hooking now and after DOMContentLoaded (players may init late)
  hookJWPlayer();
  hookVideoJS();
  document.addEventListener("DOMContentLoaded", () => {
    hookJWPlayer();
    hookVideoJS();
  });

  // ── Helpers ───────────────────────────────────────────────────
  function isManifestUrl(url) {
    return /\.(m3u8|mpd)(\?|#|$)/i.test(url) || /(\/playlist|\/master|\/index|\/manifest)(\.m3u8|\.mpd)?(\?|$)/i.test(url);
  }
  function isManifestCT(ct) {
    return /mpegurl|dash\+xml|vnd\.apple\.mpegurl/i.test(ct);
  }
})();
