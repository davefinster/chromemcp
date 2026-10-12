package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// noVNC's page gets the paste hook; the paste endpoint takes only a POST
// of some text from the page itself.
func TestPasteHTTP(t *testing.T) {
	novnc := t.TempDir()
	os.WriteFile(filepath.Join(novnc, "vnc.html"), []byte("<html><body>noVNC</body></html>"), 0o600)
	mgr := &manager{cfg: &managerConfig{Xvnc: "/usr/bin/Xvnc"}, views: newViewTokens(time.Minute), sessions: map[string]*session{}}
	h := newViewHandler(mgr, novnc)
	tok, _ := mgr.views.issue("s-1")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/view/"+tok+"/vnc.html", nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "function chromemcpPaste(") ||
		!strings.HasSuffix(body, "</script>\n</body></html>") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("vnc.html: %d %v\n%s", w.Code, w.Header(), body)
	}

	for _, tt := range []struct {
		name, method, site, body string
		code                     int
	}{
		{"get", "GET", "", "", 405},
		{"cross-site", "POST", "cross-site", "secret", 403},
		{"too long", "POST", "same-origin", strings.Repeat("x", pasteMax+1), 413},
		{"empty", "POST", "same-origin", "", 400},
		{"no session", "POST", "same-origin", "secret", 404},
		{"no fetch metadata", "POST", "", "secret", 404},
	} {
		r := httptest.NewRequest(tt.method, "/view/"+tok+"/paste", strings.NewReader(tt.body))
		if tt.site != "" {
			r.Header.Set("Sec-Fetch-Site", tt.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.code {
			t.Errorf("%s: got %d, want %d (%s)", tt.name, w.Code, tt.code, strings.TrimSpace(w.Body.String()))
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/view/nope/paste", strings.NewReader("secret")))
	if w.Code != http.StatusForbidden {
		t.Errorf("a bad token got %d", w.Code)
	}
}

const pasteTestPage = `<!doctype html><title>Sign in</title>
<input id="user" style="position:absolute;left:40px;top:40px;width:300px">
<input id="pass" type="password" style="position:absolute;left:40px;top:100px;width:300px">
<button id="go" style="position:absolute;left:40px;top:160px">Go</button>`

func postPaste(t *testing.T, url, text string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(text))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// The noVNC view's paste goes to the field that has the focus in the page
// that has it, and nowhere when nothing that takes text has it.
func TestPasteFocusedPage(t *testing.T) {
	mgr := testChrome(t)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(pasteTestPage)) }))
	defer page.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		return navigate(ctx, tb, page.URL, 20*time.Second)
	})
	if err != nil {
		t.Fatal(err)
	}
	focus := func(id string) {
		t.Helper()
		err := mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
			var ok bool
			return evalJSON(ctx, tb, "(document.getElementById('"+id+"').focus(), true)", &ok)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	values := func() (v struct{ User, Pass string }) {
		t.Helper()
		mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
			return evalJSON(ctx, tb, "({User: document.getElementById('user').value, Pass: document.getElementById('pass').value})", &v)
		})
		return v
	}
	mgr.views = newViewTokens(time.Minute)
	views := httptest.NewServer(newViewHandler(mgr, "/nonexistent"))
	defer views.Close()
	tok, _ := mgr.views.issue(s.meta.ID)
	url := views.URL + "/view/" + tok + "/paste"

	focus("pass")
	if code, msg := postPaste(t, url, "hunter2"); code != 200 || msg != "Pasted 7 characters into “Sign in”" {
		t.Errorf("paste into the password field: %d %q", code, msg)
	}
	if v := values(); v.Pass != "hunter2" || v.User != "" {
		t.Errorf("after the paste: %+v", v)
	}
	// An agent's action holds the session for as long as it runs, a wait
	// for this very sign-in included; a paste does not wait for it.
	focus("user")
	s.mu.Lock()
	code, msg := postPaste(t, url, "me@example.com")
	s.mu.Unlock()
	if code != 200 {
		t.Errorf("paste while the session is busy: %d %q", code, msg)
	}
	if v := values(); v.Pass != "hunter2" || v.User != "me@example.com" {
		t.Errorf("after the paste into the user field: %+v", v)
	}
	focus("go")
	if code, msg := postPaste(t, url, "nowhere"); code != http.StatusConflict || !strings.Contains(msg, "click into the text field") {
		t.Errorf("paste with a button focused: %d %q", code, msg)
	}
	if v := values(); v.Pass != "hunter2" || v.User != "me@example.com" {
		t.Errorf("a refused paste went somewhere: %+v", v)
	}
	mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error { s.park(); return nil })
	if code, msg := postPaste(t, url, "parked"); code != http.StatusConflict || !strings.Contains(msg, "parked") {
		t.Errorf("paste into a parked session: %d %q", code, msg)
	}
}

// The viewer's half, in a browser: the paste shortcut over the screen of the
// screencast page sends the viewer's clipboard into the session's field.
func TestPasteViewerCast(t *testing.T) {
	mgr := testChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, url := pasteSession(t, ctx, mgr, modeHeadless)
	for _, mac := range []bool{false, true} {
		viewerPaste(t, ctx, mgr, s, url+"/cast", "#stage", mac)
	}
}

// The same through noVNC's own page, on a headful session's Xvnc. It needs
// noVNC and Xvnc, which only the image has: CHROMEMCP_TEST_NOVNC names
// noVNC's directory, and Xvnc is CHROMEMCP_XVNC or on PATH.
func TestPasteViewerNoVNC(t *testing.T) {
	novnc := os.Getenv("CHROMEMCP_TEST_NOVNC")
	xvnc := defaultXvnc()
	if novnc == "" || xvnc == "" {
		t.Skip("CHROMEMCP_TEST_NOVNC and an Xvnc are needed")
	}
	mgr := testChrome(t)
	mgr.cfg.Xvnc = xvnc
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, url := pasteSession(t, ctx, mgr, modeHeadful, novnc)
	for _, mac := range []bool{false, true} {
		viewerPaste(t, ctx, mgr, s, url+"/vnc.html?autoconnect=1&resize=scale&path=ws", "#noVNC_container canvas", mac)
	}
}

