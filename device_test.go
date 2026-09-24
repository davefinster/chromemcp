package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	// No profile asked for is the Windows one; the native profile is by name.
	if defaultDevice != "windows" {
		t.Errorf("default device is %q", defaultDevice)
	}
	if d, err := lookupDevice(""); err != nil || d.Name != defaultDevice || !d.emulated() {
		t.Fatalf("default: %+v %v", d, err)
	}
	d, err := lookupDevice(nativeDevice)
	if err != nil || d.Name != "linux" || d.emulated() {
		t.Fatalf("native: %+v %v", d, err)
	}
	// Metadata from before the profile was recorded means the native one,
	// not the current default; recorded names are kept.
	if n := (&sessionMeta{}).deviceName(); n != nativeDevice {
		t.Errorf("legacy metadata resolves to %q", n)
	}
	if n := (&sessionMeta{Device: "windows"}).deviceName(); n != "windows" {
		t.Errorf("recorded device resolves to %q", n)
	}
	ver := chromeVersion{Full: "152.0.7977.82", Major: "152"}
	if d.userAgentOverride(ver) != nil || d.initScript(ver) != "" || d.chromeFlags(ver) != nil {
		t.Error("native profile emulates something")
	}
	if m := d.metrics(1280, 800, false); m != nil {
		t.Errorf("native headful metrics: %+v", m)
	}
	if m := d.metrics(1280, 800, true); m == nil || m.Width != 1280 || m.Height != 800 || m.ScreenWidth != 0 || m.DeviceScaleFactor != 1 {
		t.Errorf("native headless metrics: %+v", m)
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
	for _, want := range []string{`"Win32"`, `"Google Inc. (NVIDIA)"`, `"152.0.7977.82"`, "webdriver", "getParameter",
		`"deviceMemory":8`, `"hardwareConcurrency":8`, `"taskbar":48`, "availHeight"} {
		if !strings.Contains(js, want) {
			t.Errorf("init script lacks %s", want)
		}
	}
	// A real browser never reports deviceMemory above the spec's cap of 8.
	if w.DeviceMemory > 8 || w.DeviceMemory == 0 {
		t.Errorf("deviceMemory %d: want 1..8", w.DeviceMemory)
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
	Screen              struct{ Width, Height, AvailWidth, AvailHeight int }
	HardwareConcurrency int     `json:"hardwareConcurrency"`
	DeviceMemory        float64 `json:"deviceMemory"`
	Window              struct {
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

	// A session asked for with no profile is recorded as the Windows one.
	s, err := mgr.start(ctx, startOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.meta.Device != "windows" || !strings.Contains(s.meta.deviceSuffix(), "device windows") {
		t.Errorf("unasked-for device recorded as %q (%s)", s.meta.Device, s.meta.deviceSuffix())
	}
	mgr.remove(s.meta.ID)

	// The native profile is Chrome as it is — and its own brand list is
	// what the algorithm must reproduce for this version.
	s, err = mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	if s.meta.Device != "linux" {
		t.Errorf("native device recorded as %q", s.meta.Device)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, es.URL, 20*time.Second); err != nil {
			return err
		}
		fp := readFingerprint(ctx, t, tb)
		if !strings.Contains(fp.UserAgent, "Linux") || fp.UserAgentData == nil || fp.UserAgentData.Platform != "Linux" {
			t.Errorf("native profile: ua %q, uad %+v", fp.UserAgent, fp.UserAgentData)
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
		// Consumer-PC numbers, not the container's server-class host values:
		// deviceMemory never above the spec cap of 8, a plausible core count,
		// and a taskbar reserved from the available height.
		if fp.DeviceMemory > 8 || fp.DeviceMemory == 0 || fp.HardwareConcurrency == 0 || fp.HardwareConcurrency > 16 {
			t.Errorf("%s: deviceMemory=%v hardwareConcurrency=%d (want <=8 and a consumer core count)", where, fp.DeviceMemory, fp.HardwareConcurrency)
		}
		if fp.Screen.AvailHeight >= fp.Screen.Height || fp.Screen.AvailWidth != fp.Screen.Width {
			t.Errorf("%s: no taskbar: avail %dx%d of %dx%d", where, fp.Screen.AvailWidth, fp.Screen.AvailHeight, fp.Screen.Width, fp.Screen.Height)
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
		// own names are not — with the exception noted in fonts.go, which
		// this deliberately does not assert: Skia answers a request for a
		// metric-compatible family with its partner whatever fontconfig
		// says, so Liberation Sans is visible for as long as Arial is, and
		// the same goes for Liberation Serif/Times New Roman, Liberation
		// Mono/Courier New, Carlito/Calibri and Caladea/Cambria. The
		// unpaired names are the ones a profile can actually take away.
		for name, want := range map[string]bool{"Segoe UI": true, "Tahoma": true, "Consolas": true, "Arial": true, "Times New Roman": true,
			"DejaVu Sans": false, "Noto Sans": false, "Ubuntu": false, "Selawik": false} {
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

func testFontSet(families ...string) *fontSet {
	s := &fontSet{files: map[string][]string{}}
	for _, f := range families {
		s.files[f] = []string{"/usr/share/fonts/" + strings.ReplaceAll(f, " ", "") + ".ttf"}
	}
	return s
}

func TestWindowsFontsPlan(t *testing.T) {
	// A machine with DejaVu and Liberation but no Selawik or Carlito.
	fonts := testFontSet("DejaVu Sans", "DejaVu Serif", "DejaVu Sans Mono",
		"Liberation Sans", "Liberation Serif", "Liberation Mono", "Noto Sans CJK JP")
	plan := windowsFontsPlan(fonts, "/s/fonts.d", "/s/fonts.cache")
	for _, want := range []string{
		"<dir>/s/fonts.d</dir>",
		"<cachedir>/s/fonts.cache</cachedir>",
		// The file behind DejaVu Sans is renamed to the first family it
		// answers for and appended the rest, so those names are real and
		// its own is gone.
		`<test name="family"><string>DejaVu Sans</string></test><edit name="family" mode="assign_replace"><string>Segoe UI</string></edit>`,
		`<edit name="family" mode="append"><string>Tahoma</string></edit>`,
		`<edit name="family" mode="append"><string>Verdana</string></edit>`,
		// Liberation Sans is the first present substitute for Arial, and
		// for Calibri, whose own stand-ins are not installed here.
		`<test name="family"><string>Liberation Sans</string></test><edit name="family" mode="assign_replace"><string>Arial</string></edit>`,
		`<edit name="family" mode="append"><string>Calibri</string></edit>`,
		`<family>sans-serif</family><prefer><family>Segoe UI</family><family>Arial</family></prefer>`,
		`<family>serif</family><prefer><family>Times New Roman</family></prefer>`,
	} {
		if !strings.Contains(plan.Conf, want) {
			t.Errorf("plan lacks %s", want)
		}
	}
	// The system configuration is not included: this directory is all
	// Chrome sees, which is what makes the machine's own names absent.
	if strings.Contains(plan.Conf, "<include") {
		t.Error("the plan includes the system configuration")
	}
	// A device family with no substitute installed is simply not presented,
	// and a substitute nothing needs is not linked, so its name goes too.
	for _, absent := range []string{"Selawik", "Carlito", "<string>Yu Mincho</string>", "Noto Color Emoji"} {
		if strings.Contains(plan.Conf, absent) {
			t.Errorf("plan mentions %s, which is not installed or not needed", absent)
		}
	}
	// Only the files behind substitutes that answer for something.
	if len(plan.Files) != 7 {
		t.Errorf("files = %v, want the seven installed substitutes, each of which answers for something", plan.Files)
	}
	for _, f := range plan.Files {
		if strings.Contains(f, "NotoSansCJKJP") && !strings.Contains(plan.Conf, "MS Gothic") {
			t.Error("the CJK font was linked without being renamed")
		}
	}

	// Impact (Blink's `fantasy`) takes the condensed narrow font when it is
	// installed, and falls back to Carlito — always in the image — when the
	// separate narrow package is not, so `fantasy` stays under the width a
	// font-fingerprinting script reads as Firefox.
	withNarrow := testFontSet("Liberation Sans Narrow", "Carlito", "Liberation Sans", "Selawik", "DejaVu Sans", "Noto Sans CJK JP")
	if c := windowsFontsPlan(withNarrow, "d", "c").Conf; !strings.Contains(c,
		`<test name="family"><string>Liberation Sans Narrow</string></test><edit name="family" mode="assign_replace"><string>Arial Narrow</string></edit><edit name="family" mode="append"><string>Impact</string></edit>`) {
		t.Error("Impact should be answered by Liberation Sans Narrow when it is installed")
	}
	noNarrow := testFontSet("Carlito", "Liberation Sans", "Selawik", "DejaVu Sans", "Noto Sans CJK JP")
	if c := windowsFontsPlan(noNarrow, "d", "c").Conf; !strings.Contains(c, `<string>Carlito</string></test><edit name="family" mode="assign_replace"><string>Arial Narrow</string></edit><edit name="family" mode="append"><string>Impact</string></edit>`) {
		t.Error("Impact should fall back to Carlito when the narrow package is absent")
	}
	// Nothing to present at all is not a configuration.
	if p := windowsFontsPlan(testFontSet(), "d", "c"); p.Conf != "" || p.Files != nil {
		t.Errorf("a machine with no usable font produced %+v", p)
	}

	// The world round-trips onto disk: the directory holds a link per file,
	// the second call changes nothing, and a profile of the machine's own
	// fonts has no configuration at all.
	dir := t.TempDir()
	d, _ := lookupDevice("windows")
	real := installedFonts()
	if real == nil {
		t.Skip("no fc-list")
	}
	p, err := d.fontsConfFile(dir, real)
	if err != nil || p == "" {
		t.Fatal(err)
	}
	links, err := os.ReadDir(filepath.Join(dir, "fonts-windows.d"))
	if err != nil || len(links) == 0 {
		t.Fatalf("font directory: %d links, %v", len(links), err)
	}
	for _, l := range links {
		target, err := os.Readlink(filepath.Join(dir, "fonts-windows.d", l.Name()))
		if err != nil {
			t.Errorf("%s is not a link: %v", l.Name(), err)
		} else if _, err := os.Stat(target); err != nil {
			t.Errorf("%s points nowhere: %v", l.Name(), err)
		}
	}
	if p2, err := d.fontsConfFile(dir, real); err != nil || p2 != p {
		t.Errorf("second build: %s %v", p2, err)
	}
	if p, _ := deviceProfiles[nativeDevice].fontsConfFile(dir, real); p != "" {
		t.Error("native profile has a fonts file")
	}
	if p, _ := d.fontsConfFile(dir, nil); p != "" {
		t.Error("a machine whose fonts cannot be listed got a configuration")
	}
}
func TestClientHintHeaders(t *testing.T) {
	ver := chromeVersion{Full: "152.0.7977.82", Major: "152"}
	if h := deviceProfiles[nativeDevice].clientHintHeaders(ver); h != nil {
		t.Errorf("native profile has client-hint headers: %v", h)
	}
	h := deviceProfiles["windows"].clientHintHeaders(ver)
	if h["sec-ch-ua-platform"] != `"Windows"` || h["sec-ch-ua-mobile"] != "?0" {
		t.Errorf("headers: %v", h)
	}
	if h["sec-ch-ua"] != `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"` {
		t.Errorf("sec-ch-ua: %q", h["sec-ch-ua"])
	}
	// The device-memory hint is capped at 8 (a real browser never sends more),
	// in both its modern and legacy header names.
	if h["sec-ch-device-memory"] != "8" || h["device-memory"] != "8" {
		t.Errorf("device-memory hints: %q / %q", h["sec-ch-device-memory"], h["device-memory"])
	}
}

func TestFontPrefs(t *testing.T) {
	if deviceProfiles[nativeDevice].fontPrefs() != nil {
		t.Error("native profile has font prefs")
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

// TestDeviceMemoryHeader checks that the emulator's request interception caps
// the Sec-CH-Device-Memory / Device-Memory headers (the container leaks its
// host's 16/32 GB, which a real browser can never report) to match the JS
// navigator.deviceMemory, so a request and script agree on a plausible value.
func TestDeviceMemoryHeader(t *testing.T) {
	mgr := testChrome(t)
	var mu sync.Mutex
	got := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Accept-CH", "Sec-CH-Device-Memory, Device-Memory")
		w.Write([]byte(`<title>x</title>hi`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Device: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil { // learns Accept-CH
			return err
		}
		if err := navigate(ctx, tb, srv.URL+"/2", 20*time.Second); err != nil { // now sends the hint
			return err
		}
		var jsMem float64
		if out, err := evaluate(ctx, tb, `navigator.deviceMemory`, 5*time.Second); err == nil {
			fmt.Sscanf(out, "%g", &jsMem)
		}
		mu.Lock()
		h := got["/2"]
		mu.Unlock()
		if h.Get("Sec-CH-Device-Memory") != "8" || h.Get("Device-Memory") != "8" {
			t.Errorf("device-memory header: sec-ch=%q legacy=%q, want 8", h.Get("Sec-CH-Device-Memory"), h.Get("Device-Memory"))
		}
		if jsMem != 8 {
			t.Errorf("navigator.deviceMemory=%v, want 8 (must match the header)", jsMem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
