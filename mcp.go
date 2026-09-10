package main

// The MCP surface, on the official Go SDK — the same transport and OAuth
// stack as the sibling dmf.zone servers. Three groups of tools: sessions
// (start / list / resume / stop / delete / live view), identities (the
// persistent logged-in profiles), and the browser itself, plus the pair
// that passes a session through to chrome-devtools-mcp.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpInstructions = `This server gives you Chrome browsers of your own. Each SESSION is one Chrome
instance on its own fresh profile — like the first launch on a clean workstation —
either headless or headful (headful renders on a private display a person can watch
and take over through a live-view link; it also looks like an ordinary browser to sites
that dislike headless ones). Sessions are ephemeral: an idle one is parked (Chrome
closed, profile kept, resumable) and an old one is deleted; session_list shows what
still exists.

IDENTITIES are the persistent part: named snapshots of a profile that the owner has
logged into (Google, etc.). session_start with identity="<name>" begins already
signed in. To create or refresh one: start a HEADFUL session, ask for session_view
and give the owner the link so they can sign in themselves (passkeys, 2FA), then
identity_save. Never try to enter someone's credentials yourself.

Working a page: browser_navigate, then browser_snapshot for the interactive elements
with refs (e12) — click and type by ref, or by text/selector, or by coordinates from a
screenshot. Most actions return a screenshot so you can see the result; pass
screenshot=false when you only need the text. browser_read gives the page's text
cheaply; browser_screenshot with labels=true overlays the refs on the picture.

devtools_tools / devtools_call pass a session through to Google's chrome-devtools-mcp
(network requests, performance traces, emulation, and more) on the same Chrome.

DEVICE PROFILES: by default a session looks like a Windows 11 PC running Chrome to the
sites it visits — user agent, client hints, navigator.platform, screen, GPU — which is
what most sites expect of an ordinary visitor. session_start device="linux" is this
server's own Chrome as it is, for comparing how a site treats the two. timezone and
locale are set per session the same way.`

type mcpApp struct {
	mgr   *manager
	views *viewHandler
}

// runMCP serves until ctx is done, then drains briefly and returns.
func runMCP(ctx context.Context, app *mcpApp, httpAddr string, oauthCfg *oauthConfig, views *viewHandler, verbose bool) error {
	var logger *slog.Logger
	if verbose {
		logger = slog.New(slog.NewTextHandler(logWriter{}, nil))
	}
	app.views = views
	server := newMCPServer(app, logger)
	if httpAddr == "" {
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	handler, err := newHTTPHandler(server, oauthCfg, logger)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// Unauthenticated on purpose: the edge's backend check and the kubelet
	// probe carry no token. It reveals only liveness and counts.
	mux.Handle("/healthz", app)
	// The live view authenticates with its own short-lived token (view.go):
	// a browser following a link cannot present the bearer token.
	mux.Handle("/view/", views)
	mux.Handle("/", handler)
	if oauthCfg != nil {
		logf("Streamable HTTP on http://%s as OAuth resource %s (issuer %s)", httpAddr, oauthCfg.PublicURL, oauthCfg.Issuer)
	} else {
		logf("Streamable HTTP on http://%s (no auth of its own — keep it on localhost)", httpAddr)
	}
	srv := &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ServeHTTP is /healthz.
func (a *mcpApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	running, parked := 0, 0
	for _, s := range a.mgr.list() {
		if s.isRunningQuick() {
			running++
		} else {
			parked++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "version": version,
		"sessions": map[string]int{"running": running, "parked": parked},
		"headful":  a.mgr.cfg.Xvnc != "",
		"devtools": a.mgr.cfg.DevToolsMCP != "",
	})
}

func newMCPServer(app *mcpApp, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "chromemcp",
		Title:   "Chrome browser sessions",
		Version: version,
	}, &mcp.ServerOptions{Instructions: mcpInstructions, Logger: logger})
	app.register(server)
	return server
}

func newHTTPHandler(server *mcp.Server, oauthCfg *oauthConfig, logger *slog.Logger) (http.Handler, error) {
	sopts := &mcp.StreamableHTTPOptions{Stateless: true, Logger: logger}
	if oauthCfg != nil {
		sopts.DisableLocalhostProtection = true
	}
	handler := http.Handler(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, sopts))
	if oauthCfg != nil {
		return newOAuthHandler(context.Background(), oauthCfg, handler)
	}
	return handler, nil
}

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true}
}

func acts(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false)}
}

func destructive(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(true)}
}

func ptr[T any](v T) *T { return &v }

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (r *result) toolResult() *mcp.CallToolResult {
	out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(r.lines, "\n")}}}
	if len(r.image) > 0 {
		out.Content = append(out.Content, &mcp.ImageContent{Data: r.image, MIMEType: r.mime})
	}
	return out
}

// ---- tool inputs ----

type sessionStartIn struct {
	Mode     string `json:"mode,omitempty" jsonschema:"headless (default) or headful. Headful runs on a display the owner can watch and drive via session_view, and is the mode for logging into accounts and for sites that block headless browsers"`
	Identity string `json:"identity,omitempty" jsonschema:"start on a copy of this saved identity's profile, i.e. already logged in as that user (identity_list)"`
	Label    string `json:"label,omitempty" jsonschema:"a short note on what the session is for, shown by session_list"`
	Viewport string `json:"viewport,omitempty" jsonschema:"page size WxH, e.g. 1280x800 (the server default) or 390x844 for a phone-sized page"`
	Device   string `json:"device,omitempty" jsonschema:"device profile the browser presents to sites: windows (the default: a Windows 11 PC running Chrome, with Windows user agent and client hints, navigator.platform Win32, 1920x1080 screen, NVIDIA GPU strings) or linux (this server's own Chrome as it is, no emulation)"`
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA time zone the browser runs in, e.g. Europe/London or Australia/Sydney (default: the server's, UTC in the container)"`
	Locale   string `json:"locale,omitempty" jsonschema:"browser language as a tag, e.g. en-US, en-GB, de-DE: sets Accept-Language, navigator.language and the Intl defaults (default: the server's, en-US)"`
	URL      string `json:"url,omitempty" jsonschema:"open this URL right away"`
}

