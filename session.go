package main

// Sessions: one Chrome instance each, on its own profile directory, in one
// of three states — running (Chrome alive), parked (Chrome closed, profile
// kept, resumable) or gone. The sessions directory is ephemeral on purpose:
// nothing here outlives the deployment except identities (identity.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // so a timezone can be checked where the image has no zoneinfo

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	modeHeadless = "headless"
	modeHeadful  = "headful"
)

// headfulChromeHeight is the room Chrome's own UI (tab strip, toolbar)
// takes above the page in a headful window, so the display is made that
// much taller than the requested viewport. windowFrameWidth is the width
// of a desktop window's side borders; an emulated device's headless window
// is made that much bigger than its viewport, so window.outerWidth and
// outerHeight exceed the inner ones the way a real window's do.
const (
	headfulChromeHeight = 88
	windowFrameWidth    = 16
)

type managerConfig struct {
	SessionsDir   string
	IdentitiesDir string
	Chrome        string
	ChromeFlags   []string
	NoSandbox     bool
	Xvnc          string
	DevToolsMCP   string
	ViewportW     int
	ViewportH     int
	IdlePark      time.Duration
	MaxAge        time.Duration
	MaxRunning    int
	ViewTTL       time.Duration
	UploadTTL     time.Duration
	ViewBase      string // base URL for the links this server hands out: live views and uploads
	Verbose       bool
}

