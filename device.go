package main

// Device profiles: what a session's Chrome tells websites it is. The
// default is the server's own Chrome as it comes — Linux, the container's
// screen, SwiftShader — and every other profile is an emulation layered on
// it at launch (emulate.go): the user agent string and its client-hint
// metadata, navigator.platform, the screen, the GPU strings, and the few
// things that give a driven browser away. Chrome's version stays its own;
// a profile only changes what the browser says about the machine, since a
// site can compare the two.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/emulation"
)

// fingerprintScript reports what a page's scripts see of the device: the
// values a profile sets and their neighbours. browser_fingerprint runs it;
// pasted into a real browser's DevTools console it reports the same, for
// comparison.
//
//go:embed testdata/fingerprint.js
var fingerprintScript string

type deviceProfile struct {
	Name        string
	Description string
	// UserAgent is the user agent string with the Chrome major version
	// substituted for %s. Chrome's reduced UA carries only the major.
	UserAgent string
	// Platform is navigator.platform.
	Platform string
	// Client hints: Sec-CH-UA-Platform and the high-entropy values.
	CHPlatform        string
	CHPlatformVersion string
	CHArchitecture    string
	CHBitness         string
	CHModel           string
	CHMobile          bool
	CHWow64           bool
	CHFormFactors     []string
	// Screen is what window.screen reports; 0 leaves it to Chrome.
	ScreenWidth, ScreenHeight int
	// Scale is window.devicePixelRatio; 0 means 1.
	Scale float64
	// DeviceMemory is navigator.deviceMemory in GB (a real browser only ever
	// reports 0.25/0.5/1/2/4/8); 0 leaves Chrome's. HardwareConcurrency is
	// navigator.hardwareConcurrency; 0 leaves Chrome's. Both default, in the
	// container, to the host's server-class numbers, which mark a VM.
	DeviceMemory        int
	HardwareConcurrency int
	// TaskbarHeight is the pixels a desktop reserves from screen.availHeight
	// (a real Windows taskbar); 0 leaves availHeight == height.
	TaskbarHeight int
	// Pointer is what the pointer/hover media queries answer: "fine" (a
	// mouse: pointer fine, hover hover) or "coarse" (a touchscreen). "" is
	// Chrome's own answer, which without an input device is none.
	Pointer string
	// WebGL unmasked vendor and renderer (WEBGL_debug_renderer_info).
	WebGLVendor, WebGLRenderer string
	// FontsPlan, when set, is the font world Chrome runs in: the machine's
	// fonts, linked into a directory of the profile's own and renamed to
	// the device's family names (fonts.go).
	FontsPlan func(fonts *fontSet, fontDir, cacheDir string) fontPlan
	// FontFamilies are the default fonts Blink resolves the CSS generic
	// families to (standard, serif, sansserif, fixed, cursive, fantasy),
	// as the Windows names Chrome uses there — so their metrics, which
	// fingerprinting scripts read off the generics, look like Windows and
	// not the Linux fallbacks. Backed by real files through FontsPlan.
	FontFamilies map[string]string
}

// The profile a session gets when none is asked for, and the one that is
// this Chrome unemulated. Windows is the default because it is what most
// of the web expects to see and what its bot heuristics score as ordinary;
// the native profile exists for comparing against it, and for what
// sessions started before profiles were recorded in their metadata were.
const (
	defaultDevice = "windows"
	nativeDevice  = "linux"
)