type sessionIn struct {
	SessionID string `json:"session_id" jsonschema:"the session, from session_start or session_list"`
}

type identitySaveIn struct {
	SessionID string `json:"session_id" jsonschema:"the session whose profile to snapshot; its Chrome is closed cleanly, copied, and started again"`
	Name      string `json:"name" jsonschema:"identity name: lowercase letters, digits, '.', '_', '-'"`
	Note      string `json:"note,omitempty" jsonschema:"what is logged in, e.g. 'google me@example.com'"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace an existing identity of that name"`
}

type identityIn struct {
	Name string `json:"name" jsonschema:"the identity's name"`
}

// tabIn is the common prefix of the browser tools.
type tabIn struct {
	SessionID  string `json:"session_id" jsonschema:"the session"`
	Tab        string `json:"tab,omitempty" jsonschema:"a tab alias from browser_tabs (t1, t2, …); default the current tab"`
	Screenshot *bool  `json:"screenshot,omitempty" jsonschema:"attach a screenshot of the page after the action (default true)"`
}

func (t tabIn) wantShot() bool { return t.Screenshot == nil || *t.Screenshot }

type navigateIn struct {
	tabIn
	URL       string `json:"url" jsonschema:"the URL; https:// is assumed when no scheme is given"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for the load (default 30000); the page is returned as-is on timeout"`
}

type historyIn struct {
	tabIn
	Action string `json:"action" jsonschema:"back, forward or reload"`
}

type snapshotIn struct {
	tabIn
	Max          int  `json:"max,omitempty" jsonschema:"most elements to list (default 250)"`
	ViewportOnly bool `json:"viewport_only,omitempty" jsonschema:"only elements currently on screen"`
	Labels       bool `json:"labels,omitempty" jsonschema:"draw the refs onto the screenshot (implies screenshot)"`
}

type screenshotIn struct {
	tabIn
	targetSpec
	FullPage bool    `json:"full_page,omitempty" jsonschema:"the whole page, not just the viewport"`
	Labels   bool    `json:"labels,omitempty" jsonschema:"overlay the refs of the last browser_snapshot (take one first)"`
	Format   string  `json:"format,omitempty" jsonschema:"jpeg (default) or png"`
	Quality  int     `json:"quality,omitempty" jsonschema:"jpeg quality 1-100 (default 70)"`
	Scale    float64 `json:"scale,omitempty" jsonschema:"downscale factor, 0 < scale <= 1"`
}

type clickIn struct {
	tabIn
	targetSpec
	X         *float64 `json:"x,omitempty" jsonschema:"viewport x in CSS px, with y, instead of a ref/selector/text (e.g. from a screenshot)"`
	Y         *float64 `json:"y,omitempty"`
	Button    string   `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Double    bool     `json:"double,omitempty" jsonschema:"double-click"`
	Modifiers string   `json:"modifiers,omitempty" jsonschema:"held modifiers, e.g. Shift or Control+Shift"`
}

type typeIn struct {
	tabIn
	targetSpec
	Text   string `json:"text" jsonschema:"the text to type"`
	Clear  bool   `json:"clear,omitempty" jsonschema:"clear the field first"`
	Submit bool   `json:"submit,omitempty" jsonschema:"press Enter afterwards"`
}

type pressIn struct {
	tabIn
	Key    string `json:"key" jsonschema:"a key with optional modifiers: Enter, Tab, Escape, ArrowDown, Backspace, F5, 'a', Control+a, Shift+Tab, Meta+Enter"`
	Repeat int    `json:"repeat,omitempty" jsonschema:"press it this many times (default 1)"`
}

type hoverIn struct {
	tabIn
	targetSpec
	X *float64 `json:"x,omitempty" jsonschema:"viewport coordinates instead of an element"`
	Y *float64 `json:"y,omitempty"`
}

type scrollIn struct {
	tabIn
	targetSpec
	Direction string `json:"direction,omitempty" jsonschema:"down (default), up, left or right"`
	Amount    int    `json:"amount,omitempty" jsonschema:"pixels (default: most of the viewport)"`
}

type selectIn struct {
	tabIn
	targetSpec
	Value string `json:"value" jsonschema:"the option to choose, by value or by visible text"`
}

type waitIn struct {
	tabIn
	Ms          int    `json:"ms,omitempty" jsonschema:"just wait this long"`
	Selector    string `json:"selector,omitempty" jsonschema:"wait until an element matching this is visible"`
	Text        string `json:"text,omitempty" jsonschema:"wait until this text appears on the page"`
	URLContains string `json:"url_contains,omitempty" jsonschema:"wait until the URL contains this"`
	Gone        bool   `json:"gone,omitempty" jsonschema:"invert: wait until the selector/text is gone"`
	TimeoutMs   int    `json:"timeout_ms,omitempty" jsonschema:"give up after this long (default 10000)"`
}

type readIn struct {
	tabIn
	Selector string `json:"selector,omitempty" jsonschema:"read only this element's text (default: the whole page)"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"truncate after this many characters (default 20000)"`
}

type evaluateIn struct {
	tabIn
	Expression string `json:"expression" jsonschema:"JavaScript evaluated in the page; a promise is awaited; the value is returned as JSON"`
	TimeoutMs  int    `json:"timeout_ms,omitempty" jsonschema:"default 15000"`
}

type consoleIn struct {
	SessionID string `json:"session_id"`
	Tab       string `json:"tab,omitempty"`
	Clear     bool   `json:"clear,omitempty" jsonschema:"forget the entries after returning them"`
	Levels    string `json:"levels,omitempty" jsonschema:"comma-separated levels to keep, e.g. error,warning (default all)"`
}

type tabNewIn struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url,omitempty" jsonschema:"open this URL in the new tab (default about:blank)"`
}

