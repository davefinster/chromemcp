package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testChrome returns a manager with a real headless Chrome, or skips.
func testChrome(t *testing.T) *manager {
	t.Helper()
	exe := defaultChrome()
	if exe == "" || os.Getenv("CHROMEMCP_TEST_NO_CHROME") != "" {
		t.Skip("no Chrome on PATH")
	}
	mgr, err := newManager(&managerConfig{
		SessionsDir:   filepath.Join(t.TempDir(), "sessions"),
		IdentitiesDir: filepath.Join(t.TempDir(), "identities"),
		Chrome:        exe,
		NoSandbox:     os.Getenv("CHROMEMCP_NO_SANDBOX") == "1",
		ViewportW:     900,
		ViewportH:     600,
		MaxRunning:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.shutdown)
	return mgr
}

const testPage = `<!doctype html><html><head><title>Test Page</title></head><body>
<h1>Hello</h1>
<form onsubmit="event.preventDefault(); document.getElementById('out').textContent = 'submitted:' + document.getElementById('q').value">
  <label for="q">Query</label> <input id="q" name="q" placeholder="type here">
  <select id="sel"><option value="a">Alpha</option><option value="b">Beta</option></select>
  <label><input type="checkbox" id="cb"> Remember</label>
  <button type="submit">Go</button>
</form>
<a href="/second">Second page</a>
<p id="out"></p>
<div style="height:3000px"></div>
<button id="bottom" onclick="document.getElementById('out').textContent='bottom clicked'">Bottom</button>
<script>console.log('page loaded'); document.cookie = 'tc=1; max-age=3600; path=/';</script>
</body></html>`

func TestBrowserIntegration(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/second" {
			w.Write([]byte("<title>Second</title><h1>Second page</h1>"))
			return
		}
		w.Write([]byte(testPage))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s, err := mgr.start(ctx, startOptions{Label: "integration"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := s.meta.ID

	// Navigate, snapshot, and act by ref, text and selector.
	var snap *snapResult
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil {
			return err
		}
		if w, h, err := viewportSize(ctx, tb); err != nil || w != 900 || h != 600 {
			t.Errorf("viewport %dx%d %v, want 900x600", w, h, err)
		}
		snap, err = snapshot(ctx, tb, 100, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Title != "Test Page" || len(snap.Elements) < 6 {
		t.Fatalf("snapshot: %s", snap.render(true))
	}
	byName := map[string]snapElement{}
	for _, e := range snap.Elements {
		byName[e.Name] = e
	}
	if q, ok := byName["Query"]; !ok || q.Role != "textbox" {
		t.Errorf("query field: %+v", byName)
	}
	if b, ok := byName["Bottom"]; !ok || b.InView {
		t.Errorf("bottom button should be offscreen: %+v", b)
	}

	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		// Type by ref, submit with Enter.
		res, err := resolveTarget(ctx, tb, targetSpec{Ref: byName["Query"].Ref})
		if err != nil {
			return err
		}
		if err := clickAt(ctx, tb, res.X, res.Y, "left", 1, 0); err != nil {
			return err
		}
		if err := insertText(ctx, tb, "hello world"); err != nil {
			return err
		}
		k, m, _ := parseKey("Enter")
		if err := pressKey(ctx, tb, k, m); err != nil {
			return err
		}
		var out string
		if err := evalJSON(ctx, tb, "document.getElementById('out').textContent", &out); err != nil {
			return err
		}
		if out != "submitted:hello world" {
			t.Errorf("after typing: out=%q", out)
		}
		// Select by text, checkbox by its label text.
		var sel map[string]string
		if err := evalJSON(ctx, tb, jsCall("selectOption", targetSpec{Selector: "#sel"}, "Beta"), &sel); err != nil {
			return err
		}
		if sel["value"] != "b" {
			t.Errorf("select: %v", sel)
		}
		res, err = resolveTarget(ctx, tb, targetSpec{Text: "Remember"})
		if err != nil {
			return err
		}
		if err := clickAt(ctx, tb, res.X, res.Y, "left", 1, 0); err != nil {
			return err
		}
		var checked bool
		evalJSON(ctx, tb, "document.getElementById('cb').checked", &checked)
		if !checked {
			t.Error("checkbox not checked by clicking its label")
		}
		// Clicking an offscreen element scrolls it into view first.
		res, err = resolveTarget(ctx, tb, targetSpec{Selector: "#bottom"})
		if err != nil {
			return err
		}
		if err := clickAt(ctx, tb, res.X, res.Y, "left", 1, 0); err != nil {
			return err
		}
		evalJSON(ctx, tb, "document.getElementById('out').textContent", &out)
		if out != "bottom clicked" {
			t.Errorf("bottom click: out=%q", out)
		}
		// Screenshots: viewport, labels, element crop, downscale.
		img, mime, err := screenshot(ctx, tb, screenshotOpts{Labels: true})
		if err != nil || mime != "image/jpeg" || len(img) < 1000 {
			t.Errorf("screenshot: %d bytes %s %v", len(img), mime, err)
		}
		var labels int
		evalJSON(ctx, tb, "document.querySelectorAll('#__cmcp_labels').length", &labels)
		if labels != 0 {
			t.Error("label overlay left in the page")
		}
		box := res.Box
		if img, _, err := screenshot(ctx, tb, screenshotOpts{Clip: &box, Format: "png"}); err != nil || len(img) == 0 {
			t.Errorf("element screenshot: %v", err)
		}
		if img, _, err := screenshot(ctx, tb, screenshotOpts{Scale: 0.5}); err != nil || len(img) == 0 {
			t.Errorf("scaled screenshot: %v", err)
		}
		// Text and console.
		tr, err := readText(ctx, tb, "", 1000)
		if err != nil || !strings.Contains(tr.Text, "Hello") {
			t.Errorf("readText: %+v %v", tr, err)
		}
		if c := tb.consoleText(false, "log"); !strings.Contains(c, "page loaded") {
			t.Errorf("console: %s", c)
		}
		// Navigation by clicking a link, then history.
		res, err = resolveTarget(ctx, tb, targetSpec{Text: "Second page"})
		if err != nil {
			return err
		}
		if err := clickAt(ctx, tb, res.X, res.Y, "left", 1, 0); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		settle(ctx, tb, 5*time.Second)
		if h := s.header(ctx, tb); !strings.Contains(h, "/second") {
			t.Errorf("after link click: %s", h)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A second tab, then close it.
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		t2, err := s.newTab(ctx, "about:blank")
		if err != nil {
			return err
		}
		if len(s.tabs) != 2 || t2.alias != "t2" {
			t.Errorf("tabs after new: %d, alias %s", len(s.tabs), t2.alias)
		}
		return s.closeTab(ctx, t2)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Park: cookies exported, tabs remembered; resume brings both back.
	s.mu.Lock()
	s.park()
	s.mu.Unlock()
	if s.isRunning() {
		t.Fatal("still running after park")
	}
	if _, err := os.Stat(filepath.Join(s.dir, cookiesFile)); err != nil {
		t.Error("no cookie jar after park")
	}
	if len(s.meta.Tabs) != 1 || !strings.HasSuffix(s.meta.Tabs[0], "/second") {
		t.Errorf("remembered tabs: %v", s.meta.Tabs)
	}
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if h := s.header(ctx, tb); !strings.Contains(h, "/second") {
			t.Errorf("after resume: %s", h)
		}
		var cookie string
		evalJSON(ctx, tb, "document.cookie", &cookie)
		if !strings.Contains(cookie, "tc=1") {
			t.Errorf("cookie lost across park/resume: %q", cookie)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, cookiesFile)); err == nil {
		t.Error("cookie jar not consumed on resume")
	}

	// Identity: save from the running session, start another from it.
	s.mu.Lock()
	s.park()
	m, err := mgr.identities.save("tester", "", sid, s.dir, false)
	relaunch := s.launch(ctx)
	s.mu.Unlock()
	if err != nil || relaunch != nil {
		t.Fatalf("identity save %v relaunch %v", err, relaunch)
	}
	if m.Bytes == 0 {
		t.Error("empty identity")
	}
	s2, err := mgr.start(ctx, startOptions{Identity: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s2.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL+"/second", 20*time.Second); err != nil {
			return err
		}
		var cookie string
		evalJSON(ctx, tb, "document.cookie", &cookie)
		if !strings.Contains(cookie, "tc=1") {
			t.Errorf("identity did not carry the cookie: %q", cookie)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// MaxRunning 2: a third session parks the least recently used.
	s3, err := mgr.start(ctx, startOptions{})
	if err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, x := range mgr.list() {
		if x.isRunningQuick() {
			running++
		}
	}
	if running != 2 {
		t.Errorf("%d running, want 2", running)
	}
	if err := mgr.remove(s3.meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s3.dir); err == nil {
		t.Error("session dir survives delete")
	}
	if _, err := mgr.get(s3.meta.ID); err == nil {
		t.Error("deleted session still listed")
	}
}

func TestParkedSessionsReload(t *testing.T) {
	dir := t.TempDir()
	meta := sessionMeta{ID: "s-0000abcd", Mode: modeHeadless, Created: time.Now(), LastUsed: time.Now(), Width: 1, Height: 1}
	s := &session{dir: filepath.Join(dir, meta.ID), meta: meta}
	os.MkdirAll(s.dir, 0o700)
	s.lastUsed.Store(time.Now().UnixNano())
	if err := s.saveMeta(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "junk"), 0o700)
	mgr, err := newManager(&managerConfig{SessionsDir: dir, IdentitiesDir: filepath.Join(t.TempDir(), "i"), Chrome: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mgr.get("s-0000abcd")
	if err != nil || got.state() != "parked" || got.meta.Mode != modeHeadless {
		t.Errorf("reloaded: %+v %v", got, err)
	}
	if _, err := mgr.get("junk"); err == nil {
		t.Error("junk directory loaded as a session")
	}
}