// deviceProfiles is the registry, by name.
var deviceProfiles = map[string]*deviceProfile{
	nativeDevice: {
		Name:        nativeDevice,
		Description: "this server's own Chrome, as it is: Linux, no emulation",
	},
	"windows": {
		Name:        "windows",
		Description: "a Windows 11 PC running Chrome: Windows user agent and client hints, navigator.platform Win32, a 1920x1080 display at 100%, an NVIDIA GPU",
		// Chrome's frozen desktop UA: "Windows NT 10.0; Win64; x64" on
		// Windows 11 too, minor version zeroed.
		UserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36",
		Platform:   "Win32",
		CHPlatform: "Windows",
		// Sec-CH-UA-Platform-Version is the Windows UniversalApiContract
		// version: 13/14/15 for Windows 11 21H2/22H2/23H2, 19 for 24H2 and
		// the 25H2 enablement package on the same branch.
		CHPlatformVersion: "19.0.0",
		CHArchitecture:    "x86",
		CHBitness:         "64",
		CHFormFactors:     []string{"Desktop"},
		ScreenWidth:       1920,
		ScreenHeight:      1080,
		Scale:             1,
		// A consumer Windows PC: 8 GB (the spec's cap, so it can't betray a
		// bigger host), 8 logical cores, and a taskbar reserving 48px.
		DeviceMemory:        8,
		HardwareConcurrency: 8,
		TaskbarHeight:       48,
		Pointer:             "fine",
		WebGLVendor:         "Google Inc. (NVIDIA)",
		WebGLRenderer:       "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 (0x00002504) Direct3D11 vs_5_0 ps_5_0, D3D11)",
		FontsPlan:           windowsFontsPlan,
		// The generic-family defaults Chrome ships on Windows. Blink
		// resolves the CSS generics through these names, which FontsConf
		// maps to stand-ins, so a script that measures `fantasy` (Impact,
		// condensed) against `system-ui` (Segoe UI) reads Windows-shaped
		// metrics rather than the Linux fallbacks that look like Firefox.
		FontFamilies: map[string]string{
			"standard":  "Times New Roman",
			"serif":     "Times New Roman",
			"sansserif": "Arial",
			"fixed":     "Consolas",
			"cursive":   "Comic Sans MS",
			"fantasy":   "Impact",
			"math":      "Cambria Math",
		},
	},
}

func deviceNames() []string {
	names := make([]string, 0, len(deviceProfiles))
	for n := range deviceProfiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// lookupDevice resolves a profile name as session_start takes it: "" is the
// default profile.
func lookupDevice(name string) (*deviceProfile, error) {
	if name == "" {
		name = defaultDevice
	}
	d, ok := deviceProfiles[name]
	if !ok {
		return nil, fmt.Errorf("device %q: want one of %s", name, strings.Join(deviceNames(), ", "))
	}
	return d, nil
}

// emulated reports whether the profile changes anything.
func (d *deviceProfile) emulated() bool { return d.UserAgent != "" }

// chromeVersion is a Chrome build number, "152.0.7977.82".
type chromeVersion struct {
	Full  string
	Major string
}

var versionRe = regexp.MustCompile(`\b(\d+)\.\d+\.\d+\.\d+\b`)

// probeChromeVersion asks the binary for its version. It is needed before
// launch — the user agent flag carries the major — and cached by the
// manager.
func probeChromeVersion(exe string, noSandbox bool) (chromeVersion, error) {
	args := []string{"--version"}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	out, err := exec.Command(exe, args...).Output()
	if err != nil {
		return chromeVersion{}, fmt.Errorf("%s --version: %w", exe, err)
	}
	return parseChromeVersion(string(out))
}

func parseChromeVersion(s string) (chromeVersion, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return chromeVersion{}, fmt.Errorf("no version in %q", strings.TrimSpace(s))
	}
	return chromeVersion{Full: m[0], Major: m[1]}, nil
}

// brandVersions is Chrome's Sec-CH-UA brand list for a major version: the
// GREASE brand, Chromium and Google Chrome, in the order Chrome picks for
// that version. It is Chrome's own algorithm (GenerateBrandVersionList in
// components/embedder_support/user_agent_utils.cc), seeded by the major,
// so the list is exactly what this Chrome sends on its own — the
// integration test checks that against the live browser. full gives the
// full versions (Sec-CH-UA-Full-Version-List) instead of majors.
func brandVersions(major, fullVersion string, full bool) []*emulation.UserAgentBrandVersion {
	var seed int
	fmt.Sscanf(major, "%d", &seed)
	greaseChars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVersions := []string{"8", "99", "24"}
	greaseBrand := "Not" + greaseChars[seed%len(greaseChars)] + "A" + greaseChars[(seed+1)%len(greaseChars)] + "Brand"
	greaseVersion := greaseVersions[seed%len(greaseVersions)]
	version := major
	if full {
		version = fullVersion
		greaseVersion += ".0.0.0"
	}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	order := orders[seed%len(orders)]
	list := make([]*emulation.UserAgentBrandVersion, 3)
	list[order[0]] = &emulation.UserAgentBrandVersion{Brand: greaseBrand, Version: greaseVersion}
	list[order[1]] = &emulation.UserAgentBrandVersion{Brand: "Chromium", Version: version}
	list[order[2]] = &emulation.UserAgentBrandVersion{Brand: "Google Chrome", Version: version}
	return list
}