type tabRefIn struct {
	SessionID string `json:"session_id"`
	Tab       string `json:"tab" jsonschema:"the tab alias, e.g. t2"`
}

type devtoolsCallIn struct {
	SessionID string         `json:"session_id"`
	Tool      string         `json:"tool" jsonschema:"the chrome-devtools-mcp tool name, from devtools_tools"`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"its arguments, per the tool's input schema"`
}

// sessionStartSchema is the reflected schema for sessionStartIn with the
// closed-set arguments -- mode and device -- advertised as enums with their
// defaults, so a client sees the valid values and what it gets without
// asking in the schema itself and not only in the prose. The device list
// comes from the registered profiles, so a new profile is advertised the
// moment it exists. The SDK also validates calls against this and fills the
// defaults in, so the handler sees the same resolution a reader of the
// schema expects.
func sessionStartSchema() *jsonschema.Schema {
	s, err := jsonschema.For[sessionStartIn](nil)
	if err != nil {
		panic(err)
	}
	s.Properties["mode"].Enum = []any{modeHeadless, modeHeadful}
	s.Properties["mode"].Default = json.RawMessage(strconv.Quote(modeHeadless))
	names := deviceNames()
	devs := make([]any, len(names))
	for i, n := range names {
		devs[i] = n
	}
	s.Properties["device"].Enum = devs
	s.Properties["device"].Default = json.RawMessage(strconv.Quote(defaultDevice))
	return s
}

func (a *mcpApp) register(s *mcp.Server) {
	// ---- sessions ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_start",
		InputSchema: sessionStartSchema(),
		Description: "Start a new Chrome: a fresh profile (nothing logged in, no history), or a copy of a saved identity's profile " +
			"(already logged in as that user). Returns the session_id every other tool needs. Headless by default; " +
			"mode=headful for a browser a person can watch and drive (session_view), or for sites that block headless Chrome. " +
			"Presents itself to sites as a Windows 11 PC running Chrome unless device=linux asks for this server's own Chrome as it is; " +
			"timezone and locale set where and in what language it runs.",
		Annotations: acts("Start a browser session"),
	}, a.sessionStart)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_list",
		Description: "The sessions that still exist: running ones and parked ones (Chrome closed, profile kept) that can be resumed. Also the saved identities.",
		Annotations: readOnly("List sessions"),
	}, a.sessionList)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_resume",
		Description: "Start Chrome again for a parked session, with its cookies, logins and tabs. Any browser tool does this implicitly; this one just does it explicitly.",
		Annotations: acts("Resume a session"),
	}, a.sessionResume)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_stop",
		Description: "Park a session: close its Chrome, keep its profile so it can be resumed later. Frees memory; happens by itself after idling.",
		Annotations: acts("Park a session"),
	}, a.sessionStop)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_delete",
		Description: "Close a session's Chrome and delete its profile. Nothing of it remains; saved identities are unaffected.",
		Annotations: destructive("Delete a session"),
	}, a.sessionDelete)
	mcp.AddTool(s, &mcp.Tool{
		Name: "session_view",
		Description: "A live-view link for a HEADFUL session: a web page showing that Chrome, with mouse and keyboard, for a person to open. " +
			"Give it to the owner when they need to log in to an account (then identity_save), solve a captcha, or watch. " +
			"The link expires; ask again for a new one.",
		Annotations: acts("Live-view link"),
	}, a.sessionView)

	// ---- identities ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "identity_list",
		Description: "The saved identities: profiles logged in as particular users, usable with session_start identity=<name>.",
		Annotations: readOnly("List identities"),
	}, a.identityList)
	mcp.AddTool(s, &mcp.Tool{
		Name: "identity_save",
		Description: "Snapshot a session's profile as a named identity, so future sessions can start logged in as it. " +
			"Do this after the owner has signed in through session_view (or to refresh an identity's cookies after using it). " +
			"The session's Chrome restarts around the copy; its tabs come back.",
		Annotations: acts("Save an identity"),
	}, a.identitySave)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "identity_delete",
		Description: "Delete a saved identity. Sessions already started from it are unaffected.",
		Annotations: destructive("Delete an identity"),
	}, a.identityDelete)

	// ---- browser ----
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_navigate",
		Description: "Go to a URL in the current (or given) tab and wait for it to load. Returns where you ended up and a screenshot.",
		Annotations: acts("Navigate"),
	}, a.navigate)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_history",
		Description: "Go back, go forward, or reload.",
		Annotations: acts("Back / forward / reload"),
	}, a.history)
	mcp.AddTool(s, &mcp.Tool{
		Name: "browser_snapshot",
		Description: "List the page's interactive elements (links, buttons, fields, headings…) with a ref each (e12), role, label, " +
			"value and position. Refs are what browser_click / browser_type / browser_select take; they are renumbered on every " +
			"snapshot and cleared by navigation, so re-snapshot after the page changes. Text only unless screenshot/labels is set.",
		Annotations: readOnly("Snapshot the page"),
	}, a.snapshot)
	mcp.AddTool(s, &mcp.Tool{
		Name: "browser_screenshot",
		Description: "See the page: the viewport (default), the whole page, or one element. labels=true overlays the refs from the " +
			"last browser_snapshot so you can match what you see to what you can click.",
		Annotations: readOnly("Screenshot"),
	}, a.screenshot)
	mcp.AddTool(s, &mcp.Tool{
		Name: "browser_click",
		Description: "Click an element — by ref (from browser_snapshot), by visible text, by CSS selector — or at viewport coordinates. " +
			"A real mouse click at the element's centre after scrolling it into view.",
		Annotations: acts("Click"),
	}, a.click)
	mcp.AddTool(s, &mcp.Tool{
		Name: "browser_type",
		Description: "Type text into a field: clicks it (ref / text / selector), optionally clears it, types, and optionally presses Enter. " +
			"Without a target, types into whatever is focused.",
		Annotations: acts("Type"),
	}, a.typeText)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_press",
		Description: "Press a key, with modifiers: Enter, Tab, Escape, ArrowDown, Backspace, Control+a, Shift+Tab, F5…",
		Annotations: acts("Press a key"),
	}, a.press)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_hover",
		Description: "Move the mouse over an element (or a point), for menus and tooltips that open on hover.",
		Annotations: acts("Hover"),
	}, a.hover)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_scroll",
		Description: "Scroll the page (or the element under the target) with the mouse wheel; or scroll an element into view by giving it as the target with no direction.",
		Annotations: acts("Scroll"),
	}, a.scroll)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_select",
		Description: "Choose an option in a <select> drop-down, by value or visible text. Custom drop-downs are clicked like anything else.",
		Annotations: acts("Select an option"),
	}, a.selectOption)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_wait",
		Description: "Wait for something: a number of milliseconds, an element to appear (or disappear), text to appear, or the URL to change.",
		Annotations: readOnly("Wait"),
	}, a.wait)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_read",
		Description: "The page's rendered text (or one element's), as a person would read it. Cheap; no screenshot.",
		Annotations: readOnly("Read the page text"),
	}, a.read)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_evaluate",
		Description: "Run JavaScript in the page and get the result back as JSON. For reading data out of the page or doing what the other tools cannot.",
		Annotations: acts("Run JavaScript"),
	}, a.evaluate)
	mcp.AddTool(s, &mcp.Tool{
		Name: "browser_fingerprint",
		Description: "What the page's scripts see of the device: user agent and client hints, navigator.platform, languages, time zone, " +
			"screen and window, GPU strings, which fonts are installed, and the same from a worker. For checking what a device profile, " +
			"timezone or locale presents to a site.",
		Annotations: readOnly("Device fingerprint"),
	}, a.fingerprint)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_console",
		Description: "Console messages and uncaught exceptions the tab has logged since the session started (or since the last clear).",
		Annotations: readOnly("Console log"),
	}, a.console)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_tabs",
		Description: "The session's open tabs with their aliases (t1, t2…), URLs and titles; the current one marked.",
		Annotations: readOnly("List tabs"),
	}, a.tabs)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_tab_new",
		Description: "Open a new tab (optionally at a URL) and make it current.",
		Annotations: acts("New tab"),
	}, a.tabNew)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_tab_select",
		Description: "Make a tab the current one (and bring it to the front).",
		Annotations: acts("Switch tab"),
	}, a.tabSelect)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browser_tab_close",
		Description: "Close a tab.",
		Annotations: acts("Close tab"),
	}, a.tabClose)

	// ---- devtools passthrough ----
	if a.mgr.cfg.DevToolsMCP != "" {
		mcp.AddTool(s, &mcp.Tool{
			Name: "devtools_tools",
			Description: "The tools of Google's chrome-devtools-mcp, attached to this session's Chrome: network requests, performance " +
				"traces, emulation (network/CPU throttling, geolocation…), and its own page tools. Read their schemas here, then devtools_call.",
			Annotations: readOnly("DevTools MCP tools"),
		}, a.devtoolsTools)
		mcp.AddTool(s, &mcp.Tool{
			Name: "devtools_call",
			Description: "Call one chrome-devtools-mcp tool on this session's Chrome, with its arguments. Page-scoped tools take a pageId " +
				"from its list_pages; leave it out and the call goes to this session's current tab. The result is passed through as-is.",
			Annotations: acts("DevTools MCP call"),
		}, a.devtoolsCall)
	}
}

// ---- session tools ----

func (a *mcpApp) sessionStart(ctx context.Context, req *mcp.CallToolRequest, in sessionStartIn) (*mcp.CallToolResult, any, error) {
	o := startOptions{Mode: in.Mode, Identity: in.Identity, Label: in.Label, Device: in.Device, Timezone: in.Timezone, Locale: in.Locale}
	if in.Viewport != "" {
		w, h, err := parseViewport(in.Viewport)
		if err != nil {
			return nil, nil, err
		}
		o.Width, o.Height = w, h
	}
	s, err := a.mgr.start(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	r := &result{}
	r.addf("started session %s (%s, %dx%d%s%s)", s.meta.ID, s.meta.Mode, s.meta.Width, s.meta.Height, identitySuffix(s.meta.Identity), s.meta.deviceSuffix())
	if s.meta.Device != "" {
		if d, err := lookupDevice(s.meta.Device); err == nil {
			r.addf("sites see %s.", d.Description)
		}
	}
	if s.meta.Mode == modeHeadful {
		r.addf("session_view gives a person a live view of this browser.")
	}
	if in.URL != "" {
		u, err := normalizeURL(in.URL)
		if err != nil {
			return nil, nil, err
		}
		err = a.mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, t *tab) error {
			if err := navigate(ctx, t, u, 30*time.Second); err != nil {
				return err
			}
			return a.finish(ctx, s, t, r, true, screenshotOpts{})
		})
		if err != nil {
			r.addf("opening %s: %v", u, err)
		}
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) sessionList(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	var sb strings.Builder
	sessions := a.mgr.list()
	if len(sessions) == 0 {
		sb.WriteString("no sessions; session_start makes one.\n")
	} else {
		fmt.Fprintf(&sb, "%d sessions (most recently used first):\n", len(sessions))
		for _, s := range sessions {
			u, title, state, tabs := s.meta.LastURL, s.meta.LastTitle, s.state(), 0
			if s.mu.TryLock() {
				u, title = s.pageInfo()
				tabs = len(s.tabs)
				s.mu.Unlock()
			} else {
				state += " (busy)"
			}
			fmt.Fprintf(&sb, "- %s  %s, %s, %dx%d", s.meta.ID, state, s.meta.Mode, s.meta.Width, s.meta.Height)
			if s.meta.Identity != "" {
				fmt.Fprintf(&sb, ", identity %s", s.meta.Identity)
			}
			sb.WriteString(s.meta.deviceSuffix())
			if s.meta.Label != "" {
				fmt.Fprintf(&sb, ", %q", s.meta.Label)
			}
			fmt.Fprintf(&sb, "; created %s, last used %s ago", s.meta.Created.Format("Jan 2 15:04"), time.Since(time.Unix(0, s.lastUsed.Load())).Round(time.Second))
			if tabs > 0 {
				fmt.Fprintf(&sb, ", %d tabs", tabs)
			}
			if u != "" {
				fmt.Fprintf(&sb, "\n    at %s — %q", u, title)
			}
			sb.WriteString("\n")
		}
	}
	ids, _ := a.mgr.identities.list()
	if len(ids) > 0 {
		fmt.Fprintf(&sb, "\n%d saved identities: ", len(ids))
		names := make([]string, len(ids))
		for i, m := range ids {
			names[i] = m.Name
		}
		sb.WriteString(strings.Join(names, ", "))
		sb.WriteString("\n")
	}
	cfg := a.mgr.cfg
	fmt.Fprintf(&sb, "\nidle sessions are parked after %s and deleted after %s unused; headful: %v; device profiles: %s (default %s)",
		cfg.IdlePark, cfg.MaxAge, cfg.Xvnc != "", strings.Join(deviceNames(), ", "), defaultDevice)
	return text(sb.String()), nil, nil
}

func (a *mcpApp) sessionResume(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, "", func(ctx context.Context, s *session, t *tab) error {
		r.addf("session %s is running (%d tabs)", s.meta.ID, len(s.tabs))
		return a.finish(ctx, s, t, r, true, screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) sessionStop(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mgr.get(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	was := s.isRunning()
	s.park()
	s.mu.Unlock()
	a.mgr.views.revokeSession(s.meta.ID)
	if !was {
		return text(fmt.Sprintf("session %s was already parked", s.meta.ID)), nil, nil
	}
	return text(fmt.Sprintf("session %s parked; its profile is kept and session_resume (or any browser tool) brings it back", s.meta.ID)), nil, nil
}

func (a *mcpApp) sessionDelete(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	if err := a.mgr.remove(in.SessionID); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("session %s deleted", in.SessionID)), nil, nil
}

func (a *mcpApp) sessionView(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	if a.views == nil || !a.views.available() {
		return nil, nil, fmt.Errorf("live views are unavailable on this server (needs Xvnc and noVNC)")
	}
	if a.mgr.cfg.ViewBase == "" {
		return nil, nil, fmt.Errorf("live views need the HTTP transport (serve -http); there is no URL to give out over stdio")
	}
	s, err := a.mgr.running(ctx, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if s.meta.Mode != modeHeadful {
		return nil, nil, fmt.Errorf("session %s is headless; only headful sessions have a live view (start one with mode=headful)", s.meta.ID)
	}
	s.touch()
	tok, exp := a.mgr.views.issue(s.meta.ID)
	u := fmt.Sprintf("%s/view/%s/vnc.html?autoconnect=1&resize=scale&path=ws", a.mgr.cfg.ViewBase, tok)
	return text(fmt.Sprintf("Live view of session %s (valid until %s, %s from now):\n%s\n\n"+
		"Anyone with the link can see and control this browser until then. Whatever they log in to stays in the session; "+
		"identity_save keeps it for future sessions.",
		s.meta.ID, exp.Format(time.RFC3339), time.Until(exp).Round(time.Minute), u)), nil, nil
}

// ---- identity tools ----

func (a *mcpApp) identityList(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	ids, err := a.mgr.identities.list()
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return text("no saved identities. To make one: session_start mode=headful, session_view for the owner to sign in, then identity_save."), nil, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d saved identities:\n", len(ids))
	for _, m := range ids {
		fmt.Fprintf(&sb, "- %s  saved %s (%.1f MB)", m.Name, m.Updated.Format("2006-01-02 15:04"), float64(m.Bytes)/1e6)
		if m.Note != "" {
			fmt.Fprintf(&sb, "  %s", m.Note)
		}
		sb.WriteString("\n")
	}
	return text(sb.String()), nil, nil
}

func (a *mcpApp) identitySave(ctx context.Context, req *mcp.CallToolRequest, in identitySaveIn) (*mcp.CallToolResult, any, error) {
	if err := checkIdentityName(in.Name); err != nil {
		return nil, nil, err
	}
	s, err := a.mgr.get(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
	wasRunning := s.isRunning()
	// Chrome must be closed for its databases to be consistent on disk.
	s.park()
	m, err := a.mgr.identities.save(in.Name, in.Note, s.meta.ID, s.dir, in.Overwrite)
	var relaunch error
	if wasRunning {
		relaunch = s.launch(ctx)
	}
	if err != nil {
		return nil, nil, err
	}
	msg := fmt.Sprintf("identity %q saved from session %s (%.1f MB); session_start identity=%q starts logged in as it", m.Name, s.meta.ID, float64(m.Bytes)/1e6, m.Name)
	if relaunch != nil {
		msg += fmt.Sprintf("\nthe session did not restart: %v (session_resume to retry)", relaunch)
	}
	return text(msg), nil, nil
}

func (a *mcpApp) identityDelete(ctx context.Context, req *mcp.CallToolRequest, in identityIn) (*mcp.CallToolResult, any, error) {
	if err := a.mgr.identities.remove(in.Name); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("identity %q deleted", in.Name)), nil, nil
}

// ---- browser tools ----

// finish appends the location header, side notes and (if wanted) a
// screenshot to a result.
func (a *mcpApp) finish(ctx context.Context, s *session, t *tab, r *result, shot bool, o screenshotOpts) error {
	r.lines = append([]string{s.header(ctx, t)}, r.lines...)
	s.footer(r, t)
	if !shot {
		return nil
	}
	img, mime, err := screenshot(ctx, t, o)
	if err != nil {
		r.addf("(no screenshot: %v)", err)
		return nil
	}
	r.image, r.mime = img, mime
	return nil
}

func (a *mcpApp) navigate(ctx context.Context, req *mcp.CallToolRequest, in navigateIn) (*mcp.CallToolResult, any, error) {
	u, err := normalizeURL(in.URL)
	if err != nil {
		return nil, nil, err
	}
	timeout := 30 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	r := &result{}
	err = a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		if err := navigate(ctx, t, u, timeout); err != nil {
			return err
		}
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) history(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, any, error) {
	var action chromedp.Action
	switch strings.ToLower(in.Action) {
	case "back":
		action = chromedp.NavigateBack()
	case "forward":
		action = chromedp.NavigateForward()
	case "reload":
		action = chromedp.Reload()
	default:
		return nil, nil, fmt.Errorf("action %q: want back, forward or reload", in.Action)
	}
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		nctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
		err := chromedp.Run(nctx, action)
		cancel()
		if err != nil && nctx.Err() == nil {
			return fmt.Errorf("%s: %w", in.Action, err)
		}
		settle(ctx, t, 3*time.Second)
		r.addf("%s done", in.Action)
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) snapshot(ctx context.Context, req *mcp.CallToolRequest, in snapshotIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		snap, err := snapshot(ctx, t, in.Max, in.ViewportOnly)
		if err != nil {
			return err
		}
		r.lines = append(r.lines, snap.render(!in.ViewportOnly))
		shot := in.Labels || (in.Screenshot != nil && *in.Screenshot)
		return a.finish(ctx, s, t, r, shot, screenshotOpts{Labels: in.Labels})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) screenshot(ctx context.Context, req *mcp.CallToolRequest, in screenshotIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		o := screenshotOpts{FullPage: in.FullPage, Format: in.Format, Quality: in.Quality, Scale: in.Scale, Labels: in.Labels}
		if !in.targetSpec.empty() {
			res, err := resolveTarget(ctx, t, in.targetSpec)
			if err != nil {
				return err
			}
			// The element sits in the viewport after resolve scrolled it there.
			box := res.Box
			pad := 8
			box[0] -= pad
			box[1] -= pad
			box[2] += 2 * pad
			box[3] += 2 * pad
			o.Clip = &box
			o.FullPage = false
			r.addf("element %s", res.describe())
		}
		return a.finish(ctx, s, t, r, true, o)
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func parseModifiers(spec string) (input.Modifier, error) {
	var mods input.Modifier
	if strings.TrimSpace(spec) == "" {
		return 0, nil
	}
	_, m, err := parseKey(spec + "+a")
	if err != nil {
		return 0, err
	}
	mods = m &^ input.ModifierShift
	if strings.Contains(strings.ToLower(spec), "shift") {
		mods |= input.ModifierShift
	}
	return mods, nil
}

func (a *mcpApp) click(ctx context.Context, req *mcp.CallToolRequest, in clickIn) (*mcp.CallToolResult, any, error) {
	mods, err := parseModifiers(in.Modifiers)
	if err != nil {
		return nil, nil, err
	}
	count := 1
	if in.Double {
		count = 2
	}
	r := &result{}
	err = a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		var x, y float64
		var what string
		if in.X != nil && in.Y != nil {
			x, y, what = *in.X, *in.Y, fmt.Sprintf("(%g, %g)", x, y)
		} else {
			res, err := resolveTarget(ctx, t, in.targetSpec)
			if err != nil {
				return err
			}
			x, y, what = res.X, res.Y, res.describe()
		}
		before := len(s.tabs)
		if err := clickAt(ctx, t, x, y, in.Button, count, mods); err != nil {
			return fmt.Errorf("click: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		settle(ctx, t, 5*time.Second)
		verb := "clicked"
		if in.Double {
			verb = "double-clicked"
		}
		r.addf("%s %s at (%.0f, %.0f)", verb, what, x, y)
		if err := s.syncTabs(ctx); err == nil && len(s.tabs) > before {
			r.addf("a new tab opened (browser_tabs lists it; browser_tab_select to switch)")
		}
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) typeText(ctx context.Context, req *mcp.CallToolRequest, in typeIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		if !in.targetSpec.empty() {
			res, err := resolveTarget(ctx, t, in.targetSpec)
			if err != nil {
				return err
			}
			if err := clickAt(ctx, t, res.X, res.Y, "left", 1, 0); err != nil {
				return fmt.Errorf("focusing %s: %w", res.describe(), err)
			}
			r.addf("into %s", res.describe())
			if in.Clear {
				var ok bool
				if err := evalJSON(ctx, t, jsCall("clearField", in.targetSpec), &ok); err == nil && !ok {
					// Not a field we know how to empty: select-all and delete.
					if k, m, err := parseKey("Control+a"); err == nil {
						pressKey(ctx, t, k, m)
					}
					if k, m, err := parseKey("Delete"); err == nil {
						pressKey(ctx, t, k, m)
					}
				}
			}
		} else if in.Clear {
			if k, m, err := parseKey("Control+a"); err == nil {
				pressKey(ctx, t, k, m)
			}
			if k, m, err := parseKey("Delete"); err == nil {
				pressKey(ctx, t, k, m)
			}
		}
		if in.Text != "" {
			if err := insertText(ctx, t, in.Text); err != nil {
				return fmt.Errorf("typing: %w", err)
			}
		}
		r.addf("typed %q", truncate(in.Text, 200))
		if in.Submit {
			k, m, _ := parseKey("Enter")
			if err := pressKey(ctx, t, k, m); err != nil {
				return fmt.Errorf("pressing Enter: %w", err)
			}
			r.addf("pressed Enter")
			time.Sleep(300 * time.Millisecond)
			settle(ctx, t, 5*time.Second)
		}
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) press(ctx context.Context, req *mcp.CallToolRequest, in pressIn) (*mcp.CallToolResult, any, error) {
	k, mods, err := parseKey(in.Key)
	if err != nil {
		return nil, nil, err
	}
	n := in.Repeat
	if n <= 0 {
		n = 1
	}
	if n > 100 {
		n = 100
	}
	r := &result{}
	err = a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		for i := 0; i < n; i++ {
			if err := pressKey(ctx, t, k, mods); err != nil {
				return fmt.Errorf("pressing %s: %w", in.Key, err)
			}
		}
		time.Sleep(200 * time.Millisecond)
		settle(ctx, t, 5*time.Second)
		if n > 1 {
			r.addf("pressed %s ×%d", in.Key, n)
		} else {
			r.addf("pressed %s", in.Key)
		}
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) hover(ctx context.Context, req *mcp.CallToolRequest, in hoverIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		var x, y float64
		var what string
		if in.X != nil && in.Y != nil {
			x, y, what = *in.X, *in.Y, fmt.Sprintf("(%g, %g)", x, y)
		} else {
			res, err := resolveTarget(ctx, t, in.targetSpec)
			if err != nil {
				return err
			}
			x, y, what = res.X, res.Y, res.describe()
		}
		if err := hoverAt(ctx, t, x, y); err != nil {
			return fmt.Errorf("hover: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		r.addf("hovering %s at (%.0f, %.0f)", what, x, y)
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) scroll(ctx context.Context, req *mcp.CallToolRequest, in scrollIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		w, h, err := viewportSize(ctx, t)
		if err != nil {
			return err
		}
		x, y := float64(w)/2, float64(h)/2
		if !in.targetSpec.empty() {
			res, err := resolveTarget(ctx, t, in.targetSpec)
			if err != nil {
				return err
			}
			x, y = res.X, res.Y
			if in.Direction == "" {
				r.addf("scrolled %s into view", res.describe())
				time.Sleep(200 * time.Millisecond)
				return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
			}
		}
		amount := float64(in.Amount)
		dir := strings.ToLower(in.Direction)
		if dir == "" {
			dir = "down"
		}
		var dx, dy float64
		switch dir {
		case "down":
			if amount == 0 {
				amount = float64(h) * 0.8
			}
			dy = amount
		case "up":
			if amount == 0 {
				amount = float64(h) * 0.8
			}
			dy = -amount
		case "right":
			if amount == 0 {
				amount = float64(w) * 0.8
			}
			dx = amount
		case "left":
			if amount == 0 {
				amount = float64(w) * 0.8
			}
			dx = -amount
		default:
			return fmt.Errorf("direction %q: want up, down, left or right", in.Direction)
		}
		if err := scrollBy(ctx, t, x, y, dx, dy); err != nil {
			return fmt.Errorf("scroll: %w", err)
		}
		time.Sleep(400 * time.Millisecond) // smooth scrolling and lazy loads
		var pos [2]int
		evalJSON(ctx, t, "[Math.round(scrollX), Math.round(scrollY)]", &pos)
		r.addf("scrolled %s by %.0f px; page now at (%d, %d)", dir, amount, pos[0], pos[1])
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) selectOption(ctx context.Context, req *mcp.CallToolRequest, in selectIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		if in.targetSpec.empty() {
			return fmt.Errorf("give the <select> as ref, selector or text")
		}
		var res struct {
			Error    string `json:"error"`
			Selected string `json:"selected"`
			Value    string `json:"value"`
		}
		if err := evalJSON(ctx, t, jsCall("selectOption", in.targetSpec, in.Value), &res); err != nil {
			return fmt.Errorf("select: %w", err)
		}
		if res.Error != "" {
			return fmt.Errorf("%s", res.Error)
		}
		time.Sleep(200 * time.Millisecond)
		settle(ctx, t, 3*time.Second)
		r.addf("selected %q (value %q) in %s", res.Selected, res.Value, in.targetSpec)
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) wait(ctx context.Context, req *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, any, error) {
	timeout := 10 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	if timeout > 2*time.Minute {
		timeout = 2 * time.Minute
	}
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		if in.Ms > 0 {
			d := time.Duration(in.Ms) * time.Millisecond
			if d > 2*time.Minute {
				d = 2 * time.Minute
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
			r.addf("waited %s", d)
		}
		if in.Selector != "" || in.Text != "" || in.URLContains != "" {
			msg, err := waitFor(ctx, t, waitSpec{Selector: in.Selector, Text: in.Text, URLContains: in.URLContains, Gone: in.Gone}, timeout)
			if err != nil {
				r.addf("wait: %v", err)
			} else {
				r.addf("%s", msg)
			}
		} else if in.Ms == 0 {
			return fmt.Errorf("give ms, or one of selector, text, url_contains")
		}
		return a.finish(ctx, s, t, r, in.wantShot(), screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) read(ctx context.Context, req *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, any, error) {
	max := in.MaxChars
	if max <= 0 {
		max = 20000
	}
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		tr, err := readText(ctx, t, in.Selector, max)
		if err != nil {
			return err
		}
		r.addf("%s — %q (%d chars%s)\n", tr.URL, tr.Title, tr.Total, map[bool]string{true: ", truncated", false: ""}[tr.Truncated])
		r.lines = append(r.lines, tr.Text)
		s.footer(r, t)
		if in.Screenshot != nil && *in.Screenshot {
			if img, mime, err := screenshot(ctx, t, screenshotOpts{}); err == nil {
				r.image, r.mime = img, mime
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) evaluate(ctx context.Context, req *mcp.CallToolRequest, in evaluateIn) (*mcp.CallToolResult, any, error) {
	timeout := 15 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		out, err := evaluate(ctx, t, in.Expression, timeout)
		if err != nil {
			return err
		}
		r.lines = append(r.lines, truncate(out, 50000))
		s.footer(r, t)
		if in.Screenshot != nil && *in.Screenshot {
			if img, mime, err := screenshot(ctx, t, screenshotOpts{}); err == nil {
				r.image, r.mime = img, mime
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) fingerprint(ctx context.Context, req *mcp.CallToolRequest, in tabIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		out, err := evaluate(ctx, t, fingerprintScript, 20*time.Second)
		if err != nil {
			return err
		}
		r.addf("session %s%s", s.meta.ID, s.meta.deviceSuffix())
		r.lines = append(r.lines, out)
		if strings.Contains(out, `"userAgentData": null`) {
			r.addf("note: navigator.userAgentData (the client hints) is only exposed on https pages; navigate to one to see it")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) console(ctx context.Context, req *mcp.CallToolRequest, in consoleIn) (*mcp.CallToolResult, any, error) {
	var out string
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		out = t.consoleText(in.Clear, in.Levels)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

func (a *mcpApp) tabs(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mgr.running(ctx, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
	if err := s.syncTabs(ctx); err != nil {
		return nil, nil, err
	}
	out, err := s.tabList(ctx)
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

func (a *mcpApp) tabNew(ctx context.Context, req *mcp.CallToolRequest, in tabNewIn) (*mcp.CallToolResult, any, error) {
	u := "about:blank"
	if in.URL != "" {
		var err error
		if u, err = normalizeURL(in.URL); err != nil {
			return nil, nil, err
		}
	}
	s, err := a.mgr.running(ctx, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
	if err := s.syncTabs(ctx); err != nil {
		return nil, nil, err
	}
	t, err := s.newTab(ctx, "about:blank")
	if err != nil {
		return nil, nil, err
	}
	s.focus(ctx, t)
	r := &result{}
	r.addf("opened tab %s", t.alias)
	if u != "about:blank" {
		if err := navigate(ctx, t, u, 30*time.Second); err != nil {
			r.addf("%v", err)
		}
	}
	if err := a.finish(ctx, s, t, r, true, screenshotOpts{}); err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) tabSelect(ctx context.Context, req *mcp.CallToolRequest, in tabRefIn) (*mcp.CallToolResult, any, error) {
	r := &result{}
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		if err := s.bringToFront(ctx, t); err != nil {
			return err
		}
		r.addf("tab %s is current", t.alias)
		return a.finish(ctx, s, t, r, true, screenshotOpts{})
	})
	if err != nil {
		return nil, nil, err
	}
	return r.toolResult(), nil, nil
}

func (a *mcpApp) tabClose(ctx context.Context, req *mcp.CallToolRequest, in tabRefIn) (*mcp.CallToolResult, any, error) {
	var out string
	err := a.mgr.withTab(ctx, in.SessionID, in.Tab, func(ctx context.Context, s *session, t *tab) error {
		alias := t.alias
		if err := s.closeTab(ctx, t); err != nil {
			return err
		}
		out = fmt.Sprintf("closed tab %s; %d tabs remain", alias, len(s.tabs))
		if cur, ok := s.tabs[s.current]; ok {
			out += fmt.Sprintf(", current is %s", cur.alias)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

// ---- devtools passthrough ----

// devtoolsFor returns the session's passthrough client (started on first
// use) and the URL of its current tab, for routing page-scoped calls.
func (a *mcpApp) devtoolsFor(ctx context.Context, sessionID string) (*devtoolsClient, string, error) {
	s, err := a.mgr.running(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
	if s.devtools == nil {
		s.devtools = newDevToolsClient(a.mgr.cfg.DevToolsMCP, s.chrome.Port, a.mgr.cfg.Verbose)
	}
	s.syncTabs(ctx)
	u, _ := s.pageInfo()
	return s.devtools, u, nil
}

func (a *mcpApp) devtoolsTools(ctx context.Context, req *mcp.CallToolRequest, in sessionIn) (*mcp.CallToolResult, any, error) {
	dc, _, err := a.devtoolsFor(ctx, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	tools, err := dc.listTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	return text(describeTools(tools)), nil, nil
}

func (a *mcpApp) devtoolsCall(ctx context.Context, req *mcp.CallToolRequest, in devtoolsCallIn) (*mcp.CallToolResult, any, error) {
	if in.Tool == "" {
		return nil, nil, fmt.Errorf("tool is required (devtools_tools lists them)")
	}
	dc, current, err := a.devtoolsFor(ctx, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	res, err := dc.call(ctx, in.Tool, in.Arguments, current)
	if err != nil {
		return nil, nil, err
	}
	// Pass the content through unchanged; a tool error stays a tool error.
	return &mcp.CallToolResult{Content: res.Content, IsError: res.IsError}, nil, nil
}
