package main

// chromemcp node: Chrome sessions on another machine, for a server that
// cannot run them itself -- a Mac, whose Chrome is a real Mac's, with its
// GPU and fonts, where the server's container has SwiftShader and stand-ins.
//
// The node is deliberately small. It launches and stops Chrome for a session
// id, relays that Chrome's DevTools endpoint, and keeps the files a page is
// given or downloads. Everything else -- MCP, OAuth, the device profile,
// driving the page, cookies, parking, the reaper -- stays on the server,
// which drives the node's Chrome over the relayed DevTools socket exactly as
// it drives its own (remote.go). So the node holds no state the server does
// not ask for, and a session is still the server's: its session.json, cookie
// jar and put files live there, and only the Chrome profile lives here.
//
// It speaks HTTPS with mutual TLS (tlsutil.go) and, by default, listens on
// loopback only: the way in from the network is `tailscale serve --tcp`,
// which forwards raw TCP, so the TLS runs end to end between server and
// node. Chrome's DevTools ports come from a fixed range, so that a firewall
// can confine the account the node runs as to the public internet plus
// exactly those loopback ports (README: Nodes).
//
// API, all under /v1 and all requiring an admitted client certificate:
//
//	GET    /info                               versions, OS, limits
//	POST   /sessions/{id}/launch               start Chrome for a session
//	POST   /sessions/{id}/stop                 close it (the profile stays)
//	GET    /sessions/{id}/wait                 block until it exits (or a timeout)
//	DELETE /sessions/{id}                      close it and delete everything
//	*      /sessions/{id}/cdp/...              its DevTools endpoint, HTTP and websocket
//	GET    /sessions/{id}/profile              the profile, as an archive (Chrome stopped)
//	PUT    /sessions/{id}/profile              replace it: an identity to start from
//	GET    /sessions/{id}/downloads            what Chrome downloaded
//	DELETE /sessions/{id}/downloads/{name}
//	PUT    /sessions/{id}/files/{name}         a file for a page's file picker
//	DELETE /sessions/{id}/files/{name}

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type nodeConfig struct {
	SessionsDir     string
	Chrome          string
	PortLow         int // DevTools ports, inclusive
	PortHigh        int
	MaxRunning      int
	DisableFeatures []string
	AllowClients    []string // client certificate names admitted; empty admits any the CA signed
	Verbose         bool
}

// nodeInfo is GET /v1/info.
type nodeInfo struct {
	Version    string   `json:"version"`
	Chrome     string   `json:"chrome"` // full version, "154.0.8037.57"
	OS         string   `json:"os"`     // Go's name: darwin, linux
	OSVersion  string   `json:"os_version"`
	Arch       string   `json:"arch"`
	MaxRunning int      `json:"max_running"`
	Running    []string `json:"running"`
}

