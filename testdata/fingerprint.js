// What a page's scripts can learn about the device it runs on — the values a
// device profile (device.go) sets, and their neighbours. Evaluates to a
// promise of one JSON object: browser_fingerprint runs it in a session, and
// pasted into the DevTools console of a real browser it logs the same object
// (and copies it to the clipboard), so the two can be compared side by side.
(async () => {
  const out = {};
  const n = navigator;
  out.userAgent = n.userAgent;
  out.platform = n.platform;
  out.appVersion = n.appVersion;
  out.vendor = n.vendor;
  out.language = n.language;
  out.languages = n.languages;
  out.hardwareConcurrency = n.hardwareConcurrency;
  out.deviceMemory = n.deviceMemory;
  out.maxTouchPoints = n.maxTouchPoints;
  out.webdriver = n.webdriver;
  out.pdfViewerEnabled = n.pdfViewerEnabled;
  out.plugins = Array.from(n.plugins).map((p) => p.name);
  out.doNotTrack = n.doNotTrack;
  out.userAgentData = null;
  if (n.userAgentData) {
    out.userAgentData = {brands: n.userAgentData.brands, mobile: n.userAgentData.mobile, platform: n.userAgentData.platform};
    try {
      out.userAgentData.highEntropy = await n.userAgentData.getHighEntropyValues(
        ['architecture', 'bitness', 'model', 'platformVersion', 'uaFullVersion', 'fullVersionList', 'wow64', 'formFactors']);
    } catch (e) {
      out.userAgentData.highEntropy = String(e);
    }
  }
  out.timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  out.timezoneOffset = new Date().getTimezoneOffset();
  out.intlLocale = Intl.DateTimeFormat().resolvedOptions().locale;
  out.screen = {
    width: screen.width, height: screen.height, availWidth: screen.availWidth, availHeight: screen.availHeight,
    colorDepth: screen.colorDepth, pixelDepth: screen.pixelDepth, orientation: screen.orientation && screen.orientation.type,
  };
  out.window = {innerWidth, innerHeight, outerWidth, outerHeight, devicePixelRatio, screenX, screenY};
  out.media = {};
  for (const q of ['(hover: hover)', '(pointer: fine)', '(prefers-color-scheme: dark)', '(prefers-reduced-motion: reduce)']) {
    out.media[q] = matchMedia(q).matches;
  }
  out.webgl = null;
  try {
    const gl = document.createElement('canvas').getContext('webgl');
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    out.webgl = {
      vendor: gl.getParameter(gl.VENDOR), renderer: gl.getParameter(gl.RENDERER),
      unmaskedVendor: ext && gl.getParameter(ext.UNMASKED_VENDOR_WEBGL),
      unmaskedRenderer: ext && gl.getParameter(ext.UNMASKED_RENDERER_WEBGL),
      version: gl.getParameter(gl.VERSION), maxTextureSize: gl.getParameter(gl.MAX_TEXTURE_SIZE),
      getParameterSource: gl.getParameter.toString(),
    };
  } catch (e) {
    out.webgl = String(e);
  }
  // Font presence, the way fingerprinting scripts test it: text set in an
  // installed font measures differently from the generic fallback.
  out.fonts = {};
  const c2d = document.createElement('canvas').getContext('2d');
  const measure = (font) => { c2d.font = '72px ' + font; return c2d.measureText('mmmmmmmmmmlliI1!@#').width; };
  const generic = ['monospace', 'sans-serif', 'serif'];
  const base = generic.map(measure);
  for (const f of ['Segoe UI', 'Calibri', 'Cambria', 'Consolas', 'Tahoma', 'Verdana', 'Arial', 'Times New Roman',
                   'Courier New', 'Helvetica Neue', 'DejaVu Sans', 'Liberation Sans', 'Noto Sans', 'Roboto', 'Ubuntu']) {
    out.fonts[f] = generic.some((g, i) => measure('"' + f + '", ' + g) !== base[i]);
  }
  // The CSS generic families' own metrics: scripts read these to tell one
  // OS/browser from another (on Windows Chrome `fantasy` is Impact, narrower
  // than the `system-ui` body font; the Linux fallbacks reverse that).
  out.genericFonts = {};
  for (const g of ['system-ui', 'fantasy', 'cursive', 'monospace', 'sans-serif', 'serif', '-apple-system']) {
    out.genericFonts[g] = Math.round(measure(g));
  }
  // The same from a worker, where a profile has to hold as well.
  out.worker = null;
  try {
    out.worker = await new Promise((resolve) => {
      const src = `postMessage({userAgent: navigator.userAgent, platform: navigator.platform,
        hardwareConcurrency: navigator.hardwareConcurrency, languages: navigator.languages,
        timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        userAgentData: navigator.userAgentData ? {brands: navigator.userAgentData.brands, platform: navigator.userAgentData.platform, mobile: navigator.userAgentData.mobile} : null,
        webgl: (() => { try { const gl = new OffscreenCanvas(1, 1).getContext('webgl'); const ext = gl.getExtension('WEBGL_debug_renderer_info'); return ext && gl.getParameter(ext.UNMASKED_RENDERER_WEBGL); } catch (e) { return String(e); } })()})`;
      const w = new Worker(URL.createObjectURL(new Blob([src], {type: 'text/javascript'})));
      w.onmessage = (ev) => { resolve(ev.data); w.terminate(); };
      w.onerror = (ev) => resolve('error: ' + ev.message);
      setTimeout(() => resolve('timeout'), 5000);
    });
  } catch (e) {
    out.worker = String(e);
  }
  // In a DevTools console: show it and put it on the clipboard.
  if (typeof copy === 'function') {
    const text = JSON.stringify(out, null, 2);
    console.log(text);
    try { copy(text); } catch (e) {}
  }
  return out;
})()
