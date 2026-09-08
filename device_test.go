package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseChromeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"Google Chrome 152.0.7977.82 \n": "152.0.7977.82",
		"Chromium 140.0.7339.80":         "140.0.7339.80",
	} {
		v, err := parseChromeVersion(in)
		if err != nil || v.Full != want || v.Major != want[:3] {
			t.Errorf("%q: got %+v %v", in, v, err)
		}
	}
	if _, err := parseChromeVersion("no version here"); err == nil {
		t.Error("want error")
	}
}

func TestBrandVersions(t *testing.T) {
	// Chrome 152 sends exactly this (seen live); the integration test
	// checks the algorithm against whatever Chrome is on PATH.
	got := brandVersions("152", "152.0.7977.82", false)
	want := []string{"Chromium/152", "Not?A_Brand/24", "Google Chrome/152"}
	for i, b := range got {
		if s := b.Brand + "/" + b.Version; s != want[i] {
			t.Errorf("brand %d: %s, want %s", i, s, want[i])
		}
	}
	full := brandVersions("152", "152.0.7977.82", true)
	if full[0].Version != "152.0.7977.82" || full[1].Version != "24.0.0.0" {
		t.Errorf("full versions: %+v %+v", full[0], full[1])
	}
}

func TestDeviceProfiles(t *testing.T) {
	if _, err := lookupDevice("android"); err == nil || !strings.Contains(err.Error(), "windows") {
		t.Errorf("unknown device: %v", err)
	}
	d, err := lookupDevice("")
	if err != nil || d.Name != "default" || d.emulated() {
		t.Fatalf("default: %+v %v", d, err)
	}
	ver := chromeVersion{Full: "152.0.7977.82", Major: "152"}
	if d.userAgentOverride(ver) != nil || d.initScript(ver) != "" || d.chromeFlags(ver) != nil {
		t.Error("default profile emulates something")
	}
	if m := d.metrics(1280, 800, false); m != nil {
		t.Errorf("default headful metrics: %+v", m)
	}
	if m := d.metrics(1280, 800, true); m == nil || m.Width != 1280 || m.Height != 800 || m.ScreenWidth != 0 || m.DeviceScaleFactor != 1 {
		t.Errorf("default headless metrics: %+v", m)
	}

	w, err := lookupDevice("windows")
	if err != nil || !w.emulated() {
		t.Fatal(err)
	}
	ua := w.userAgentOverride(ver)
	if ua.UserAgent != "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36" {
		t.Errorf("ua: %s", ua.UserAgent)
	}
	if ua.Platform != "Win32" || ua.UserAgentMetadata.Platform != "Windows" || ua.UserAgentMetadata.Architecture != "x86" || ua.UserAgentMetadata.Mobile {
		t.Errorf("metadata: %+v", ua.UserAgentMetadata)
	}
	if f := w.chromeFlags(ver); len(f) != 2 || !strings.HasPrefix(f[0], "--user-agent=Mozilla/5.0 (Windows") || !strings.Contains(f[1], "primaryPointerType=4") {
		t.Errorf("flags: %v", f)
	}
	if m := w.metrics(1280, 800, true); m.Width != 1280 || m.ScreenWidth != 1920 || m.ScreenHeight != 1080 {
		t.Errorf("windows headless metrics: %+v", m)
	}
	if m := w.metrics(1280, 800, false); m.Width != 0 || m.Height != 0 || m.ScreenWidth != 1920 {
		t.Errorf("windows headful metrics: %+v", m)
	}
	// A viewport wider than the display makes the display that wide.
	if m := w.metrics(2560, 1440, true); m.ScreenWidth != 2560 || m.ScreenHeight != 1440 {
		t.Errorf("large viewport metrics: %+v", m)
	}
	js := w.initScript(ver)
	for _, want := range []string{`"Win32"`, `"Google Inc. (NVIDIA)"`, `"152.0.7977.82"`, "webdriver", "getParameter"} {
		if !strings.Contains(js, want) {
			t.Errorf("init script lacks %s", want)
		}
	}
}