// userAgentOverride is the CDP override for the profile on this Chrome.
func (d *deviceProfile) userAgentOverride(ver chromeVersion) *emulation.SetUserAgentOverrideParams {
	if !d.emulated() {
		return nil
	}
	return &emulation.SetUserAgentOverrideParams{
		UserAgent: fmt.Sprintf(d.UserAgent, ver.Major),
		Platform:  d.Platform,
		UserAgentMetadata: &emulation.UserAgentMetadata{
			Brands:          brandVersions(ver.Major, ver.Full, false),
			FullVersionList: brandVersions(ver.Major, ver.Full, true),
			Platform:        d.CHPlatform,
			PlatformVersion: d.CHPlatformVersion,
			Architecture:    d.CHArchitecture,
			Model:           d.CHModel,
			Mobile:          d.CHMobile,
			Bitness:         d.CHBitness,
			Wow64:           d.CHWow64,
			FormFactors:     d.CHFormFactors,
		},
	}
}

// clientHintHeaders are the client-hint request headers the profile stands
// for, lowercase name to value. The emulator stamps them onto every request
// (emulate.go): a tab's first navigation — a popup, or one the owner opens in
// the live view — is requested by the browser before the per-target user-agent
// override can reach it, and the command-line flag sets only the user-agent
// string; and Sec-CH-Device-Memory is never covered by the UA override at all,
// so the container's host memory (16/32) leaks into it — an impossible value
// (a real browser caps the hint at 8) that anti-bots read as a VM. Only headers
// already on a request are rewritten, never added, so a request carries exactly
// the hints Chrome chose to send, with the machine ones corrected.
func (d *deviceProfile) clientHintHeaders(ver chromeVersion) map[string]string {
	if !d.emulated() {
		return nil
	}
	var brands strings.Builder
	for i, b := range brandVersions(ver.Major, ver.Full, false) {
		if i > 0 {
			brands.WriteString(", ")
		}
		fmt.Fprintf(&brands, "%q;v=%q", b.Brand, b.Version)
	}
	mobile := "?0"
	if d.CHMobile {
		mobile = "?1"
	}
	h := map[string]string{
		"sec-ch-ua-platform": fmt.Sprintf("%q", d.CHPlatform),
		"sec-ch-ua-mobile":   mobile,
		"sec-ch-ua":          brands.String(),
	}
	if d.DeviceMemory > 0 {
		// The header form is a bare number ("8"); its legacy name is
		// Device-Memory, still sent alongside the Sec-CH- one.
		mem := fmt.Sprintf("%d", d.DeviceMemory)
		h["sec-ch-device-memory"] = mem
		h["device-memory"] = mem
	}
	return h
}

// fontPrefs is the Preferences fragment that sets Blink's generic-family
// fonts (device.go's FontFamilies) for a profile, merged into the profile's
// Default/Preferences before launch (session.go). nil for a profile that
// sets none.
func (d *deviceProfile) fontPrefs() map[string]any {
	if len(d.FontFamilies) == 0 {
		return nil
	}
	fonts := map[string]any{}
	for generic, family := range d.FontFamilies {
		// Zyyy is the "common" script; it is what a page with no :lang gets.
		fonts[generic] = map[string]any{"Zyyy": family}
	}
	return map[string]any{"webkit": map[string]any{"webprefs": map[string]any{"fonts": fonts}}}
}

