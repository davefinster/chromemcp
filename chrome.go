package main

// Launching and stopping one Chrome process. The process is this server's
// own child rather than chromedp's: the server owns the user-data-dir (the
// session's profile), the display it renders on, and the remote-debugging
// port the DevTools MCP passthrough attaches to.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

type chromeLaunch struct {
	Exe         string
	UserDataDir string
	Downloads   string
	Headless    bool
	Display     string // DISPLAY for a headful launch
	Width       int    // window size; the page viewport in headless mode
	Height      int
	NoSandbox   bool
	ExtraFlags  []string
	Env         []string // extra environment, "K=V"
	Verbose     bool
	Logf        func(string, ...any)
}

// chromeProc is a running Chrome.
type chromeProc struct {
	cmd   *exec.Cmd
	Port  int    // remote-debugging port
	WSURL string // ws://127.0.0.1:port/devtools/browser/<id>

	done    chan struct{}
	exitErr error

	mu     sync.Mutex
	stderr []string // last lines, for diagnostics
}

// chromeFlags is the launch command line, kept separate for tests.
func (l *chromeLaunch) flags() []string {
	args := []string{
		"--user-data-dir=" + l.UserDataDir,
		"--remote-debugging-port=0",
		// A fresh install's first-run prompts and the "make Chrome your
		// default browser" nag would otherwise sit over the page.
		"--no-first-run",
		"--no-default-browser-check",
		// Cookies encrypted with the fixed "basic" store key rather than a
		// keyring this container does not have, so a saved identity's
		// profile decrypts wherever it is restored.
		"--password-store=basic",
		// A parked session is a Chrome that exited; on resume it must not
		// offer to restore pages, it must just restore its state quietly.
		"--hide-crash-restore-bubble",
		"--disable-session-crashed-bubble",
		"--disable-features=Translate,MediaRouter",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-dev-shm-usage",
		"--window-position=0,0",
		fmt.Sprintf("--window-size=%d,%d", l.Width, l.Height),
	}
	if l.Headless {
		args = append(args, "--headless=new", "--disable-gpu")
	} else {
		// Without a GPU, headful Chrome has no WebGL at all unless it is
		// allowed to fall back to SwiftShader — which headless does by
		// itself. A browser with no WebGL breaks maps and charts, and is
		// nothing like the PC a device profile describes.
		args = append(args, "--enable-unsafe-swiftshader")
	}
	if l.NoSandbox {
		args = append(args, "--no-sandbox", "--disable-setuid-sandbox")
	}
	args = append(args, l.ExtraFlags...)
	return append(args, "about:blank")
}

// launchChrome starts Chrome and waits for its DevTools endpoint.
func launchChrome(ctx context.Context, l *chromeLaunch) (*chromeProc, error) {
	if err := os.MkdirAll(l.UserDataDir, 0o700); err != nil {
		return nil, err
	}
	if l.Downloads != "" {
		os.MkdirAll(l.Downloads, 0o700)
	}
	// Chrome refuses to start on a profile whose previous instance is still
	// alive; a stale lock from a Chrome we killed is not that.
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(filepath.Join(l.UserDataDir, f))
	}
	os.Remove(filepath.Join(l.UserDataDir, "DevToolsActivePort"))

	cmd := exec.Command(l.Exe, l.flags()...)
	cmd.Env = append(os.Environ(), "HOME="+l.UserDataDir)
	cmd.Env = append(cmd.Env, l.Env...)
	if l.Headless {
		cmd.Env = append(cmd.Env, "DISPLAY=")
	} else {
		cmd.Env = append(cmd.Env, "DISPLAY="+l.Display)
	}
	// Its own process group, so stopping it takes its renderers along.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Stderr through a pipe of our own rather than cmd.StderrPipe: with
	// the latter, Wait returns only when every holder of the pipe has
	// closed it, and Chrome's crashpad helper outlives the browser by a
	// while — every clean exit would look like a hang.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = pw
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("starting chrome: %w", err)
	}
	pw.Close()
	p := &chromeProc{cmd: cmd, done: make(chan struct{})}
	go p.readStderr(pr, l)
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
		// Sweep the helpers (crashpad) that outlive the browser.
		time.AfterFunc(2*time.Second, func() { p.signal(syscall.SIGKILL) })
	}()

	// DevToolsActivePort appears once the debugging server is listening:
	// the port on the first line, the browser websocket path on the second.
	portFile := filepath.Join(l.UserDataDir, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-p.done:
			return nil, fmt.Errorf("chrome exited during startup: %v\n%s", p.exitErr, p.lastStderr())
		case <-ctx.Done():
			p.kill()
			return nil, ctx.Err()
		default:
		}
		if b, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) >= 2 {
				port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
				if err == nil && port > 0 && devtoolsUp(port) {
					p.Port = port
					p.WSURL = fmt.Sprintf("ws://127.0.0.1:%d%s", port, strings.TrimSpace(lines[1]))
					return p, nil
				}
			}
		}
		if time.Now().After(deadline) {
			p.kill()
			return nil, fmt.Errorf("chrome did not open its DevTools port within 30s\n%s", p.lastStderr())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func devtoolsUp(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *chromeProc) readStderr(r io.ReadCloser, l *chromeLaunch) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if l.Verbose && l.Logf != nil {
			l.Logf("chrome: %s", line)
		}
		p.mu.Lock()
		p.stderr = append(p.stderr, line)
		if len(p.stderr) > 40 {
			p.stderr = p.stderr[len(p.stderr)-40:]
		}
		p.mu.Unlock()
	}
}

func (p *chromeProc) lastStderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.stderr, "\n")
}

// Alive reports whether the process is still running.
func (p *chromeProc) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// PID is the process id, for the session listing.
func (p *chromeProc) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// closeGracefully asks Chrome to quit over its own DevTools websocket —
// Browser.close, which chromedp's executor refuses to send to a browser
// it did not launch — and reports whether Chrome acknowledged it.
func (p *chromeProc) closeGracefully(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, p.WSURL, nil)
	if err != nil {
		return false
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.close"}`)); err != nil {
		return false
	}
	_, msg, err := c.Read(ctx)
	return err == nil && strings.Contains(string(msg), `"id":1`)
}

// stop closes Chrome: gracefully if it will, then SIGTERM to the group,
// then SIGKILL. grace bounds the wait for the graceful exit.
func (p *chromeProc) stop(grace time.Duration) {
	if p.Alive() && p.closeGracefully(context.Background()) {
		select {
		case <-p.done:
			return
		case <-time.After(grace):
		}
	}
	p.signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
	}
	p.kill()
}

func (p *chromeProc) kill() {
	p.signal(syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}

func (p *chromeProc) signal(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	// The whole group: Chrome's renderers and GPU process included.
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		p.cmd.Process.Signal(sig)
	}
}