// fingerprint is the decoded output of testdata/fingerprint.js.
type fingerprint struct {
	UserAgent     string   `json:"userAgent"`
	Platform      string   `json:"platform"`
	Language      string   `json:"language"`
	Languages     []string `json:"languages"`
	Webdriver     bool     `json:"webdriver"`
	Timezone      string   `json:"timezone"`
	UserAgentData *struct {
		Platform    string `json:"platform"`
		Brands      []struct{ Brand, Version string }
		HighEntropy struct {
			Architecture    string                            `json:"architecture"`
			PlatformVersion string                            `json:"platformVersion"`
			UaFullVersion   string                            `json:"uaFullVersion"`
			FullVersionList []struct{ Brand, Version string } `json:"fullVersionList"`
		} `json:"highEntropy"`
	} `json:"userAgentData"`
	Screen struct{ Width, Height, AvailHeight int }
	Window struct {
		InnerWidth       int     `json:"innerWidth"`
		InnerHeight      int     `json:"innerHeight"`
		OuterWidth       int     `json:"outerWidth"`
		OuterHeight      int     `json:"outerHeight"`
		DevicePixelRatio float64 `json:"devicePixelRatio"`
	}
	Media    map[string]bool `json:"media"`
	WebGLRaw json.RawMessage `json:"webgl"` // an object, or an error string where there is no WebGL
	WebGL    struct {
		UnmaskedVendor     string `json:"unmaskedVendor"`
		UnmaskedRenderer   string `json:"unmaskedRenderer"`
		GetParameterSource string `json:"getParameterSource"`
	}
	Fonts  map[string]bool `json:"fonts"`
	Worker json.RawMessage `json:"worker"`
}

func readFingerprint(ctx context.Context, t *testing.T, tb *tab) *fingerprint {
	t.Helper()
	out, err := evaluate(ctx, tb, fingerprintScript, 20*time.Second)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	var fp fingerprint
	if err := json.Unmarshal([]byte(out), &fp); err != nil {
		t.Fatalf("fingerprint json: %v\n%s", err, out)
	}
	if err := json.Unmarshal(fp.WebGLRaw, &fp.WebGL); err != nil {
		t.Errorf("no webgl: %s", fp.WebGLRaw)
	}
	return &fp
}

// echoServer records the headers of every request and asks for the
// high-entropy client hints, the way a site interested in them does.
type echoServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs map[string][]http.Header
}

func newEchoServer() *echoServer {
	es := &echoServer{reqs: map[string][]http.Header{}}
	es.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.reqs[r.URL.Path] = append(es.reqs[r.URL.Path], r.Header.Clone())
		es.mu.Unlock()
		w.Header().Set("Accept-CH", "Sec-CH-UA-Platform-Version, Sec-CH-UA-Arch, Sec-CH-UA-Bitness, Sec-CH-UA-Full-Version-List, Sec-CH-UA-Model, Sec-CH-UA-WoW64, Sec-CH-UA-Form-Factors")
		switch r.URL.Path {
		case "/popup":
			w.Write([]byte(`<title>popup</title><p>popup</p>`))
		case "/sw.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Write([]byte(`self.addEventListener('install', () => { fetch('/from-sw'); });`))
		default:
			w.Write([]byte(`<title>echo</title><a id="pop" href="/popup" target="_blank">open popup</a>` +
				`<script>navigator.serviceWorker.register('/sw.js').then(() => fetch('/from-page'))</script>`))
		}
	}))
	return es
}