// chromeFlags are the command-line flags the profile adds. The user agent
// goes on the command line as well as into every target's override: the
// override reaches a target only once it exists, and a popup's first
// navigation and a service worker's script fetch are requested before
// that. The flag makes the user-agent string right on those; the client
// hints are corrected by the emulator's request stamping (clientHintHeaders).
func (d *deviceProfile) chromeFlags(ver chromeVersion) []string {
	if !d.emulated() {
		return nil
	}
	flags := []string{"--user-agent=" + fmt.Sprintf(d.UserAgent, ver.Major)}
	// Blink's pointer and hover types (Settings.json5: 1 none, 2 coarse,
	// 4 fine; hover 1 none, 2 hover). Without a real input device Chrome
	// answers "none" to both, headless and on an Xvnc alike, and a site
	// then shows its touch layout.
	switch d.Pointer {
	case "fine":
		flags = append(flags, "--blink-settings=primaryPointerType=4,availablePointerTypes=4,primaryHoverType=2,availableHoverTypes=2")
	case "coarse":
		flags = append(flags, "--blink-settings=primaryPointerType=2,availablePointerTypes=2,primaryHoverType=1,availableHoverTypes=1")
	}
	return flags
}

// metrics is the device-metrics override: the viewport (headless only —
// a headful window is its own viewport) and the profile's screen.
func (d *deviceProfile) metrics(width, height int, headless bool) *emulation.SetDeviceMetricsOverrideParams {
	if !headless && !d.emulated() {
		return nil
	}
	p := &emulation.SetDeviceMetricsOverrideParams{DeviceScaleFactor: 1}
	if headless {
		p.Width, p.Height = int64(width), int64(height)
	}
	if d.Scale > 0 {
		p.DeviceScaleFactor = d.Scale
	}
	if d.ScreenWidth > 0 && d.ScreenHeight > 0 {
		sw, sh := d.ScreenWidth, d.ScreenHeight
		// A page larger than the profile's display is a display that
		// size: the window cannot be bigger than the screen it is on.
		if width > sw || height > sh {
			sw, sh = max(width, sw), max(height, sh)
		}
		p.ScreenWidth, p.ScreenHeight = int64(sw), int64(sh)
	}
	return p
}

// initScript is the JavaScript run in every document and worker before the
// page's own: the few things a profile cannot set through the protocol.
// Patched functions report native code to Function.prototype.toString.
func (d *deviceProfile) initScript(ver chromeVersion) string {
	if !d.emulated() {
		return ""
	}
	b, _ := json.Marshal(map[string]any{
		"platform":            d.Platform,
		"vendor":              d.WebGLVendor,
		"renderer":            d.WebGLRenderer,
		"fullVersion":         ver.Full,
		"deviceMemory":        d.DeviceMemory,        // 0: leave Chrome's
		"hardwareConcurrency": d.HardwareConcurrency, // 0: leave Chrome's
		"taskbar":             d.TaskbarHeight,       // 0: leave availHeight == height
	})
	return fmt.Sprintf(deviceInitScript, string(b))
}