// sessionMeta is what survives in session.json: enough to list a parked
// session and to relaunch it the way it was started.
type sessionMeta struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Mode      string    `json:"mode"`
	Identity  string    `json:"identity,omitempty"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"last_used"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Device    string    `json:"device,omitempty"`   // device profile name (device.go), see deviceName
	Timezone  string    `json:"timezone,omitempty"` // IANA zone Chrome runs in; "" is the server's
	Locale    string    `json:"locale,omitempty"`   // Chrome's --lang; "" is the server's
	LastURL   string    `json:"last_url,omitempty"`
	LastTitle string    `json:"last_title,omitempty"`
	Tabs      []string  `json:"tabs,omitempty"` // URLs open when parked, reopened on resume
}

type session struct {
	mgr  *manager
	dir  string
	meta sessionMeta

	// mu serializes every operation on the session: Chrome is driven one
	// action at a time, and the tab table is only touched under it.
	mu sync.Mutex

	chrome     *chromeProc
	disp       *display
	emu        *emulator
	allocCtx   context.Context
	allocStop  context.CancelFunc
	browserCtx context.Context
	tabs       map[target.ID]*tab
	closing    map[target.ID]time.Time // tabs we closed; Chrome may list them a moment longer
	aliases    map[string]target.ID    // "t1" -> target
	nextTab    int
	current    target.ID
	front      target.ID // the tab last brought to the front (headless hides the others)
	devtools   *devtoolsClient
	viewers    atomic.Int32 // open live-view connections
	lastUsed   atomic.Int64 // unix nanos; the meta's copy is written on park
}

// tab is one attached page target.
type tab struct {
	id     target.ID
	seq    int // adoption order; the alias is "t<seq>"
	alias  string
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	console []consoleEntry
	dialogs []string // messages of dialogs auto-handled since the last report
}

type consoleEntry struct {
	When  time.Time
	Level string
	Text  string
}

const maxConsole = 300

func (t *tab) addConsole(level, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.console = append(t.console, consoleEntry{When: time.Now(), Level: level, Text: text})
	if len(t.console) > maxConsole {
		t.console = t.console[len(t.console)-maxConsole:]
	}
}

// takeDialogs returns and clears the dialogs handled since the last call.
func (t *tab) takeDialogs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.dialogs
	t.dialogs = nil
	return d
}

type manager struct {
	cfg        *managerConfig
	identities *identityStore
	views      *viewTokens
	uploads    *uploadTokens

	mu       sync.Mutex
	sessions map[string]*session

	verMu       sync.Mutex
	ver         *chromeVersion // the binary's version, probed on first need
	fonts       *fontSet       // the machine's fonts, listed on first need
	fontsListed bool
}

// installedFonts is the machine's fonts, listed once.
func (m *manager) installedFonts() *fontSet {
	m.verMu.Lock()
	defer m.verMu.Unlock()
	if !m.fontsListed {
		m.fonts = installedFonts()
		m.fontsListed = true
		if m.fonts == nil {
			logf("fc-list unavailable: device profiles cannot present the machine's fonts under their own family names")
		}
	}
	return m.fonts
}

// chromeVersion is the Chrome binary's version, probed once.
func (m *manager) chromeVersion() (chromeVersion, error) {
	m.verMu.Lock()
	defer m.verMu.Unlock()
	if m.ver == nil {
		v, err := probeChromeVersion(m.cfg.Chrome, m.cfg.NoSandbox)
		if err != nil {
			return chromeVersion{}, err
		}
		m.ver = &v
	}
	return *m.ver, nil
}

func newManager(cfg *managerConfig) (*manager, error) {
	if err := os.MkdirAll(cfg.SessionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("sessions dir: %w", err)
	}
	ids, err := newIdentityStore(cfg.IdentitiesDir)
	if err != nil {
		return nil, err
	}
	m := &manager{
		cfg: cfg, identities: ids, sessions: map[string]*session{},
		views:   newViewTokens(cfg.ViewTTL),
		uploads: newUploadTokens(cfg.UploadTTL),
	}
	m.loadParked()
	return m, nil
}

// loadParked picks up sessions a previous process left in the directory,
// as parked ones. Their Chrome is gone (it was our child) but the profile
// is intact, so they resume like any parked session.
func (m *manager) loadParked() {
	entries, err := os.ReadDir(m.cfg.SessionsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.cfg.SessionsDir, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "session.json"))
		if err != nil {
			continue
		}
		var meta sessionMeta
		if json.Unmarshal(b, &meta) != nil || meta.ID != e.Name() {
			continue
		}
		s := &session{mgr: m, dir: dir, meta: meta}
		s.lastUsed.Store(meta.LastUsed.UnixNano())
		m.sessions[meta.ID] = s
		logf("session %s: found parked (%s, last used %s)", meta.ID, meta.Mode, meta.LastUsed.Format(time.RFC3339))
	}
}

func newSessionID() string {
	var b [4]byte
	rand.Read(b[:])
	return "s-" + hex.EncodeToString(b[:])
}

type startOptions struct {
	Mode     string
	Identity string
	Label    string
	Width    int
	Height   int
	Device   string
	Timezone string
	Locale   string
}

var localeRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// start creates a session — a Chrome on a fresh profile, or on a copy of an
// identity's — and launches it.
func (m *manager) start(ctx context.Context, o startOptions) (*session, error) {
	switch o.Mode {
	case "", modeHeadless:
		o.Mode = modeHeadless
	case modeHeadful:
		if m.cfg.Xvnc == "" {
			return nil, errors.New("headful sessions are unavailable on this server (no Xvnc); use mode \"headless\"")
		}
	default:
		return nil, fmt.Errorf("mode %q: want headless or headful", o.Mode)
	}
	if o.Width == 0 || o.Height == 0 {
		o.Width, o.Height = m.cfg.ViewportW, m.cfg.ViewportH
	}
	if o.Width < 200 || o.Height < 200 || o.Width > 4096 || o.Height > 4096 {
		return nil, fmt.Errorf("viewport %dx%d: want 200..4096 on each side", o.Width, o.Height)
	}
	if o.Identity != "" {
		if _, err := m.identities.get(o.Identity); err != nil {
			return nil, err
		}
	}
	dev, err := lookupDevice(o.Device)
	if err != nil {
		return nil, err
	}
	// Recorded by name, so the session keeps the profile it was started with
	// whatever the default becomes.
	o.Device = dev.Name
	if o.Timezone != "" {
		if _, err := time.LoadLocation(o.Timezone); err != nil {
			return nil, fmt.Errorf("timezone %q: want an IANA zone such as Europe/London or Australia/Sydney", o.Timezone)
		}
	}
	if o.Locale != "" && !localeRe.MatchString(o.Locale) {
		return nil, fmt.Errorf("locale %q: want a language tag such as en-US or de-DE", o.Locale)
	}

	id := newSessionID()
	dir := filepath.Join(m.cfg.SessionsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	s := &session{mgr: m, dir: dir, meta: sessionMeta{
		ID: id, Label: o.Label, Mode: o.Mode, Identity: o.Identity,
		Created: now, LastUsed: now, Width: o.Width, Height: o.Height,
		Device: o.Device, Timezone: o.Timezone, Locale: o.Locale,
	}}
	s.lastUsed.Store(now.UnixNano())
	if o.Identity != "" {
		if err := m.identities.seed(o.Identity, s.dir); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("seeding profile from identity %q: %w", o.Identity, err)
		}
	}
	if err := s.saveMeta(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	m.makeRoom(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.launch(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	logf("session %s: started (%s%s%s)", id, o.Mode, identitySuffix(o.Identity), s.meta.deviceSuffix())
	return s, nil
}

func identitySuffix(identity string) string {
	if identity == "" {
		return ""
	}
	return ", identity " + identity
}

// deviceName is the session's device profile. Metadata written before
// profiles were recorded has none, and those sessions were this Chrome as
// it is -- not whatever the default is now.
func (m *sessionMeta) deviceName() string {
	if m.Device == "" {
		return nativeDevice
	}
	return m.Device
}

// deviceSuffix describes the emulation for listings: ", device windows,
// timezone Europe/London".
func (m *sessionMeta) deviceSuffix() string {
	parts := []string{"device " + m.deviceName()}
	if m.Timezone != "" {
		parts = append(parts, "timezone "+m.Timezone)
	}
	if m.Locale != "" {
		parts = append(parts, "locale "+m.Locale)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// get finds a session by id; running or parked.
func (m *manager) get(id string) (*session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no session %q (session_list shows what exists; session_start makes one)", id)
	}
	return s, nil
}

// running finds a session and makes sure its Chrome is up, resuming a
// parked one. The caller holds no lock; the session is returned unlocked.
func (m *manager) running(ctx context.Context, id string) (*session, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isRunning() {
		return s, nil
	}
	m.makeRoom(id)
	if err := s.launch(ctx); err != nil {
		return nil, fmt.Errorf("resuming session %s: %w", id, err)
	}
	logf("session %s: resumed", id)
	return s, nil
}

// makeRoom parks least-recently-used sessions until one more Chrome fits
// under MaxRunning. keep is the session about to run and is never parked.
func (m *manager) makeRoom(keep string) {
	if m.cfg.MaxRunning <= 0 {
		return
	}
	for {
		var running []*session
		m.mu.Lock()
		for _, s := range m.sessions {
			if s.meta.ID != keep && s.isRunningQuick() {
				running = append(running, s)
			}
		}
		m.mu.Unlock()
		if len(running) < m.cfg.MaxRunning {
			return
		}
		sort.Slice(running, func(i, j int) bool { return running[i].lastUsed.Load() < running[j].lastUsed.Load() })
		victim := running[0]
		logf("session %s: parking to make room (max-running %d)", victim.meta.ID, m.cfg.MaxRunning)
		victim.mu.Lock()
		victim.park()
		victim.mu.Unlock()
	}
}

func (m *manager) list() []*session {
	m.mu.Lock()
	out := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].lastUsed.Load() > out[j].lastUsed.Load() })
	return out
}

// remove deletes a session: Chrome closed, profile gone.
func (m *manager) remove(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.park()
	s.mu.Unlock()
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.views.revokeSession(id)
	m.uploads.revokeSession(id)
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	logf("session %s: deleted", id)
	return nil
}

// reaper parks idle sessions and deletes old ones.
func (m *manager) reaper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, s := range m.list() {
			idle := time.Since(time.Unix(0, s.lastUsed.Load()))
			if m.cfg.MaxAge > 0 && idle > m.cfg.MaxAge && s.viewers.Load() == 0 {
				logf("session %s: unused for %s, deleting", s.meta.ID, idle.Round(time.Minute))
				m.remove(s.meta.ID)
				continue
			}
			if m.cfg.IdlePark > 0 && idle > m.cfg.IdlePark && s.viewers.Load() == 0 {
				s.mu.Lock()
				if s.isRunning() {
					logf("session %s: idle for %s, parking", s.meta.ID, idle.Round(time.Minute))
					s.park()
				}
				s.mu.Unlock()
			}
		}
	}
}

// shutdown parks every running session, so their profiles are consistent
// on disk should the directory survive the process.
func (m *manager) shutdown() {
	var wg sync.WaitGroup
	for _, s := range m.list() {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			s.mu.Lock()
			s.park()
			s.mu.Unlock()
		}(s)
	}
	wg.Wait()
}

// ---- session ----

func (s *session) profileDir() string   { return filepath.Join(s.dir, "profile") }
func (s *session) downloadsDir() string { return filepath.Join(s.dir, "downloads") }

// filesDir holds the files the agent has put on the session for a page to
// be given (files.go). Inside the session directory on purpose: deleting
// the session deletes them.
func (s *session) filesDir() string { return filepath.Join(s.dir, "files") }

// mergeProfilePrefs deep-merges prefs into the profile's Default/Preferences
// JSON (Chrome's per-profile settings), creating it if need be. Chrome
// rewrites this file on a clean exit, keeping what it does not manage, so a
// merge each launch is idempotent and survives park/resume and an identity's
// own settings. The font families here are plain (unprotected) preferences,
// not the HMAC-guarded "Secure Preferences", so writing them is safe.
func mergeProfilePrefs(profileDir string, prefs map[string]any) error {
	if len(prefs) == 0 {
		return nil
	}
	dir := filepath.Join(profileDir, "Default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "Preferences")
	current := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &current); err != nil {
			// A corrupt Preferences file: Chrome would reset it anyway.
			current = map[string]any{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	deepMerge(current, prefs)
	b, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// deepMerge recursively merges src into dst: nested objects are merged, and
// a leaf in src replaces the one in dst.
func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}

func (s *session) saveMeta() error {
	s.meta.LastUsed = time.Unix(0, s.lastUsed.Load())
	b, err := json.MarshalIndent(&s.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "session.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "session.json"))
}

func (s *session) touch() { s.lastUsed.Store(time.Now().UnixNano()) }

// isRunning needs s.mu; isRunningQuick is the lock-free approximation the
// manager uses for counting.
func (s *session) isRunning() bool      { return s.chrome != nil && s.chrome.Alive() }
func (s *session) isRunningQuick() bool { return s.chrome != nil && s.chrome.Alive() }

func (s *session) state() string {
	if s.isRunningQuick() {
		return "running"
	}
	return "parked"
}

// launch starts the display (headful) and Chrome, connects, and adopts the
// tabs Chrome opened. Caller holds s.mu.
func (s *session) launch(ctx context.Context) error {
	if s.chrome != nil || s.disp != nil {
		// A Chrome that died on its own leaves its display and contexts
		// behind; clear them before starting over.
		s.park()
	}
	cfg := s.mgr.cfg
	w, h := s.meta.Width, s.meta.Height
	dev, err := lookupDevice(s.meta.deviceName())
	if err != nil {
		return err
	}
	var ver chromeVersion
	if dev.emulated() {
		if ver, err = s.mgr.chromeVersion(); err != nil {
			return fmt.Errorf("device %s: %w", dev.Name, err)
		}
	}
	var disp *display
	if s.meta.Mode == modeHeadful {
		var err error
		disp, err = startDisplay(ctx, cfg.Xvnc, s.meta.ID, w, h+headfulChromeHeight, cfg.Verbose)
		if err != nil {
			return err
		}
	}
	l := &chromeLaunch{
		Exe:         cfg.Chrome,
		UserDataDir: s.profileDir(),
		Downloads:   s.downloadsDir(),
		Headless:    s.meta.Mode == modeHeadless,
		Width:       w,
		Height:      h,
		NoSandbox:   cfg.NoSandbox,
		ExtraFlags:  append(append([]string(nil), cfg.ChromeFlags...), dev.chromeFlags(ver)...),
		Verbose:     cfg.Verbose,
		Logf:        logf,
	}
	if fc, err := dev.fontsConfFile(cfg.SessionsDir, s.mgr.installedFonts()); err != nil {
		logf("session %s: fonts configuration: %v", s.meta.ID, err)
	} else if fc != "" {
		l.Env = append(l.Env, "FONTCONFIG_FILE="+fc)
	}
	// The generic-family font defaults go into the profile's Preferences,
	// where Blink reads them, before Chrome opens it. Merged, so an
	// identity's own settings and a resumed profile's survive.
	if err := mergeProfilePrefs(s.profileDir(), dev.fontPrefs()); err != nil {
		logf("session %s: font preferences: %v", s.meta.ID, err)
	}
	if s.meta.Locale != "" {
		flags, env := localeLaunch(s.meta.Locale)
		l.ExtraFlags = append(l.ExtraFlags, flags...)
		l.Env = append(l.Env, env...)
	}
	if s.meta.Timezone != "" {
		l.Env = append(l.Env, "TZ="+s.meta.Timezone)
	}
	if disp != nil {
		l.Display = disp.Display()
		l.Height = h + headfulChromeHeight
	} else if dev.emulated() {
		l.Width, l.Height = w+windowFrameWidth, h+headfulChromeHeight
	}
	proc, err := launchChrome(ctx, l)
	if err != nil {
		if disp != nil {
			disp.stop()
		}
		return err
	}
	// The device profile goes on before anything connects, so the first
	// tab carries it from its first request.
	emu, err := startEmulator(ctx, proc.WSURL, &emulationSpec{
		UserAgent: dev.userAgentOverride(ver),
		Metrics:   dev.metrics(w, h, s.meta.Mode == modeHeadless),
		Script:    dev.initScript(ver),
		CHHeaders: dev.clientHintHeaders(ver),
	}, logf)
	if err != nil {
		proc.stop(2 * time.Second)
		if disp != nil {
			disp.stop()
		}
		return fmt.Errorf("device emulation: %w", err)
	}

	allocCtx, allocStop := chromedp.NewRemoteAllocator(context.Background(), proc.WSURL, chromedp.NoModifyURL)
	opts := []chromedp.ContextOption{chromedp.WithErrorf(func(f string, a ...any) { logf("chromedp: "+f, a...) })}
	if cfg.Verbose {
		opts = append(opts, chromedp.WithLogf(func(f string, a ...any) { logf("chromedp: "+f, a...) }))
	}
	if os.Getenv("CHROMEMCP_CDP_DEBUG") != "" {
		opts = append(opts, chromedp.WithDebugf(func(f string, a ...any) { logf("cdp: "+f, a...) }))
	}
	browserCtx, _ := chromedp.NewContext(allocCtx, opts...)

	s.chrome, s.disp, s.emu = proc, disp, emu
	s.allocCtx, s.allocStop, s.browserCtx = allocCtx, allocStop, browserCtx
	s.tabs = map[target.ID]*tab{}
	s.closing = map[target.ID]time.Time{}
	s.aliases = map[string]target.ID{}
	s.nextTab = 0
	s.current = ""

	connect := func() error {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		// Connecting: Targets is the first call on the browser websocket.
		if _, err := chromedp.Targets(browserCtx); err != nil {
			return fmt.Errorf("connecting to chrome: %w", err)
		}
		c := chromedp.FromContext(browserCtx)
		bctx := cdp.WithExecutor(cctx, c.Browser)
		if err := browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).
			WithDownloadPath(s.downloadsDir()).WithEventsEnabled(true).Do(bctx); err != nil {
			logf("session %s: download behaviour: %v", s.meta.ID, err)
		}
		// Chrome opens its first tab; it can take a moment to appear as
		// a target.
		found := false
		for i := 0; i < 100 && !found; i++ {
			if err := s.syncTabs(cctx); err != nil {
				return err
			}
			found = len(s.tabs) > 0
			if !found {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if !found {
			if _, err := s.newTab(cctx, "about:blank"); err != nil {
				return err
			}
		}
		// The jar this server keeps goes in before any page loads.
		if err := s.importCookies(cctx); err != nil {
			logf("session %s: %v", s.meta.ID, err)
		}
		// Then the tabs that were open when the session was parked: the
		// first in the tab Chrome opened, the rest as new ones.
		if len(s.meta.Tabs) > 0 {
			first, err := s.resolveTab("")
			if err == nil {
				if err := navigate(cctx, first, s.meta.Tabs[0], 15*time.Second); err != nil {
					logf("session %s: reopening %s: %v", s.meta.ID, s.meta.Tabs[0], err)
				}
			}
			for _, u := range s.meta.Tabs[1:] {
				if _, err := s.newTab(cctx, u); err != nil {
					logf("session %s: reopening %s: %v", s.meta.ID, u, err)
				}
			}
			if first != nil {
				s.current = first.id
			}
		}
		return nil
	}
	if err := connect(); err != nil {
		s.park()
		return err
	}
	return nil
}

// park closes Chrome (and the display) and keeps the profile. Caller holds
// s.mu. Safe to call on a session that is not running.
func (s *session) park() {
	if s.devtools != nil {
		s.devtools.close()
		s.devtools = nil
	}
	if s.chrome != nil {
		if s.chrome.Alive() && s.browserCtx != nil {
			c := chromedp.FromContext(s.browserCtx)
			if c != nil && c.Browser != nil {
				ctx, cancel := context.WithTimeout(s.browserCtx, 5*time.Second)
				// Remember the tabs: a cleanly closed Chrome starts blank.
				if infos, err := chromedp.Targets(ctx); err == nil {
					s.meta.Tabs = nil
					for _, t := range s.sortedTabs() {
						for _, info := range infos {
							if info.TargetID == t.id && info.URL != "" && info.URL != "about:blank" && !strings.HasPrefix(info.URL, "chrome://") {
								s.meta.Tabs = append(s.meta.Tabs, info.URL)
							}
						}
					}
				}
				if err := s.exportCookies(); err != nil {
					logf("session %s: %v", s.meta.ID, err)
				}
				cancel()
			}
		}
		// A clean exit where Chrome grants one, so the profile's databases
		// are consistent for a resume or an identity snapshot.
		s.chrome.stop(5 * time.Second)
	}
	if s.emu != nil {
		s.emu.close()
	}
	if s.allocStop != nil {
		s.allocStop()
	}
	if s.disp != nil {
		s.disp.stop()
	}
	s.chrome, s.disp, s.emu, s.allocCtx, s.allocStop, s.browserCtx = nil, nil, nil, nil, nil, nil
	s.tabs, s.closing, s.aliases, s.current, s.front = nil, nil, nil, "", ""
	if err := s.saveMeta(); err != nil {
		logf("session %s: saving metadata: %v", s.meta.ID, err)
	}
}

// restart parks and relaunches: what an identity snapshot needs around the
// profile copy. Caller holds s.mu.
func (s *session) restart(ctx context.Context) error {
	s.park()
	return s.launch(ctx)
}

// syncTabs reconciles the tab table with Chrome's page targets: adopts new
// ones (opened by a page, or by the user on the live view) and drops the
// closed ones. Caller holds s.mu.
func (s *session) syncTabs(ctx context.Context) error {
	infos, err := chromedp.Targets(s.browserCtx)
	if err != nil {
		return fmt.Errorf("listing tabs: %w", err)
	}
	seen := map[target.ID]bool{}
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		if closed, ok := s.closing[info.TargetID]; ok {
			if time.Since(closed) < 5*time.Second {
				continue
			}
			delete(s.closing, info.TargetID)
		}
		seen[info.TargetID] = true
		if _, ok := s.tabs[info.TargetID]; ok {
			continue
		}
		if err := s.adopt(ctx, info.TargetID); err != nil {
			logf("session %s: attaching to tab %s: %v", s.meta.ID, info.TargetID, err)
		}
	}
	for id, t := range s.tabs {
		if !seen[id] {
			delete(s.tabs, id)
			delete(s.aliases, t.alias)
			t.cancel()
		}
	}
	if _, ok := s.tabs[s.current]; !ok {
		s.current = ""
		// The oldest surviving tab.
		var best *tab
		for _, t := range s.tabs {
			if best == nil || t.seq < best.seq {
				best = t
			}
		}
		if best != nil {
			s.current = best.id
		}
	}
	return nil
}

// adopt attaches to a page target and starts collecting its console and
// answering its dialogs. Caller holds s.mu.
func (s *session) adopt(ctx context.Context, id target.ID) error {
	tctx, cancel := chromedp.NewContext(s.browserCtx, chromedp.WithTargetID(id))
	// Attach now rather than on first use. The target's event loop lives on
	// the context this first Run gets, so it must be the tab's own; the
	// timeout is enforced from outside.
	errc := make(chan error, 1)
	go func() { errc <- chromedp.Run(tctx) }()
	var err error
	select {
	case err = <-errc:
	case <-time.After(15 * time.Second):
		err = errors.New("timed out attaching")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		return err
	}
	// The viewport (headless Chrome's window size includes a virtual
	// toolbar) and the device profile were put on the target by the
	// emulator when Chrome created it, before it ran anything.
	s.nextTab++
	t := &tab{id: id, seq: s.nextTab, alias: fmt.Sprintf("t%d", s.nextTab), ctx: tctx, cancel: cancel}
	s.tabs[id] = t
	s.aliases[t.alias] = id
	if s.current == "" {
		s.current = id
	}
	chromedp.ListenTarget(tctx, func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			var parts []string
			for _, a := range ev.Args {
				parts = append(parts, remoteObjectString(a))
			}
			t.addConsole(string(ev.Type), strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			text := ev.ExceptionDetails.Text
			if ev.ExceptionDetails.Exception != nil {
				text += " " + remoteObjectString(ev.ExceptionDetails.Exception)
			}
			t.addConsole("exception", text)
		case *page.EventJavascriptDialogOpening:
			// Accept, so the page never blocks on a dialog nobody can
			// see; the message is reported with the next result.
			msg := fmt.Sprintf("%s dialog: %q (accepted)", ev.Type, ev.Message)
			t.mu.Lock()
			t.dialogs = append(t.dialogs, msg)
			t.mu.Unlock()
			go chromedp.Run(tctx, page.HandleJavaScriptDialog(true))
		}
	})
	return nil
}

// newTab opens a page and makes it current. Caller holds s.mu. The tab
// is created blank and then navigated: a target created at a URL has its
// first request on the way before the emulator can reach it.
func (s *session) newTab(ctx context.Context, url string) (*tab, error) {
	c := chromedp.FromContext(s.browserCtx)
	id, err := target.CreateTarget("about:blank").Do(cdp.WithExecutor(ctx, c.Browser))
	if err != nil {
		return nil, fmt.Errorf("opening tab: %w", err)
	}
	if err := s.adopt(ctx, id); err != nil {
		return nil, err
	}
	s.current = id
	t := s.tabs[id]
	if url != "" && url != "about:blank" {
		if err := navigate(ctx, t, url, 30*time.Second); err != nil {
			return t, err
		}
	}
	return t, nil
}

// resolveTab finds a tab by alias or target id; "" is the current tab.
// Caller holds s.mu.
func (s *session) resolveTab(ref string) (*tab, error) {
	if ref == "" {
		if t, ok := s.tabs[s.current]; ok {
			return t, nil
		}
		return nil, errors.New("the session has no tabs; browser_tab_new opens one")
	}
	if id, ok := s.aliases[ref]; ok {
		return s.tabs[id], nil
	}
	if t, ok := s.tabs[target.ID(ref)]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("no tab %q in session %s (browser_tabs lists them)", ref, s.meta.ID)
}

// closeTab closes a page target. Caller holds s.mu.
func (s *session) closeTab(ctx context.Context, t *tab) error {
	delete(s.tabs, t.id)
	delete(s.aliases, t.alias)
	s.closing[t.id] = time.Now()
	t.cancel() // chromedp closes the target on cancel
	if s.current == t.id {
		s.current = ""
	}
	return s.syncTabs(ctx)
}

// remoteObjectString renders a console argument: primitives as they are,
// objects from the preview Chrome sends with console events.
func remoteObjectString(o *runtime.RemoteObject) string {
	if o == nil {
		return ""
	}
	if o.Value != nil {
		var v any
		if json.Unmarshal(o.Value, &v) == nil {
			if str, ok := v.(string); ok {
				return str
			}
		}
		return string(o.Value)
	}
	if p := o.Preview; p != nil && len(p.Properties) > 0 {
		var sb strings.Builder
		open, close := "{", "}"
		if p.Subtype == "array" {
			open, close = "[", "]"
		}
		sb.WriteString(open)
		for i, prop := range p.Properties {
			if i > 0 {
				sb.WriteString(", ")
			}
			if p.Subtype != "array" {
				sb.WriteString(prop.Name + ": ")
			}
			if prop.Type == "string" {
				sb.WriteString(strconv.Quote(prop.Value))
			} else if prop.Value != "" {
				sb.WriteString(prop.Value)
			} else {
				sb.WriteString(string(prop.Type))
			}
		}
		if p.Overflow {
			sb.WriteString(", …")
		}
		sb.WriteString(close)
		return sb.String()
	}
	if o.Description != "" {
		return o.Description
	}
	return string(o.Type)
}