type nodeLaunchRequest struct {
	Headless bool           `json:"headless"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Flags    []string       `json:"flags,omitempty"`
	Env      []string       `json:"env,omitempty"`
	Prefs    map[string]any `json:"prefs,omitempty"`
	// Locale is the UI language, and Intl's default locale with it. Chrome
	// on macOS takes it from neither --lang nor the environment, only from
	// the AppleLanguages preference, so the node sets that (setChromeLocale).
	Locale string `json:"locale,omitempty"`
}

type nodeLaunchResponse struct {
	BrowserPath string `json:"browser_path"` // /devtools/browser/<id>
	Downloads   string `json:"downloads"`    // Chrome's download directory, a path on the node
	Chrome      string `json:"chrome"`
}

// nodeFile is one entry of a downloads listing.
type nodeFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Partial  bool      `json:"partial,omitempty"`
	Path     string    `json:"path"`
}

type nodeSession struct {
	dir  string
	proc *chromeProc
	port int
}

type nodeServer struct {
	cfg *nodeConfig

	launchMu sync.Mutex // one launch at a time: the port and the running count are decided under it
	mu       sync.Mutex
	sessions map[string]*nodeSession

	verMu sync.Mutex
	ver   chromeVersion
	verAt time.Time

	bundleOnce sync.Once
	bundleID   string // Chrome's macOS bundle identifier, com.google.Chrome
}

var nodeSessionIDRe = regexp.MustCompile(`^s-[0-9a-f]{8}$`)

// The flags a server may add to a node's Chrome. The node builds the rest
// of the command line itself; anything else the server asked for would be
// a way to run a command as the node's account (--renderer-cmd-prefix and
// friends), to move the profile, or to open the DevTools port wider.
var nodeFlagPrefixes = []string{"--user-agent=", "--blink-settings=", "--accept-lang=", "--lang="}

// And the environment: where and in what language Chrome runs, nothing that
// changes how it is loaded (DYLD_*).
var nodeEnvPrefixes = []string{"TZ=", "LANG=", "LANGUAGE="}

func allowedPrefix(v string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

func defaultNodeSessionsDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "chromemcp-node", "sessions")
	}
	return filepath.Join(os.TempDir(), "chromemcp-node-sessions")
}

func parsePortRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		b = a
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || lo < 1024 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("ports %q: want LOW-HIGH, unprivileged", s)
	}
	return lo, hi, nil
}

func node(args []string) int {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	defFeatures := ""
	if hostOS == "darwin" {
		// Chrome's own DNS client sends queries straight to the resolver on
		// the LAN, which the node account's firewall blocks; without it,
		// names resolve through mDNSResponder, whose queries are not this
		// account's (measured on macOS 27).
		defFeatures = "AsyncDns"
	}
	var (
		listen      = fs.String("listen", env("CHROMEMCP_NODE_LISTEN", "127.0.0.1:9310"), "HTTPS address to serve on; loopback, with `tailscale serve --tcp` in front, is the intended shape (env CHROMEMCP_NODE_LISTEN)")
		certFile    = fs.String("tls-cert", env("CHROMEMCP_NODE_TLS_CERT", ""), "this node's certificate, PEM; re-read when it changes (env CHROMEMCP_NODE_TLS_CERT)")
		keyFile     = fs.String("tls-key", env("CHROMEMCP_NODE_TLS_KEY", ""), "its key (env CHROMEMCP_NODE_TLS_KEY)")
		caFile      = fs.String("client-ca", env("CHROMEMCP_NODE_CLIENT_CA", ""), "the CA client certificates must chain to, PEM (env CHROMEMCP_NODE_CLIENT_CA)")
		allow       multiFlag
		chromePath  = fs.String("chrome", defaultChrome(), "the Chrome binary (env CHROMEMCP_CHROME)")
		sessionsDir = fs.String("sessions-dir", env("CHROMEMCP_NODE_SESSIONS_DIR", defaultNodeSessionsDir()), "where session profiles and files live (env CHROMEMCP_NODE_SESSIONS_DIR)")
		ports       = fs.String("ports", env("CHROMEMCP_NODE_PORTS", "9300-9309"), "DevTools port range, LOW-HIGH; the firewall must let this account reach them on loopback (env CHROMEMCP_NODE_PORTS)")
		maxRunning  = fs.Int("max-running", envInt("CHROMEMCP_NODE_MAX_RUNNING", 1), "most Chrome instances alive at once (env CHROMEMCP_NODE_MAX_RUNNING)")
		features    = fs.String("disable-features", env("CHROMEMCP_NODE_DISABLE_FEATURES", defFeatures), "comma-separated Chrome features to turn off (env CHROMEMCP_NODE_DISABLE_FEATURES)")
		verbose     = fs.Bool("verbose", false, "log Chrome's stderr")
	)
	fs.Var(&allow, "allow-client", "a client certificate name (CN, DNS, email or URI SAN) admitted; repeatable; none admits every certificate the CA signed (env CHROMEMCP_NODE_ALLOW_CLIENTS, space-separated)")
	fs.Parse(args)

	if *certFile == "" || *keyFile == "" || *caFile == "" {
		die(errors.New("a node needs -tls-cert, -tls-key and -client-ca: it speaks mutual TLS only"))
	}
	if *chromePath == "" {
		die(errors.New("no Chrome found: set -chrome or CHROMEMCP_CHROME"))
	}
	lo, hi, err := parsePortRange(*ports)
	if err != nil {
		die(err)
	}
	allowed := []string(allow)
	if len(allowed) == 0 {
		allowed = strings.Fields(os.Getenv("CHROMEMCP_NODE_ALLOW_CLIENTS"))
	}
	var disabled []string
	for _, f := range strings.Split(*features, ",") {
		if f = strings.TrimSpace(f); f != "" {
			disabled = append(disabled, f)
		}
	}
	tlsCfg, err := serverTLS(*certFile, *keyFile, *caFile)
	if err != nil {
		die(fmt.Errorf("tls: %w", err))
	}
	syscall.Umask(0o077)
	ns, err := newNodeServer(&nodeConfig{
		SessionsDir: *sessionsDir, Chrome: *chromePath, PortLow: lo, PortHigh: hi,
		MaxRunning: max(*maxRunning, 1), DisableFeatures: disabled, AllowClients: allowed, Verbose: *verbose,
	})
	if err != nil {
		die(err)
	}
	if len(allowed) == 0 {
		logf("node: no -allow-client: any certificate the CA signed is admitted")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die(err)
	}
	srv := &http.Server{Handler: ns.handler(), TLSConfig: tlsCfg, ReadHeaderTimeout: 30 * time.Second}
	logf("node: HTTPS on %s; chrome %s; sessions in %s; DevTools ports %d-%d; at most %d running", ln.Addr(), *chromePath, *sessionsDir, lo, hi, ns.cfg.MaxRunning)
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	select {
	case err = <-errc:
	case <-ctx.Done():
		logf("node: shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv.Shutdown(sctx)
		cancel()
	}
	ns.shutdown()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		die(err)
	}
	return 0
}

func newNodeServer(cfg *nodeConfig) (*nodeServer, error) {
	if err := os.MkdirAll(cfg.SessionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("sessions dir: %w", err)
	}
	ns := &nodeServer{cfg: cfg, sessions: map[string]*nodeSession{}}
	ns.sweep()
	return ns, nil
}

// sweep stops Chromes a previous run of the node left behind. They are in
// process groups of their own, so they outlive a node that crashed or was
// killed, still holding their DevTools ports and profiles.
func (ns *nodeServer) sweep() {
	entries, _ := os.ReadDir(ns.cfg.SessionsDir)
	for _, e := range entries {
		pidFile := filepath.Join(ns.cfg.SessionsDir, e.Name(), "chrome.pid")
		b, err := os.ReadFile(pidFile)
		if err != nil {
			continue
		}
		os.Remove(pidFile)
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 1 {
			continue
		}
		// Only if the pid is still a Chrome: the number may have been reused.
		out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || !strings.Contains(string(out), filepath.Base(ns.cfg.Chrome)) {
			continue
		}
		logf("node: stopping Chrome %d left by a previous run (session %s)", pid, e.Name())
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func (ns *nodeServer) shutdown() {
	ns.mu.Lock()
	var procs []*chromeProc
	for _, s := range ns.sessions {
		if s.proc != nil {
			procs = append(procs, s.proc)
		}
	}
	ns.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func() { defer wg.Done(); p.stop(3 * time.Second) }()
	}
	wg.Wait()
}

func (ns *nodeServer) chromeVersion() (chromeVersion, error) {
	ns.verMu.Lock()
	defer ns.verMu.Unlock()
	// Chrome on a Mac updates itself in place, so the answer is only kept
	// for a minute.
	if ns.ver.Full != "" && time.Since(ns.verAt) < time.Minute {
		return ns.ver, nil
	}
	v, err := probeChromeVersion(ns.cfg.Chrome, false)
	if err != nil {
		return chromeVersion{}, err
	}
	ns.ver, ns.verAt = v, time.Now()
	return v, nil
}

// osVersion is the version a browser on this machine reports as its
// platform version: macOS's own ("27.0", from sw_vers), or the kernel's.
func osVersion() string {
	var out []byte
	if hostOS == "darwin" {
		out, _ = exec.Command("sw_vers", "-productVersion").Output()
	} else {
		out, _ = exec.Command("uname", "-r").Output()
	}
	return strings.TrimSpace(string(out))
}

// ---- HTTP ----

func (ns *nodeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", ns.info)
	mux.HandleFunc("POST /v1/sessions/{id}/launch", ns.withID(ns.launch))
	mux.HandleFunc("POST /v1/sessions/{id}/stop", ns.withID(ns.stop))
	mux.HandleFunc("GET /v1/sessions/{id}/wait", ns.withID(ns.wait))
	mux.HandleFunc("DELETE /v1/sessions/{id}", ns.withID(ns.remove))
	mux.HandleFunc("/v1/sessions/{id}/cdp/", ns.withID(ns.cdp))
	mux.HandleFunc("GET /v1/sessions/{id}/profile", ns.withID(ns.getProfile))
	mux.HandleFunc("PUT /v1/sessions/{id}/profile", ns.withID(ns.putProfile))
	mux.HandleFunc("GET /v1/sessions/{id}/downloads", ns.withID(ns.downloads))
	mux.HandleFunc("DELETE /v1/sessions/{id}/downloads/{name}", ns.withID(ns.deleteDownload))
	mux.HandleFunc("PUT /v1/sessions/{id}/files/{name}", ns.withID(ns.putFile))
	mux.HandleFunc("DELETE /v1/sessions/{id}/files/{name}", ns.withID(ns.deleteFile))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := peerAllowed(r.TLS, ns.cfg.AllowClients)
		if !ok {
			logf("node: refused %s %s from %s (certificate %q is not admitted)", r.Method, r.URL.Path, r.RemoteAddr, who)
			nodeError(w, http.StatusForbidden, fmt.Errorf("certificate %q is not admitted", who))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (ns *nodeServer) withID(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !nodeSessionIDRe.MatchString(id) {
			nodeError(w, http.StatusBadRequest, fmt.Errorf("session id %q", id))
			return
		}
		h(w, r, id)
	}
}

func nodeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func nodeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (ns *nodeServer) sessionDir(id string) string { return filepath.Join(ns.cfg.SessionsDir, id) }

// running is the session's live Chrome, or nil.
func (ns *nodeServer) running(id string) *nodeSession {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	s := ns.sessions[id]
	if s == nil || s.proc == nil || !s.proc.Alive() {
		return nil
	}
	return s
}

func (ns *nodeServer) info(w http.ResponseWriter, r *http.Request) {
	info := nodeInfo{Version: version, OS: hostOS, OSVersion: osVersion(), Arch: goArch(), MaxRunning: ns.cfg.MaxRunning, Running: []string{}}
	if v, err := ns.chromeVersion(); err == nil {
		info.Chrome = v.Full
	} else {
		logf("node: %v", err)
	}
	ns.mu.Lock()
	for id, s := range ns.sessions {
		if s.proc != nil && s.proc.Alive() {
			info.Running = append(info.Running, id)
		}
	}
	ns.mu.Unlock()
	nodeJSON(w, info)
}

// freePort is a DevTools port from the range that no session of this node
// holds and nothing else is listening on.
func (ns *nodeServer) freePort() (int, error) {
	held := map[int]bool{}
	ns.mu.Lock()
	for _, s := range ns.sessions {
		if s.proc != nil && s.proc.Alive() {
			held[s.port] = true
		}
	}
	ns.mu.Unlock()
	for p := ns.cfg.PortLow; p <= ns.cfg.PortHigh; p++ {
		if held[p] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free DevTools port in %d-%d", ns.cfg.PortLow, ns.cfg.PortHigh)
}

func (ns *nodeServer) launch(w http.ResponseWriter, r *http.Request, id string) {
	var req nodeLaunchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		nodeError(w, http.StatusBadRequest, err)
		return
	}
	if !req.Headless && hostOS != "darwin" {
		nodeError(w, http.StatusBadRequest, errors.New("this node runs headless sessions only: it has no display of its own to give one"))
		return
	}
	if req.Width < 200 || req.Height < 200 || req.Width > 4096+64 || req.Height > 4096+128 {
		nodeError(w, http.StatusBadRequest, fmt.Errorf("window %dx%d", req.Width, req.Height))
		return
	}
	for _, f := range req.Flags {
		if !allowedPrefix(f, nodeFlagPrefixes) {
			nodeError(w, http.StatusBadRequest, fmt.Errorf("flag %q is not one a node passes to Chrome", f))
			return
		}
	}
	for _, e := range req.Env {
		if !allowedPrefix(e, nodeEnvPrefixes) {
			nodeError(w, http.StatusBadRequest, fmt.Errorf("environment %q is not one a node passes to Chrome", e))
			return
		}
	}
	if req.Locale != "" && !localeRe.MatchString(req.Locale) {
		nodeError(w, http.StatusBadRequest, fmt.Errorf("locale %q", req.Locale))
		return
	}

	ns.launchMu.Lock()
	defer ns.launchMu.Unlock()
	// A launch for a session whose Chrome is still up is a server that lost
	// track of it (a restart, a network blip): start over from its profile.
	if s := ns.running(id); s != nil {
		logf("node: session %s: relaunching over a running Chrome", id)
		s.proc.stop(5 * time.Second)
	}
	running := 0
	ns.mu.Lock()
	for sid, s := range ns.sessions {
		if sid != id && s.proc != nil && s.proc.Alive() {
			running++
		}
	}
	ns.mu.Unlock()
	if running >= ns.cfg.MaxRunning {
		nodeError(w, http.StatusConflict, fmt.Errorf("this node is running %d sessions, its limit; park one first", running))
		return
	}
	port, err := ns.freePort()
	if err != nil {
		nodeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err := ns.setChromeLocale(req.Locale); err != nil {
		logf("node: session %s: locale %q: %v", id, req.Locale, err)
	}
	dir := ns.sessionDir(id)
	profile := filepath.Join(dir, "profile")
	if err := mergeProfilePrefs(profile, req.Prefs); err != nil {
		logf("node: session %s: preferences: %v", id, err)
	}
	extra := req.Flags
	if !req.Headless {
		// A headful Chrome on a Mac, run by an account with no login session:
		// a real browser window that WindowServer never shows. Chrome renders
		// it all the same (measured: screenshots, the Metal GPU), but it would
		// take an unseen window for an occluded one and stop producing frames
		// and firing timers, which a live view and a page's own scripts need.
		extra = append(append([]string(nil), extra...),
			"--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding", "--disable-background-timer-throttling")
	}
	l := &chromeLaunch{
		Exe:             ns.cfg.Chrome,
		UserDataDir:     profile,
		Downloads:       filepath.Join(dir, "downloads"),
		Headless:        req.Headless,
		Width:           req.Width,
		Height:          req.Height,
		Port:            port,
		DisableFeatures: ns.cfg.DisableFeatures,
		ExtraFlags:      extra,
		Env:             req.Env,
		Verbose:         ns.cfg.Verbose,
		Logf:            logf,
	}
	proc, err := launchChrome(r.Context(), l)
	if err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	os.WriteFile(filepath.Join(dir, "chrome.pid"), []byte(strconv.Itoa(proc.PID())), 0o600)
	ns.mu.Lock()
	ns.sessions[id] = &nodeSession{dir: dir, proc: proc, port: port}
	ns.mu.Unlock()
	go func() {
		<-proc.done
		os.Remove(filepath.Join(dir, "chrome.pid"))
		logf("node: session %s: Chrome exited", id)
	}()
	path := proc.WSURL
	if i := strings.Index(path, "/devtools/"); i >= 0 {
		path = path[i:]
	}
	ver, _ := ns.chromeVersion()
	logf("node: session %s: Chrome %d up on DevTools port %d", id, proc.PID(), port)
	nodeJSON(w, nodeLaunchResponse{BrowserPath: path, Downloads: l.Downloads, Chrome: ver.Full})
}

// setChromeLocale sets the language Chrome on macOS starts in: its
// AppleLanguages preference, for the account the node runs as. Chrome reads
// it once, at startup, and launches are one at a time, so it is in effect
// per session. No locale removes it, and Chrome follows the account's own.
// (Its command-line equivalent, -AppleLanguages "(en-AU)", is taken by
// Chrome's own parser for two URLs to open, which headless refuses.)
func (ns *nodeServer) setChromeLocale(locale string) error {
	if hostOS != "darwin" {
		return nil // Linux takes LANG/LANGUAGE from the environment instead
	}
	ns.bundleOnce.Do(func() { ns.bundleID = chromeBundleID(ns.cfg.Chrome) })
	if ns.bundleID == "" {
		return errors.New("no bundle identifier for " + ns.cfg.Chrome)
	}
	if locale == "" {
		exec.Command("defaults", "delete", ns.bundleID, "AppleLanguages").Run() // absent is fine
		return nil
	}
	if out, err := exec.Command("defaults", "write", ns.bundleID, "AppleLanguages", "-array", locale).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// chromeBundleID reads the bundle identifier from the Info.plist of the app
// the binary is in: com.google.Chrome, or Canary's, or Chromium's.
func chromeBundleID(exe string) string {
	i := strings.Index(exe, ".app/")
	if i < 0 {
		return ""
	}
	out, err := exec.Command("defaults", "read", exe[:i+4]+"/Contents/Info", "CFBundleIdentifier").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (ns *nodeServer) stop(w http.ResponseWriter, r *http.Request, id string) {
	grace := 5 * time.Second
	if ms, err := strconv.Atoi(r.URL.Query().Get("grace_ms")); err == nil && ms >= 0 && ms <= 30000 {
		grace = time.Duration(ms) * time.Millisecond
	}
	if s := ns.running(id); s != nil {
		s.proc.stop(grace)
		logf("node: session %s: stopped", id)
	}
	nodeJSON(w, map[string]bool{"alive": false})
}

// wait blocks until the session's Chrome exits or the timeout passes, and
// says which: the server's watch on a remote Chrome (remote.go) is a loop of
// these, so it learns of a crash as it happens.
func (ns *nodeServer) wait(w http.ResponseWriter, r *http.Request, id string) {
	timeout := 50 * time.Second
	if d, err := time.ParseDuration(r.URL.Query().Get("timeout")); err == nil && d > 0 && d <= 5*time.Minute {
		timeout = d
	}
	s := ns.running(id)
	if s == nil {
		nodeJSON(w, map[string]bool{"alive": false})
		return
	}
	select {
	case <-s.proc.done:
		nodeJSON(w, map[string]bool{"alive": false})
	case <-time.After(timeout):
		nodeJSON(w, map[string]bool{"alive": true})
	case <-r.Context().Done():
	}
}

func (ns *nodeServer) remove(w http.ResponseWriter, r *http.Request, id string) {
	if s := ns.running(id); s != nil {
		s.proc.stop(3 * time.Second)
	}
	ns.mu.Lock()
	delete(ns.sessions, id)
	ns.mu.Unlock()
	if err := os.RemoveAll(ns.sessionDir(id)); err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	logf("node: session %s: deleted", id)
	nodeJSON(w, map[string]bool{"deleted": true})
}

// cdp relays the session's DevTools endpoint: /json/* and the websockets.
// Chrome builds the URLs it advertises (webSocketDebuggerUrl) from the Host
// header, and accepts only an IP address or localhost there; the server's
// relay sends its own loopback address, so what Chrome advertises is the
// relay, and every client on the server connects back through it unchanged.
func (ns *nodeServer) cdp(w http.ResponseWriter, r *http.Request, id string) {
	s := ns.running(id)
	if s == nil {
		nodeError(w, http.StatusNotFound, fmt.Errorf("session %s has no running Chrome here", id))
		return
	}
	target := fmt.Sprintf("127.0.0.1:%d", s.port)
	prefix := "/v1/sessions/" + id + "/cdp"
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target
			if host, _, err := net.SplitHostPort(pr.In.Host); err == nil && (net.ParseIP(host) != nil || host == "localhost") {
				pr.Out.Host = pr.In.Host
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			nodeError(w, http.StatusBadGateway, err)
		},
	}
	proxy.ServeHTTP(w, r)
}

// getProfile streams the session's profile, for the server to save as an
// identity. Only with Chrome stopped: its databases are consistent on disk
// only after a clean exit, which is why the server parks the session first.
func (ns *nodeServer) getProfile(w http.ResponseWriter, r *http.Request, id string) {
	if ns.running(id) != nil {
		nodeError(w, http.StatusConflict, fmt.Errorf("session %s is running; stop it first", id))
		return
	}
	profile := filepath.Join(ns.sessionDir(id), "profile")
	if _, err := os.Stat(profile); err != nil {
		nodeError(w, http.StatusNotFound, fmt.Errorf("session %s has no profile here", id))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	if err := writeProfileArchive(w, profile); err != nil {
		// Headers are gone; the truncated body fails the reader's gzip check.
		logf("node: session %s: sending its profile: %v", id, err)
	}
}

// putProfile replaces the session's profile with the archive sent: an
// identity a session is to start from. Unpacked beside and swapped in, so
// a failed transfer leaves the old profile (or none) rather than half of one.
func (ns *nodeServer) putProfile(w http.ResponseWriter, r *http.Request, id string) {
	if ns.running(id) != nil {
		nodeError(w, http.StatusConflict, fmt.Errorf("session %s is running; stop it first", id))
		return
	}
	dir := ns.sessionDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	incoming := filepath.Join(dir, ".profile-incoming")
	os.RemoveAll(incoming)
	n, err := readProfileArchive(r.Body, incoming)
	if err != nil {
		os.RemoveAll(incoming)
		nodeError(w, http.StatusBadRequest, err)
		return
	}
	profile := filepath.Join(dir, "profile")
	os.RemoveAll(profile)
	if err := os.Rename(incoming, profile); err != nil {
		os.RemoveAll(incoming)
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	logf("node: session %s: profile replaced (%d bytes)", id, n)
	nodeJSON(w, map[string]int64{"bytes": n})
}

func (ns *nodeServer) downloads(w http.ResponseWriter, r *http.Request, id string) {
	files, err := listFileDir(filepath.Join(ns.sessionDir(id), "downloads"), true)
	if err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]nodeFile, 0, len(files))
	for _, f := range files {
		out = append(out, nodeFile{Name: f.Name, Size: f.Size, Modified: f.Modified, Partial: f.Partial, Path: f.path})
	}
	nodeJSON(w, out)
}

func (ns *nodeServer) deleteDownload(w http.ResponseWriter, r *http.Request, id string) {
	name := r.PathValue("name")
	files, err := listFileDir(filepath.Join(ns.sessionDir(id), "downloads"), true)
	if err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	// Resolved through the listing, never joined: a download's name is
	// Chrome's, not one this node would have allowed itself.
	for _, f := range files {
		if f.Name == name {
			if err := os.Remove(f.path); err != nil {
				nodeError(w, http.StatusInternalServerError, err)
				return
			}
			nodeJSON(w, map[string]bool{"deleted": true})
			return
		}
	}
	nodeError(w, http.StatusNotFound, fmt.Errorf("no download %q", name))
}

// putFile stores a file the server has been given for the session, so a
// page's file input can be handed its path. The server keeps the original
// and enforces the session's limits; this is a copy, sent when a page is
// about to be given it.
func (ns *nodeServer) putFile(w http.ResponseWriter, r *http.Request, id string) {
	name := r.PathValue("name")
	if err := checkFileName(name); err != nil {
		nodeError(w, http.StatusBadRequest, err)
		return
	}
	dir := filepath.Join(ns.sessionDir(id), "files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	n, err := copyLimited(tmp, r.Body, maxFileBytes)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	path := filepath.Join(dir, name)
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		nodeError(w, http.StatusBadRequest, err)
		return
	}
	nodeJSON(w, map[string]any{"path": path, "size": n})
}

func (ns *nodeServer) deleteFile(w http.ResponseWriter, r *http.Request, id string) {
	name := r.PathValue("name")
	if err := checkFileName(name); err != nil {
		nodeError(w, http.StatusBadRequest, err)
		return
	}
	err := os.Remove(filepath.Join(ns.sessionDir(id), "files", name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		nodeError(w, http.StatusInternalServerError, err)
		return
	}
	nodeJSON(w, map[string]bool{"deleted": true})
}