const deviceInitScript = `(() => {
  'use strict';
  const P = %s;
  const natives = new WeakMap();
  const nativeToString = Function.prototype.toString;
  const mask = (fn, name) => {
    Object.defineProperty(fn, 'name', {value: name, configurable: true});
    natives.set(fn, name);
    return fn;
  };
  Object.defineProperty(Function.prototype, 'toString', {
    value: mask(function toString() {
      const name = natives.get(this);
      if (name !== undefined) return 'function ' + name + '() { [native code] }';
      return nativeToString.call(this);
    }, 'toString'),
    writable: true, enumerable: false, configurable: true,
  });
  const getter = (proto, prop, get) => {
    const d = proto && Object.getOwnPropertyDescriptor(proto, prop);
    if (!d || !d.get) return;
    Object.defineProperty(proto, prop, {get: mask(get, 'get ' + prop), set: d.set, enumerable: d.enumerable, configurable: d.configurable});
  };
  // navigator.platform. The protocol's override keeps it in per-page
  // settings that every attached session restores after a process swap,
  // the ones with nothing set included, and workers never get it at all.
  if (navigator.platform !== P.platform) {
    getter(self.Navigator ? Navigator.prototype : WorkerNavigator.prototype, 'platform', function () { return P.platform; });
  }
  // navigator.webdriver is true in a Chrome with remote debugging on; a
  // person's Chrome says false.
  if (typeof Navigator !== 'undefined') {
    getter(Navigator.prototype, 'webdriver', function () { return false; });
  }
  // navigator.deviceMemory and hardwareConcurrency. The container reports its
  // host's numbers — deviceMemory 16/32 (a real browser caps it at 8, so
  // anything higher is impossible and marks a VM) and a server-class core
  // count — so pin them to a consumer PC's, in the page and in workers.
  const NavProto = self.Navigator ? Navigator.prototype : (typeof WorkerNavigator !== 'undefined' ? WorkerNavigator.prototype : null);
  if (NavProto && P.deviceMemory) {
    getter(NavProto, 'deviceMemory', function () { return P.deviceMemory; });
  }
  if (NavProto && P.hardwareConcurrency) {
    getter(NavProto, 'hardwareConcurrency', function () { return P.hardwareConcurrency; });
  }
  // screen.availHeight: a Windows desktop reserves the taskbar, so the
  // available height is a little less than the screen height. Emulated
  // device metrics leave them equal (no taskbar), which reads as "no real
  // desktop"; reserve the taskbar and keep availWidth == width, avail top/left 0.
  if (typeof Screen !== 'undefined' && P.taskbar) {
    getter(Screen.prototype, 'availHeight', function () { return Math.max(0, this.height - P.taskbar); });
    getter(Screen.prototype, 'availWidth', function () { return this.width; });
    getter(Screen.prototype, 'availTop', function () { return 0; });
    getter(Screen.prototype, 'availLeft', function () { return 0; });
  }
  // WEBGL_debug_renderer_info: the GPU strings, only where the real
  // getParameter would have answered (the extension must be enabled).
  for (const C of [self.WebGLRenderingContext, self.WebGL2RenderingContext]) {
    if (!C) continue;
    const orig = C.prototype.getParameter;
    C.prototype.getParameter = mask(function (pname) {
      const v = orig.apply(this, arguments);
      if (typeof v === 'string') {
        if (pname === 0x9245) return P.vendor;
        if (pname === 0x9246) return P.renderer;
      }
      return v;
    }, 'getParameter');
  }
  // uaFullVersion comes back blank when the user agent is set on the
  // command line; it is the full version of the Chrome brand.
  if (typeof NavigatorUAData !== 'undefined' && P.fullVersion) {
    const orig = NavigatorUAData.prototype.getHighEntropyValues;
    NavigatorUAData.prototype.getHighEntropyValues = mask(function (hints) {
      return orig.apply(this, arguments).then(v => {
        if (v && 'uaFullVersion' in v && !v.uaFullVersion) v.uaFullVersion = P.fullVersion;
        return v;
      });
    }, 'getHighEntropyValues');
  }
  // window.outerWidth/outerHeight: a real desktop window's outer size is
  // its inner size plus the browser frame. A tab reports 0 for a moment
  // right after it opens (and a headless window always does), which reads
  // as "no window" — a bot signal. Report the inner size plus a frame
  // whenever the native value is 0, and pass it through otherwise.
  if (typeof window !== 'undefined') {
    const frame = {outerWidth: [0, 'innerWidth'], outerHeight: [74, 'innerHeight']};
    for (const prop in frame) {
      const [pad, inner] = frame[prop];
      const d = Object.getOwnPropertyDescriptor(window, prop);
      if (!d || !d.get) continue;
      const nativeGet = d.get;
      try {
        Object.defineProperty(window, prop, {configurable: true, enumerable: d.enumerable,
          get: mask(function () { const v = nativeGet.call(this); return v || (window[inner] + pad); }, 'get ' + prop)});
      } catch (e) {}
    }
  }
})();`

// localeLaunch is how a session's locale reaches Chrome on Linux, where
// --lang is ignored: --accept-lang sets Accept-Language and
// navigator.languages ("en-AU,en" — Chrome adds the q-values), and the
// LANG/LANGUAGE environment sets the UI language and with it Intl's
// default locale, as Chrome resolves it (en-AU has no UI locale of its own
// and becomes en-GB, the way it does on a real machine). A timezone is
// the TZ environment, read by every process.
func localeLaunch(locale string) (flags, env []string) {
	if locale == "" {
		return nil, nil
	}
	lang, region, hasRegion := strings.Cut(locale, "-")
	lang = strings.ToLower(lang)
	accept := lang
	posix := lang
	if hasRegion {
		region = strings.ToUpper(region)
		accept = lang + "-" + region + "," + lang
		posix = lang + "_" + region
	}
	return []string{"--accept-lang=" + accept},
		[]string{"LANG=" + posix + ".UTF-8", "LANGUAGE=" + posix + ":" + lang}
}
