package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseKey(t *testing.T) {
	tests := []struct {
		spec string
		key  string
		mods input.Modifier
		text string
	}{
		{"Enter", "Enter", 0, "\r"},
		{"a", "a", 0, "a"},
		{"A", "A", input.ModifierShift, "A"},
		{"Control+a", "a", input.ModifierCtrl, "a"},
		{"Shift+Tab", "Tab", input.ModifierShift, ""},
		{"Meta+Enter", "Enter", input.ModifierMeta, "\r"},
		{"ArrowDown", "ArrowDown", 0, ""},
		{"escape", "Escape", 0, ""},
		{"F5", "F5", 0, ""},
		{"Space", " ", 0, " "},
	}
	for _, tt := range tests {
		k, mods, err := parseKey(tt.spec)
		if err != nil {
			t.Errorf("%s: %v", tt.spec, err)
			continue
		}
		if k.Key != tt.key || mods != tt.mods || k.Text != tt.text {
			t.Errorf("%s: got key %q mods %d text %q, want %q %d %q", tt.spec, k.Key, mods, k.Text, tt.key, tt.mods, tt.text)
		}
	}
	for _, bad := range []string{"Hyper+a", "Control+", "NoSuchKey"} {
		if _, _, err := parseKey(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if mods, _ := parseModifiers("Control+Shift"); mods != input.ModifierCtrl|input.ModifierShift {
		t.Errorf("parseModifiers: got %d", mods)
	}
}

func TestParseViewport(t *testing.T) {
	if w, h, err := parseViewport("1280x800"); err != nil || w != 1280 || h != 800 {
		t.Errorf("got %d %d %v", w, h, err)
	}
	if w, h, err := parseViewport("390X844"); err != nil || w != 390 || h != 844 {
		t.Errorf("got %d %d %v", w, h, err)
	}
	for _, bad := range []string{"1280", "10x10", "5000x100", "axb"} {
		if _, _, err := parseViewport(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":         "https://example.com",
		"http://x.test/a?b=1": "http://x.test/a?b=1",
		"about:blank":         "about:blank",
		" https://a.b ":       "https://a.b",
	} {
		got, err := normalizeURL(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := normalizeURL(""); err == nil {
		t.Error("empty: want error")
	}
}

func TestChromeFlags(t *testing.T) {
	l := &chromeLaunch{Exe: "chrome", UserDataDir: "/p", Headless: true, Width: 1000, Height: 700, NoSandbox: true, ExtraFlags: []string{"--lang=en-AU"}}
	flags := strings.Join(l.flags(), " ")
	for _, want := range []string{"--user-data-dir=/p", "--remote-debugging-port=0", "--headless=new", "--no-sandbox", "--window-size=1000,700", "--password-store=basic", "--lang=en-AU", "about:blank"} {
		if !strings.Contains(flags, want) {
			t.Errorf("flags lack %s: %s", want, flags)
		}
	}
	l.Headless, l.NoSandbox = false, false
	flags = strings.Join(l.flags(), " ")
	if strings.Contains(flags, "headless") || strings.Contains(flags, "no-sandbox") {
		t.Errorf("headful flags: %s", flags)
	}
}

// fakeProfile writes a profile tree with the kinds of entries a copy must
// keep and the kinds it must drop.
func fakeProfile(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"Local State":                           `{"os_crypt":{}}`,
		"Default/Preferences":                   `{}`,
		"Default/Cookies":                       "sqlite",
		"Default/Local Storage/leveldb/000.log": "kv",
		"Default/Cache/Cache_Data/index":        "cache", // dropped
		"Default/Code Cache/js/index":           "cache", // dropped
		"Default/Service Worker/CacheStorage/x": "sw",    // dropped
		"SingletonLock":                         "lock",  // dropped
		"DevToolsActivePort":                    "1234",  // dropped
		"BrowserMetrics/x.pma":                  "pma",   // dropped
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopyProfile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "profile")
	fakeProfile(t, src)
	dst := filepath.Join(t.TempDir(), "copy")
	n, err := copyProfile(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("copied nothing")
	}
	for _, keep := range []string{"Local State", "Default/Preferences", "Default/Cookies", "Default/Local Storage/leveldb/000.log"} {
		if _, err := os.Stat(filepath.Join(dst, keep)); err != nil {
			t.Errorf("%s not copied", keep)
		}
	}
	for _, drop := range []string{"Default/Cache", "Default/Code Cache", "Default/Service Worker", "SingletonLock", "DevToolsActivePort", "BrowserMetrics/x.pma"} {
		if _, err := os.Stat(filepath.Join(dst, drop)); err == nil {
			t.Errorf("%s copied but should be skipped", drop)
		}
	}
}

func TestIdentityStore(t *testing.T) {
	st, err := newIdentityStore(filepath.Join(t.TempDir(), "ids"))
	if err != nil {
		t.Fatal(err)
	}
	session := t.TempDir()
	fakeProfile(t, filepath.Join(session, "profile"))
	os.WriteFile(filepath.Join(session, cookiesFile), []byte(`[{"name":"sid","value":"1","domain":"a.test","path":"/"}]`), 0o600)

	if _, err := st.save("Bad Name", "", "s", session, false); err == nil {
		t.Error("bad name accepted")
	}
	m, err := st.save("google-me", "google me@example.com", "s-1", session, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "google-me" || m.Note != "google me@example.com" || m.Bytes == 0 {
		t.Errorf("meta %+v", m)
	}
	if _, err := st.save("google-me", "", "s-2", session, false); err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Errorf("second save without overwrite: %v", err)
	}
	m2, err := st.save("google-me", "", "s-2", session, true)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Note != "google me@example.com" || !m2.Created.Equal(m.Created) || m2.Session != "s-2" {
		t.Errorf("overwrite lost metadata: %+v", m2)
	}
	list, _ := st.list()
	if len(list) != 1 || list[0].Name != "google-me" {
		t.Errorf("list %+v", list)
	}

	dst := t.TempDir()
	if err := st.seed("google-me", dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "profile", "Default", "Cookies")); err != nil {
		t.Error("seeded profile lacks Cookies")
	}
	if b, err := os.ReadFile(filepath.Join(dst, cookiesFile)); err != nil || !strings.Contains(string(b), "sid") {
		t.Error("seeded session lacks the cookie jar")
	}
	if err := st.seed("nope", t.TempDir()); err == nil {
		t.Error("seeding an unknown identity succeeded")
	}
	if err := st.remove("google-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.get("google-me"); err == nil {
		t.Error("still there after remove")
	}
}

func TestViewTokens(t *testing.T) {
	v := newViewTokens(50 * time.Millisecond)
	tok, _ := v.issue("s-1")
	if len(tok) != 48 {
		t.Errorf("token %q", tok)
	}
	if sid, ok := v.lookup(tok); !ok || sid != "s-1" {
		t.Error("lookup failed")
	}
	if _, ok := v.lookup("nope"); ok {
		t.Error("unknown token accepted")
	}
	tok2, _ := v.issue("s-2")
	v.revokeSession("s-2")
	if _, ok := v.lookup(tok2); ok {
		t.Error("revoked token accepted")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := v.lookup(tok); ok {
		t.Error("expired token accepted")
	}
}

func TestViewHandlerHTTP(t *testing.T) {
	novnc := t.TempDir()
	os.WriteFile(filepath.Join(novnc, "vnc.html"), []byte("<html>noVNC</html>"), 0o600)
	mgr := &manager{cfg: &managerConfig{Xvnc: "/usr/bin/Xvnc"}, views: newViewTokens(time.Minute), sessions: map[string]*session{}}
	h := newViewHandler(mgr, novnc)
	if !h.available() {
		t.Fatal("view handler not available")
	}
	tok, _ := mgr.views.issue("s-1")

	for _, tt := range []struct {
		path string
		code int
	}{
		{"/view/", 404},
		{"/view/badtoken/vnc.html", 403},
		{"/view/" + tok + "/vnc.html", 200},
		{"/view/" + tok + "/", 302},
		{"/view/" + tok + "/ws", 404}, // session s-1 does not exist
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tt.path, nil))
		if w.Code != tt.code {
			t.Errorf("%s: got %d, want %d (%s)", tt.path, w.Code, tt.code, strings.TrimSpace(w.Body.String()))
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/view/"+tok+"/vnc.html", nil))
	if !strings.Contains(w.Body.String(), "noVNC") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("vnc.html: %q %v", w.Body.String(), w.Header())
	}
}

func TestSnapshotRender(t *testing.T) {
	var r snapResult
	err := json.Unmarshal([]byte(`{"url":"https://a.test/","title":"A","viewport":{"w":800,"h":600},"scroll":{"x":0,"y":0,"pageW":800,"pageH":2000},
	  "elements":[
	    {"ref":"e1","role":"link","tag":"a","name":"Home","box":[1,2,30,10],"inView":true,"href":"https://a.test/"},
	    {"ref":"e2","role":"textbox","tag":"input","name":"Email","type":"email","value":"x@y","box":[1,20,100,20],"inView":true},
	    {"ref":"e3","role":"checkbox","tag":"input","name":"Remember","checked":true,"box":[1,50,10,10],"inView":false},
	    {"ref":"e4","role":"combobox","tag":"select","name":"Country","value":"AU","options":["AU","NZ"],"box":[1,70,80,20],"inView":true,"disabled":true},
	    {"ref":"e5","role":"file","tag":"input","name":"Attachment","type":"file","box":[1,90,100,20],"inView":true}
	  ]}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	out := r.render(true)
	for _, want := range []string{
		`e1 link "Home" -> https://a.test/ @1,2 30x10`,
		`e2 textbox (email) "Email" value="x@y"`,
		`e3 checkbox "Remember" checked (offscreen)`,
		`e4 combobox "Country" value="AU" disabled options=[AU | NZ]`,
		"-- below/above the viewport",
		// A file input is worth a word: nothing else says how one is filled.
		"file_put puts one on the session, browser_upload gives it to the input",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	// Viewport-first ordering puts e4 (in view) before e3 (offscreen).
	if strings.Index(out, "e4 ") > strings.Index(out, "e3 ") {
		t.Errorf("offscreen element listed before in-view ones:\n%s", out)
	}
}

func TestJSCall(t *testing.T) {
	expr := jsCall("resolve", targetSpec{Ref: "e1"})
	if !strings.HasSuffix(expr, `.resolve({"ref":"e1"})`) || !strings.HasPrefix(expr, "(() => {") {
		t.Errorf("jsCall: %s", expr[len(expr)-60:])
	}
}

// TestToolRegistry checks the tool surface over an in-memory transport,
// without any Chrome.
func TestToolRegistry(t *testing.T) {
	mgr, err := newManager(&managerConfig{
		SessionsDir: filepath.Join(t.TempDir(), "s"), IdentitiesDir: filepath.Join(t.TempDir(), "i"),
		Chrome: "/nonexistent", DevToolsMCP: "chrome-devtools-mcp", ViewportW: 1280, ViewportH: 800,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newMCPServer(&mcpApp{mgr: mgr}, nil)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"session_start", "session_list", "session_resume", "session_stop", "session_delete", "session_view",
		"identity_list", "identity_save", "identity_delete",
		"file_put", "file_list", "file_delete", "file_upload_url",
		"browser_navigate", "browser_history", "browser_snapshot", "browser_screenshot", "browser_click", "browser_type",
		"browser_press", "browser_hover", "browser_scroll", "browser_select", "browser_upload", "browser_wait", "browser_read",
		"browser_evaluate", "browser_fingerprint", "browser_console", "browser_tabs", "browser_tab_new", "browser_tab_select", "browser_tab_close",
		"devtools_tools", "devtools_call",
	} {
		if !names[want] {
			t.Errorf("tool %s not registered", want)
		}
	}
	if len(res.Tools) != 35 {
		t.Errorf("%d tools, want 35", len(res.Tools))
	}
	// session_start advertises its closed-set arguments as enums on the wire,
	// so a client sees the valid values without reading the prose.
	for _, tool := range res.Tools {
		if tool.Name != "session_start" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type        string   `json:"type"`
				Description string   `json:"description"`
				Enum        []string `json:"enum"`
				Default     string   `json:"default"`
			} `json:"properties"`
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for prop, want := range map[string]struct {
			enum []string
			def  string
		}{
			"device": {deviceNames(), "windows"},
			"mode":   {[]string{"headless", "headful"}, "headless"},
		} {
			got := schema.Properties[prop]
			if got.Type != "string" || got.Description == "" {
				t.Errorf("session_start.%s: type %q, description %q", prop, got.Type, got.Description)
			}
			if strings.Join(got.Enum, ",") != strings.Join(want.enum, ",") || got.Default != want.def {
				t.Errorf("session_start.%s enum = %v default %q, want %v default %q", prop, got.Enum, got.Default, want.enum, want.def)
			}
			if !slices.Contains(got.Enum, got.Default) {
				t.Errorf("session_start.%s default %q is outside its enum %v", prop, got.Default, got.Enum)
			}
		}
	}
	// A device outside the enum is refused by schema validation before any Chrome is launched.
	r0, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "session_start", Arguments: map[string]any{"device": "amiga"}})
	if err != nil {
		t.Fatal(err)
	}
	if txt := r0.Content[0].(*mcp.TextContent).Text; !r0.IsError || !strings.Contains(txt, "enum") || !strings.Contains(txt, "windows") {
		t.Errorf("session_start device=amiga: isError=%v %q, want an enum error naming the valid devices", r0.IsError, txt)
	}
	// An unknown session is a tool error, not a transport error.
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "browser_navigate", Arguments: map[string]any{"session_id": "s-00000000", "url": "https://x.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError {
		t.Error("navigate on a missing session did not fail")
	}
	// The empty listing.
	r, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "session_list", Arguments: map[string]any{}})
	if txt := r.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "no sessions") {
		t.Errorf("session_list: %s", txt)
	}
}

func TestHealthz(t *testing.T) {
	mgr, _ := newManager(&managerConfig{SessionsDir: filepath.Join(t.TempDir(), "s"), IdentitiesDir: filepath.Join(t.TempDir(), "i"), Chrome: "/x"})
	w := httptest.NewRecorder()
	(&mcpApp{mgr: mgr}).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}