// pasteSession starts a session on pasteTestPage and returns it with the
// base of its view URLs.
func pasteSession(t *testing.T, ctx context.Context, mgr *manager, mode string, novnc ...string) (*session, string) {
	t.Helper()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(pasteTestPage)) }))
	t.Cleanup(page.Close)
	s, err := mgr.start(ctx, startOptions{Device: nativeDevice, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		return navigate(ctx, tb, page.URL, 20*time.Second)
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := "/nonexistent"
	if len(novnc) > 0 {
		dir = novnc[0]
	}
	mgr.views = newViewTokens(time.Minute)
	views := httptest.NewServer(newViewHandler(mgr, dir))
	t.Cleanup(views.Close)
	tok, _ := mgr.views.issue(s.meta.ID)
	return s, views.URL + "/view/" + tok
}

// viewerPaste empties the session's password field and focuses it, opens
// a view in a second session, the viewer -- as a Mac, or not -- puts text on
// its clipboard, focuses the screen and presses the paste shortcut there;
// then a key pressed straight after it must arrive after the text.
func viewerPaste(t *testing.T, ctx context.Context, mgr *manager, s *session, viewURL, screen string, mac bool) {
	t.Helper()
	err := mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		var ok bool
		return evalJSON(ctx, tb, "(document.getElementById('pass').value = '', document.getElementById('pass').focus(), true)", &ok)
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.remove(viewer.meta.ID)
	const secret = "pa55-wörd!"
	mod, modKey, modCode, modVK := input.ModifierCtrl, "Control", "ControlLeft", int64(17)
	if mac {
		mod, modKey, modCode, modVK = input.ModifierMeta, "Meta", "MetaLeft", 91
	}
	err = mgr.withTab(ctx, viewer.meta.ID, "", func(ctx context.Context, _ *session, tb *tab) error {
		if mac {
			// What the hook and noVNC each read to tell a Mac.
			err := chromedp.Run(tb.ctx, emulation.SetUserAgentOverride("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)").
				WithPlatform("MacIntel").WithUserAgentMetadata(&emulation.UserAgentMetadata{Platform: "macOS", PlatformVersion: "15.0.0", Architecture: "arm"}))
			if err != nil {
				return err
			}
		}
		if err := navigate(ctx, tb, viewURL, 20*time.Second); err != nil {
			return err
		}
		origin := viewURL[:strings.Index(viewURL, "/view/")]
		return chromedp.Run(tb.ctx,
			browser.SetPermission(&browser.PermissionDescriptor{Name: "clipboard-write"}, browser.PermissionSettingGranted).WithOrigin(origin),
			chromedp.WaitVisible(screen, chromedp.ByQuery),
			chromedp.Evaluate(`navigator.clipboard.writeText(`+jsString(secret)+`)`, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
				return p.WithAwaitPromise(true).WithUserGesture(true)
			}),
			chromedp.Evaluate(`document.querySelector(`+jsString(screen)+`).focus()`, nil),
			chromedp.ActionFunc(func(ctx context.Context) error {
				for _, k := range []*input.DispatchKeyEventParams{
					input.DispatchKeyEvent(input.KeyRawDown).WithKey(modKey).WithCode(modCode).WithWindowsVirtualKeyCode(modVK).WithModifiers(mod),
					input.DispatchKeyEvent(input.KeyRawDown).WithKey("v").WithCode("KeyV").WithWindowsVirtualKeyCode(86).WithModifiers(mod).WithCommands([]string{"paste"}),
					input.DispatchKeyEvent(input.KeyUp).WithKey("v").WithCode("KeyV").WithWindowsVirtualKeyCode(86).WithModifiers(mod),
					input.DispatchKeyEvent(input.KeyUp).WithKey(modKey).WithCode(modCode).WithWindowsVirtualKeyCode(modVK),
					// Typed at once: it must wait for the paste.
					input.DispatchKeyEvent(input.KeyDown).WithKey("!").WithCode("Digit1").WithWindowsVirtualKeyCode(49).WithText("!").WithModifiers(input.ModifierShift),
					input.DispatchKeyEvent(input.KeyUp).WithKey("!").WithCode("Digit1").WithWindowsVirtualKeyCode(49).WithModifiers(input.ModifierShift),
				} {
					if err := k.Do(ctx); err != nil {
						return err
					}
				}
				return nil
			}),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Pass, Note string
		Focused    bool
	}
	for i := 0; i < 50; i++ {
		mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, _ *session, tb *tab) error {
			return evalJSON(ctx, tb, "({Pass: document.getElementById('pass').value})", &got)
		})
		mgr.withTab(ctx, viewer.meta.ID, "", func(ctx context.Context, _ *session, tb *tab) error {
			return evalJSON(ctx, tb, `({Note: [...document.querySelectorAll('[role=status]')].map(e => e.textContent).join(''),
				Focused: document.activeElement === document.querySelector(`+jsString(screen)+`)})`, &got)
		})
		if got.Pass == secret+"!" && got.Note != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Pass != secret+"!" {
		t.Errorf("mac %v: the session's field holds %q, want %q", mac, got.Pass, secret+"!")
	}
	if !strings.HasPrefix(got.Note, "Pasted 10 characters") || !got.Focused {
		t.Errorf("mac %v: the viewer says %q, its screen focused: %v", mac, got.Note, got.Focused)
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
