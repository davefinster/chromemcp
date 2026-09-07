// chromemcp gives an agent a Chrome of its own: an MCP server that launches
// Chrome instances (headless, or headful on a private VNC display), drives
// them, and hands back what the page looks like after every action. It is
// OAuth-protected the way the other dmf.zone MCP servers are, and can pass
// each session through to Google's Chrome DevTools MCP for the low-level
// work (network, performance, emulation) this server does not reimplement.
//
//	chromemcp serve -http :8787 \                    public deployment
//	  -oauth-issuer https://<env>.authkit.app \
//	  -public-url https://chromemcp.dmf.zone \
//	  -allowed-email you@example.com \
//	  -identities-dir /data/identities
//	chromemcp serve -http 127.0.0.1:8787              local, no auth
//	chromemcp serve                                   stdio (Claude Code, etc.)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is stamped by the build (-ldflags "-X main.version=…").
var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "chromemcp: "+format+"\n", args...)
}

// logWriter routes the MCP SDK's slog output through logf.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	cmd, rest := splitCommand(os.Args[1:])
	switch cmd {
	case "serve":
		os.Exit(serve(rest))
	case "version":
		fmt.Println("chromemcp", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `chromemcp — Chrome browser sessions for agents, over MCP

usage: chromemcp <command> [flags]

commands:
  serve        run the MCP server            chromemcp serve -http :8787 ...
  version      print the version

`)
}

// splitCommand pops a leading subcommand (the first non-flag argument) off
// argv. flag.Parse would stop at it and leave every later flag unparsed.
func splitCommand(argv []string) (cmd string, rest []string) {
	rest = argv
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		cmd, rest = rest[0], rest[1:]
	}
	return cmd, rest
}