// last is the most recent request to a path, waiting a moment for it.
func (es *echoServer) last(path string) http.Header {
	for i := 0; i < 40; i++ {
		es.mu.Lock()
		hs := es.reqs[path]
		es.mu.Unlock()
		if len(hs) > 0 {
			return hs[len(hs)-1]
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func TestDeviceIntegration(t *testing.T) {
	mgr := testChrome(t)
	es := newEchoServer()
	defer es.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ver, err := mgr.chromeVersion()
	if err != nil {
		t.Fatal(err)
	}

	// The default profile is Chrome as it is — and its own brand list is
	// what the algorithm must reproduce for this version.
	s, err := mgr.start(ctx, startOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, es.URL, 20*time.Second); err != nil {
			return err
		}
		fp := readFingerprint(ctx, t, tb)
		if !strings.Contains(fp.UserAgent, "Linux") || fp.UserAgentData == nil || fp.UserAgentData.Platform != "Linux" {
			t.Errorf("default profile: ua %q, uad %+v", fp.UserAgent, fp.UserAgentData)
		}
		want := brandVersions(ver.Major, ver.Full, false)
		for i, b := range fp.UserAgentData.Brands {
			if i >= len(want) || b.Brand != want[i].Brand || b.Version != want[i].Version {
				t.Errorf("brand list differs from Chrome's: got %+v, algorithm %+v", fp.UserAgentData.Brands, want)
				break
			}
		}
		if fv := fp.UserAgentData.HighEntropy.FullVersionList; len(fv) == 3 && fv[0].Version != brandVersions(ver.Major, ver.Full, true)[0].Version {
			t.Errorf("full version list: %+v", fv)
		}
		if fp.Window.InnerWidth != 900 || fp.Window.InnerHeight != 600 {
			t.Errorf("default viewport: %+v", fp.Window)
		}
		if fp.Fonts["Segoe UI"] || !fp.Fonts["DejaVu Sans"] {
			t.Errorf("default fonts: %v", fp.Fonts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.remove(s.meta.ID)

	// Windows: headers and script-visible values, page and worker.
	s, err = mgr.start(ctx, startOptions{Device: "windows", Timezone: "Australia/Sydney", Locale: "en-AU"})
	if err != nil {
		t.Fatal(err)
	}
	sid := s.meta.ID
	winUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver.Major + ".0.0.0 Safari/537.36"
	checkWindows := func(ctx context.Context, tb *tab, where string) {
		fp := readFingerprint(ctx, t, tb)
		if fp.UserAgent != winUA || fp.Platform != "Win32" || fp.Webdriver {
			t.Errorf("%s: ua %q platform %q webdriver %v", where, fp.UserAgent, fp.Platform, fp.Webdriver)
		}
		if fp.UserAgentData == nil || fp.UserAgentData.Platform != "Windows" {
			t.Errorf("%s: userAgentData %+v", where, fp.UserAgentData)
		} else if he := fp.UserAgentData.HighEntropy; he.Architecture != "x86" || he.PlatformVersion != "19.0.0" || he.UaFullVersion != ver.Full {
			t.Errorf("%s: high entropy %+v (chrome %s)", where, he, ver.Full)
		}
		if fp.Screen.Width != 1920 || fp.Screen.Height != 1080 || fp.Window.InnerWidth != 900 || fp.Window.InnerHeight != 600 || fp.Window.DevicePixelRatio != 1 {
			t.Errorf("%s: screen %+v window %+v", where, fp.Screen, fp.Window)
		}
		if fp.Window.OuterWidth <= fp.Window.InnerWidth || fp.Window.OuterHeight <= fp.Window.InnerHeight {
			t.Errorf("%s: window has no frame: %+v", where, fp.Window)
		}
		if !fp.Media["(hover: hover)"] || !fp.Media["(pointer: fine)"] {
			t.Errorf("%s: pointer media %v", where, fp.Media)
		}
		if !strings.Contains(fp.WebGL.UnmaskedRenderer, "NVIDIA") || fp.WebGL.UnmaskedVendor != "Google Inc. (NVIDIA)" || fp.WebGL.GetParameterSource != "function getParameter() { [native code] }" {
			t.Errorf("%s: webgl %+v", where, fp.WebGL)
		}
		if fp.Timezone != "Australia/Sydney" || fp.Language != "en-AU" {
			t.Errorf("%s: timezone %q language %q", where, fp.Timezone, fp.Language)
		}
		// Windows families are there under the image's fonts; the image's
		// own names are not.
		for name, want := range map[string]bool{"Segoe UI": true, "Tahoma": true, "Consolas": true, "Arial": true, "Times New Roman": true,
			"DejaVu Sans": false, "Liberation Sans": false, "Noto Sans": false, "Ubuntu": false} {
			if fp.Fonts[name] != want {
				t.Errorf("%s: font %q present=%v, want %v (%v)", where, name, fp.Fonts[name], want, fp.Fonts)
			}
		}
		// The CSS generic families carry Windows metrics: `fantasy` (Blink
		// resolves it to Impact, condensed) measures clearly narrower than
		// the `system-ui` body font (Segoe UI). A font-fingerprinting script
		// that finds fantasy wider than the UI font reads the box as Firefox
		// on Linux; this relationship is what keeps it reading as Windows.
		var gm struct{ Fantasy, System int }
		if out, err := evaluate(ctx, tb, `(() => { const c=document.createElement('div');c.style.cssText='position:absolute;left:-9999px';document.body.appendChild(c);const m=f=>{const e=document.createElement('span');e.style.fontSize='72px';e.style.fontFamily=f;e.textContent='mmmmmmmmmmlliWWWW';c.appendChild(e);return e.offsetWidth;};const r={fantasy:m('fantasy'),system:m('system-ui')};c.remove();return r;})()`, 5*time.Second); err == nil {
			json.Unmarshal([]byte(out), &gm)
		} else {
			t.Errorf("%s: font metric probe: %v", where, err)
		}
		if gm.Fantasy == 0 || gm.Fantasy >= gm.System || gm.Fantasy >= 900 {
			t.Errorf("%s: generic font metrics fantasy=%d system=%d (want fantasy < system and < 900)", where, gm.Fantasy, gm.System)
		}
		var w struct {
			UserAgent     string   `json:"userAgent"`
			Platform      string   `json:"platform"`
			Timezone      string   `json:"timezone"`
			Languages     []string `json:"languages"`
			WebGL         string   `json:"webgl"`
			UserAgentData *struct{ Platform string }
		}
		if err := json.Unmarshal(fp.Worker, &w); err != nil || w.UserAgent != winUA || w.Platform != "Win32" || w.UserAgentData == nil || w.UserAgentData.Platform != "Windows" {
			t.Errorf("%s: worker %s (%v)", where, fp.Worker, err)
		} else if w.Timezone != "Australia/Sydney" || len(w.Languages) == 0 || w.Languages[0] != "en-AU" || !strings.Contains(w.WebGL, "NVIDIA") {
			t.Errorf("%s: worker %s", where, fp.Worker)
		}
	}
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, es.URL, 20*time.Second); err != nil {
			return err
		}
		checkWindows(ctx, tb, "page")
		// The navigation carried the profile's user agent and low-entropy
		// hints; once the site asked (Accept-CH), the high-entropy ones.
		h := es.last("/")
		if h.Get("User-Agent") != winUA || h.Get("Sec-CH-UA-Platform") != `"Windows"` || h.Get("Sec-CH-UA-Mobile") != "?0" {
			t.Errorf("navigation headers: %v", h)
		}
		if !strings.HasPrefix(h.Get("Accept-Language"), "en-AU") {
			t.Errorf("accept-language: %q", h.Get("Accept-Language"))
		}
		if h := es.last("/from-page"); h == nil || h.Get("Sec-CH-UA-Platform-Version") != `"19.0.0"` || h.Get("Sec-CH-UA-Arch") != `"x86"` || !strings.Contains(h.Get("Sec-CH-UA-Full-Version-List"), ver.Full) {
			t.Errorf("high-entropy hints: %v", h)
		}
		// Requests made outside any page — the service worker's script,
		// and its own fetches — carry the user agent too.
		for _, p := range []string{"/sw.js", "/from-sw"} {
			if h := es.last(p); h == nil || h.Get("User-Agent") != winUA {
				t.Errorf("%s headers: %v", p, h)
			}
		}
		// A popup: the browser dispatches its first request before the
		// per-target override can reach the new tab, so the user agent
		// comes from the command line and the client-hint platform from
		// the emulator's request stamping (both must be Windows), and by
		// the time its scripts run the whole profile is in place.
		res, err := resolveTarget(ctx, tb, targetSpec{Text: "open popup"})
		if err != nil {
			return err
		}
		if err := clickAt(ctx, tb, res.X, res.Y, "left", 1, 0); err != nil {
			return err
		}
		if h := es.last("/popup"); h == nil || h.Get("User-Agent") != winUA || h.Get("Sec-CH-UA-Platform") != `"Windows"` {
			t.Errorf("popup headers: UA=%q platform=%q", h.Get("User-Agent"), h.Get("Sec-CH-UA-Platform"))
		}
		time.Sleep(500 * time.Millisecond)
		if err := s.syncTabs(ctx); err != nil {
			return err
		}
		if len(s.tabs) != 2 {
			t.Fatalf("%d tabs after popup", len(s.tabs))
		}
		popup, err := s.resolveTab("t2")
		if err != nil {
			return err
		}
		s.focus(ctx, popup)
		settle(ctx, popup, 5*time.Second)
		checkWindows(ctx, popup, "popup")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Park and resume: the profile is part of the session, not the launch.
	s.mu.Lock()
	s.park()
	s.mu.Unlock()
	if s.meta.Device != "windows" || s.meta.Timezone != "Australia/Sydney" {
		t.Errorf("meta after park: %+v", s.meta)
	}
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, es.URL, 20*time.Second); err != nil {
			return err
		}
		checkWindows(ctx, tb, "resumed")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.remove(sid)

	// Headful: the window is real, the screen is the profile's.
	if xvnc := defaultXvnc(); xvnc != "" {
		mgr.cfg.Xvnc = xvnc
		s, err := mgr.start(ctx, startOptions{Mode: modeHeadful, Device: "windows"})
		if err != nil {
			t.Fatal(err)
		}
		err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
			if err := navigate(ctx, tb, es.URL, 20*time.Second); err != nil {
				return err
			}
			fp := readFingerprint(ctx, t, tb)
			// The window is Xvnc's, give or take a pixel of frame.
			if fp.Platform != "Win32" || fp.Screen.Width != 1920 || fp.Screen.Height != 1080 || fp.Window.InnerWidth < 898 || fp.Window.InnerWidth > 900 || fp.Webdriver {
				t.Errorf("headful: platform %q screen %+v window %+v webdriver %v", fp.Platform, fp.Screen, fp.Window, fp.Webdriver)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		mgr.remove(s.meta.ID)
	}
}

func TestLocaleLaunch(t *testing.T) {
	flags, env := localeLaunch("en-AU")
	if len(flags) != 1 || flags[0] != "--accept-lang=en-AU,en" || len(env) != 2 || env[0] != "LANG=en_AU.UTF-8" || env[1] != "LANGUAGE=en_AU:en" {
		t.Errorf("en-AU: %v %v", flags, env)
	}
	flags, env = localeLaunch("de")
	if flags[0] != "--accept-lang=de" || env[0] != "LANG=de.UTF-8" {
		t.Errorf("de: %v %v", flags, env)
	}
	if f, e := localeLaunch(""); f != nil || e != nil {
		t.Error("empty locale launches something")
	}
}

func TestWindowsFontsConf(t *testing.T) {
	// A machine with DejaVu and Liberation but no Selawik or Carlito.
	installed := map[string]bool{"DejaVu Sans": true, "DejaVu Serif": true, "DejaVu Sans Mono": true,
		"Liberation Sans": true, "Liberation Serif": true, "Liberation Mono": true, "Noto Sans CJK JP": true}
	conf := windowsFontsConf(installed)
	for _, want := range []string{
		// Segoe UI prefers what is there, in order, and nothing that is not.
		`<family>Segoe UI</family><prefer><family>DejaVu Sans</family><family>Liberation Sans</family></prefer>`,
		// Calibri's only present substitute.
		`<family>Calibri</family><prefer><family>Liberation Sans</family></prefer>`,
		// Hidden families are rewritten to nothing plus a decoy of another family.
		`<string>DejaVu Sans</string></test><edit name="family" mode="assign_replace"><string>chromemcp-absent</string><string>Noto Sans CJK JP</string></edit>`,
		`<string>Liberation Sans</string></test><edit name="family" mode="assign_replace"><string>chromemcp-absent</string><string>DejaVu Sans</string></edit>`,
		`<family>sans-serif</family><prefer><family>DejaVu Sans</family><family>Liberation Sans</family></prefer>`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf lacks %s", want)
		}
	}
	// Families with no substitute present are left out, as are hides of
	// fonts that are not there.
	for _, absent := range []string{"Selawik", "Carlito", "Yu Mincho", "Microsoft YaHei", "<string>Ubuntu</string>", "<string>Noto Sans</string>"} {
		if strings.Contains(conf, absent) {
			t.Errorf("conf mentions %s, which is not installed", absent)
		}
	}
	// Impact (Blink's `fantasy`) prefers the condensed narrow font when it
	// is installed, and falls back to Carlito — always in the image — when
	// the separate narrow package is not, so `fantasy` stays under the
	// width a font-fingerprinting script reads as Firefox.
	withNarrow := map[string]bool{"Liberation Sans Narrow": true, "Carlito": true, "Liberation Sans": true, "Selawik": true, "DejaVu Sans": true, "Noto Sans CJK JP": true}
	if c := windowsFontsConf(withNarrow); !strings.Contains(c, `<family>Impact</family><prefer><family>Liberation Sans Narrow</family>`) {
		t.Error("Impact should prefer Liberation Sans Narrow when installed")
	}
	noNarrow := map[string]bool{"Carlito": true, "Liberation Sans": true, "Selawik": true, "DejaVu Sans": true, "Noto Sans CJK JP": true}
	if c := windowsFontsConf(noNarrow); !strings.Contains(c, `<family>Impact</family><prefer><family>Carlito</family>`) {
		t.Error("Impact should fall back to Carlito when the narrow package is absent")
	}
	// With no list, everything is assumed present.
	all := windowsFontsConf(nil)
	if !strings.Contains(all, `<family>Segoe UI</family><prefer><family>Selawik</family>`) || !strings.Contains(all, "<family>Meiryo</family>") {
		t.Error("nil installed set should assume every font")
	}
	// The file round-trips through the profile.
	dir := t.TempDir()
	d, _ := lookupDevice("windows")
	p, err := d.fontsConfFile(dir, installed)
	if err != nil || p == "" {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != conf {
		t.Error("written config differs")
	}
	if p2, err := d.fontsConfFile(dir, installed); err != nil || p2 != p {
		t.Errorf("second write: %s %v", p2, err)
	}
	if p, _ := deviceProfiles["default"].fontsConfFile(dir, installed); p != "" {
		t.Error("default profile has a fonts file")
	}
}

func TestClientHintHeaders(t *testing.T) {
	ver := chromeVersion{Full: "152.0.7977.82", Major: "152"}
	if h := deviceProfiles["default"].clientHintHeaders(ver); h != nil {
		t.Errorf("default has client-hint headers: %v", h)
	}
	h := deviceProfiles["windows"].clientHintHeaders(ver)
	if h["sec-ch-ua-platform"] != `"Windows"` || h["sec-ch-ua-mobile"] != "?0" {
		t.Errorf("headers: %v", h)
	}
	if h["sec-ch-ua"] != `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"` {
		t.Errorf("sec-ch-ua: %q", h["sec-ch-ua"])
	}
}

func TestFontPrefs(t *testing.T) {
	if deviceProfiles["default"].fontPrefs() != nil {
		t.Error("default has font prefs")
	}
	p := deviceProfiles["windows"].fontPrefs()
	fonts := p["webkit"].(map[string]any)["webprefs"].(map[string]any)["fonts"].(map[string]any)
	if fonts["fantasy"].(map[string]any)["Zyyy"] != "Impact" {
		t.Errorf("fantasy pref: %v", fonts["fantasy"])
	}
	if fonts["sansserif"].(map[string]any)["Zyyy"] != "Arial" {
		t.Errorf("sansserif pref: %v", fonts["sansserif"])
	}
}

func TestMergeProfilePrefs(t *testing.T) {
	dir := t.TempDir()
	// nil prefs: nothing written.
	if err := mergeProfilePrefs(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Preferences")); err == nil {
		t.Error("nil prefs wrote a file")
	}
	// Existing Preferences with unrelated settings survive the merge.
	os.MkdirAll(filepath.Join(dir, "Default"), 0o700)
	os.WriteFile(filepath.Join(dir, "Default", "Preferences"),
		[]byte(`{"profile":{"name":"me"},"webkit":{"webprefs":{"fonts":{"fantasy":{"Zyyy":"OldFont"}},"other":1}}}`), 0o600)
	if err := mergeProfilePrefs(dir, deviceProfiles["windows"].fontPrefs()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	var got map[string]any
	json.Unmarshal(b, &got)
	if got["profile"].(map[string]any)["name"] != "me" {
		t.Error("merge lost the unrelated profile setting")
	}
	wp := got["webkit"].(map[string]any)["webprefs"].(map[string]any)
	if wp["other"].(float64) != 1 {
		t.Error("merge lost a sibling webpref")
	}
	if wp["fonts"].(map[string]any)["fantasy"].(map[string]any)["Zyyy"] != "Impact" {
		t.Errorf("merge did not override the font: %v", wp["fonts"])
	}
	if wp["fonts"].(map[string]any)["fixed"].(map[string]any)["Zyyy"] != "Consolas" {
		t.Error("merge did not add the new font families")
	}
}
