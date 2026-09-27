package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A profile crosses to and from a node as an archive with copyProfile's
// rules, and an archive from the other end is unpacked only as plain files
// beneath the profile.
func TestProfileArchive(t *testing.T) {
	src := filepath.Join(t.TempDir(), "profile")
	fakeProfile(t, src)
	var buf bytes.Buffer
	if err := writeProfileArchive(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := readProfileArchive(bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"Local State", "Default/Preferences", "Default/Cookies", "Default/Local Storage/leveldb/000.log"} {
		if _, err := os.Stat(filepath.Join(dst, keep)); err != nil {
			t.Errorf("%s did not come across: %v", keep, err)
		}
	}
	for _, drop := range []string{"Default/Cache", "Default/Code Cache", "Default/Service Worker", "SingletonLock", "DevToolsActivePort", "BrowserMetrics"} {
		if _, err := os.Stat(filepath.Join(dst, drop)); err == nil {
			t.Errorf("%s came across", drop)
		}
	}
	if _, err := readProfileArchive(bytes.NewReader(buf.Bytes()), dst); err == nil {
		t.Error("unpacked over an existing profile")
	}

	crafted := func(h *tar.Header, body string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		tw := tar.NewWriter(zw)
		h.Size = int64(len(body))
		tw.WriteHeader(h)
		tw.Write([]byte(body))
		tw.Close()
		zw.Close()
		return b.Bytes()
	}
	outside := filepath.Join(t.TempDir(), "outside")
	for name, h := range map[string]*tar.Header{
		"dot-dot":  {Typeflag: tar.TypeReg, Name: "../" + filepath.Base(outside), Mode: 0o600},
		"absolute": {Typeflag: tar.TypeReg, Name: outside, Mode: 0o600},
		"symlink":  {Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc", Mode: 0o777},
	} {
		d := filepath.Join(t.TempDir(), name)
		if _, err := readProfileArchive(bytes.NewReader(crafted(h, "x")), d); err == nil {
			t.Errorf("%s entry accepted", name)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("an archive wrote outside its profile")
	}
}

const castTestPage = `<!doctype html><title>Cast</title>
<button id="b" style="position:absolute;left:40px;top:40px;width:200px;height:80px"
  onclick="document.title='clicked'">Press</button>
<input id="i" style="position:absolute;left:40px;top:200px;width:300px">`

// The screencast view of a headless session: frames arrive, and the
// viewer's click, typing and paste reach the page.
func TestCastView(t *testing.T) {
	mgr := testChrome(t)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(castTestPage)) }))
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

	mgr.views = newViewTokens(time.Minute) // testChrome's manager sets no view TTL
	views := httptest.NewServer(newViewHandler(mgr, "/nonexistent"))
	defer views.Close()
	tok, _ := mgr.views.issue(s.meta.ID)
	resp, err := http.Get(views.URL + "/view/" + tok + "/cast")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "img-src data:") {
		t.Fatalf("cast page: %d %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if resp, err := http.Get(views.URL + "/view/nope/cast"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a bad token got %d", resp.StatusCode)
		}
	}

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(views.URL, "http")+"/view/"+tok+"/cast.ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(16 << 20)
	var frameW float64
	var sawTabs bool
	for frameW == 0 || !sawTabs {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for a frame and the tab list: %v", err)
		}
		var m struct {
			T    string    `json:"t"`
			W    float64   `json:"w"`
			Data string    `json:"data"`
			Tabs []castTab `json:"tabs"`
		}
		json.Unmarshal(data, &m)
		switch m.T {
		case "frame":
			if m.Data != "" {
				frameW = m.W
			}
		case "tabs":
			sawTabs = len(m.Tabs) == 1 && m.Tabs[0].Current && m.Tabs[0].Title == "Cast"
		}
	}
	if s.viewers.Load() != 1 {
		t.Errorf("viewers = %d while watching", s.viewers.Load())
	}
	send := func(m castIn) {
		b, _ := json.Marshal(m)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	// Click the button, then the field; type into it and paste after that.
	for _, p := range [][2]float64{{140, 80}, {100, 210}} {
		send(castIn{T: "mouse", Type: "mousePressed", X: p[0], Y: p[1], Button: "left", Buttons: 1, ClickCount: 1})
		send(castIn{T: "mouse", Type: "mouseReleased", X: p[0], Y: p[1], Button: "left", ClickCount: 1})
	}
	for _, ch := range "hi" {
		send(castIn{T: "key", Type: "keyDown", Key: string(ch), Code: "Key" + strings.ToUpper(string(ch)), Text: string(ch)})
		send(castIn{T: "key", Type: "keyUp", Key: string(ch), Code: "Key" + strings.ToUpper(string(ch))})
	}
	send(castIn{T: "paste", Text: " there"})
	var got struct {
		Title string `json:"title"`
		Value string `json:"value"`
	}
	for i := 0; i < 50; i++ {
		mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
			return evalJSON(ctx, tb, "({title: document.title, value: document.getElementById('i').value})", &got)
		})
		if got.Title == "clicked" && got.Value == "hi there" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Title != "clicked" || got.Value != "hi there" {
		t.Errorf("after the viewer's click, typing and paste: title %q, field %q", got.Title, got.Value)
	}
	c.Close(websocket.StatusNormalClosure, "")
	for i := 0; i < 50 && s.viewers.Load() != 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if s.viewers.Load() != 0 {
		t.Errorf("viewers = %d after the viewer left", s.viewers.Load())
	}
}