// findExe returns the first of the candidates on PATH, or "".
func findExe(candidates ...string) string {
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func defaultChrome() string {
	if p := os.Getenv("CHROMEMCP_CHROME"); p != "" {
		return p
	}
	if p := os.Getenv("CHROME_PATH"); p != "" {
		return p
	}
	return findExe("google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "chrome")
}

func defaultXvnc() string {
	if p := os.Getenv("CHROMEMCP_XVNC"); p != "" {
		return p
	}
	return findExe("Xvnc", "Xtigervnc")
}

func defaultDevToolsMCP() string {
	if p := os.Getenv("CHROMEMCP_DEVTOOLS_MCP"); p != "" {
		return p
	}
	if p := findExe("chrome-devtools-mcp"); p != "" {
		return p
	}
	if findExe("npx") != "" {
		return "npx -y chrome-devtools-mcp@" + devToolsMCPVersion
	}
	return ""
}

// devToolsMCPVersion is the chrome-devtools-mcp release this server was
// written against (its CLI flags and tool names). The Dockerfile installs
// exactly this version.
const devToolsMCPVersion = "1.8.0"

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		httpAddr = fs.String("http", env("CHROMEMCP_HTTP", ""), "serve Streamable HTTP on this address instead of stdio, e.g. :8787 (env CHROMEMCP_HTTP)")
		issuer   = fs.String("oauth-issuer", env("CHROMEMCP_OAUTH_ISSUER", ""), "OAuth authorization server issuer URL (e.g. https://xyz.authkit.app); enables bearer-token auth")
		public   = fs.String("public-url", env("CHROMEMCP_PUBLIC_URL", ""), "canonical public URL of this server; the OAuth resource identifier tokens must be addressed to, and the base of live-view links")
		emailPin = fs.String("allowed-email", env("CHROMEMCP_ALLOWED_EMAIL", ""), "only accept tokens whose email claim matches this address")
		viewBase = fs.String("view-url", env("CHROMEMCP_VIEW_URL", ""), "base URL for live-view links when it differs from -public-url (default: -public-url, else http://<-http>)")

		sessionsDir   = fs.String("sessions-dir", env("CHROMEMCP_SESSIONS_DIR", filepath.Join(os.TempDir(), "chromemcp-sessions")), "where session profiles live; ephemeral by design (env CHROMEMCP_SESSIONS_DIR)")
		identitiesDir = fs.String("identities-dir", env("CHROMEMCP_IDENTITIES_DIR", defaultIdentitiesDir()), "where saved identities (logged-in profile snapshots) persist (env CHROMEMCP_IDENTITIES_DIR)")

		chromePath  = fs.String("chrome", defaultChrome(), "the Chrome binary (env CHROMEMCP_CHROME, CHROME_PATH; default: the first of google-chrome-stable, google-chrome, chromium on PATH)")
		noSandbox   = fs.Bool("no-sandbox", envBool("CHROMEMCP_NO_SANDBOX", false), "launch Chrome with --no-sandbox, for containers that cannot give it user namespaces (env CHROMEMCP_NO_SANDBOX)")
		chromeFlags multiFlag
		xvncPath    = fs.String("xvnc", defaultXvnc(), "the Xvnc binary (TigerVNC) headful sessions run on; empty disables headful sessions (env CHROMEMCP_XVNC)")
		novncDir    = fs.String("novnc", env("CHROMEMCP_NOVNC", "/usr/share/novnc"), "the noVNC web client directory served under /view/ (env CHROMEMCP_NOVNC)")
		devtoolsCmd = fs.String("devtools-mcp", defaultDevToolsMCP(), "command that runs chrome-devtools-mcp over stdio; empty disables the passthrough (env CHROMEMCP_DEVTOOLS_MCP)")
		viewport    = fs.String("viewport", env("CHROMEMCP_VIEWPORT", "1280x800"), "default page viewport WxH for new sessions (env CHROMEMCP_VIEWPORT)")

		idlePark   = fs.Duration("idle-park", envDuration("CHROMEMCP_IDLE_PARK", 30*time.Minute), "close Chrome for a session idle this long, keeping its profile so it can be resumed (0 disables)")
		maxAge     = fs.Duration("max-age", envDuration("CHROMEMCP_MAX_AGE", 24*time.Hour), "delete sessions not used for this long (0 disables)")
		maxRunning = fs.Int("max-running", envInt("CHROMEMCP_MAX_RUNNING", 6), "most Chrome instances alive at once; the least recently used is parked to make room")
		viewTTL    = fs.Duration("view-ttl", envDuration("CHROMEMCP_VIEW_TTL", 30*time.Minute), "how long a live-view link stays valid")
		verbose    = fs.Bool("verbose", false, "log the MCP SDK and Chrome's stderr chatter")
	)
	fs.Var(&chromeFlags, "chrome-flag", "extra Chrome command-line flag (repeatable; env CHROMEMCP_CHROME_FLAGS, space-separated)")
	fs.Parse(args)

	var oauthCfg *oauthConfig
	if *issuer != "" {
		if *httpAddr == "" {
			die(fmt.Errorf("-oauth-issuer only applies to the HTTP transport (add -http addr)"))
		}
		if *public == "" {
			die(fmt.Errorf("-oauth-issuer needs -public-url (the URL clients reach this server at)"))
		}
		oauthCfg = &oauthConfig{Issuer: *issuer, PublicURL: *public, AllowedEmail: *emailPin}
	} else if *emailPin != "" {
		die(fmt.Errorf("-allowed-email needs -oauth-issuer"))
	}
	if *chromePath == "" {
		die(fmt.Errorf("no Chrome found: set -chrome or CHROMEMCP_CHROME"))
	}
	w, h, err := parseViewport(*viewport)
	if err != nil {
		die(err)
	}
	extra := []string(chromeFlags)
	if len(extra) == 0 {
		extra = strings.Fields(os.Getenv("CHROMEMCP_CHROME_FLAGS"))
	}

	base := *viewBase
	if base == "" {
		base = *public
	}
	if base == "" && *httpAddr != "" {
		host, port, _ := strings.Cut(*httpAddr, ":")
		if host == "" || host == "0.0.0.0" || host == "[::]" {
			host = "127.0.0.1"
		}
		base = "http://" + host + ":" + port
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr, err := newManager(&managerConfig{
		SessionsDir:   *sessionsDir,
		IdentitiesDir: *identitiesDir,
		Chrome:        *chromePath,
		ChromeFlags:   extra,
		NoSandbox:     *noSandbox,
		Xvnc:          *xvncPath,
		DevToolsMCP:   *devtoolsCmd,
		ViewportW:     w,
		ViewportH:     h,
		IdlePark:      *idlePark,
		MaxAge:        *maxAge,
		MaxRunning:    *maxRunning,
		ViewTTL:       *viewTTL,
		ViewBase:      strings.TrimSuffix(base, "/"),
		Verbose:       *verbose,
	})
	if err != nil {
		die(err)
	}
	logf("chrome %s; sessions in %s; identities in %s", *chromePath, *sessionsDir, *identitiesDir)
	if *xvncPath == "" {
		logf("no Xvnc: headful sessions are unavailable (install tigervnc-standalone-server or set -xvnc)")
	}
	if *devtoolsCmd == "" {
		logf("no chrome-devtools-mcp command: the devtools_* passthrough is unavailable")
	}
	go mgr.reaper(ctx)

	app := &mcpApp{mgr: mgr}
	views := newViewHandler(mgr, *novncDir)
	err = runMCP(ctx, app, *httpAddr, oauthCfg, views, *verbose)
	mgr.shutdown()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		die(err)
	}
	return 0
}

func defaultIdentitiesDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "chromemcp", "identities")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "chromemcp-identities")
	}
	return filepath.Join(home, ".local", "share", "chromemcp", "identities")
}

func parseViewport(s string) (w, h int, err error) {
	ws, hs, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok {
		return 0, 0, fmt.Errorf("viewport %q: want WxH", s)
	}
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if err1 != nil || err2 != nil || w < 200 || h < 200 || w > 4096 || h > 4096 {
		return 0, 0, fmt.Errorf("viewport %q: want WxH between 200 and 4096", s)
	}
	return w, h, nil
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "chromemcp: error:", err)
	os.Exit(1)
}
