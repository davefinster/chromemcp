package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadTokens(t *testing.T) {
	u := newUploadTokens(50 * time.Millisecond)
	tok, _ := u.issue("s-1", "")
	if len(tok) != 48 {
		t.Errorf("token %q", tok)
	}
	got, ok := u.lookup(tok)
	if !ok || got.session != "s-1" || got.name != "" {
		t.Errorf("lookup = %+v %v", got, ok)
	}
	if _, ok := u.lookup("nope"); ok {
		t.Error("unknown token accepted")
	}
	pinned, _ := u.issue("s-1", "resume.pdf")
	if got, _ := u.lookup(pinned); got.name != "resume.pdf" {
		t.Errorf("pinned token = %+v", got)
	}
	tok2, _ := u.issue("s-2", "")
	u.revokeSession("s-2")
	if _, ok := u.lookup(tok2); ok {
		t.Error("revoked token accepted")
	}
	if _, ok := u.lookup(tok); !ok {
		t.Error("revoking one session took another session's token")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := u.lookup(tok); ok {
		t.Error("expired token accepted")
	}
}

// uploadTestSession is a manager holding one session, which is all the
// upload handler needs: no Chrome is involved in taking a file.
func uploadTestSession(t *testing.T) (*manager, *session) {
	t.Helper()
	mgr, err := newManager(&managerConfig{
		SessionsDir: filepath.Join(t.TempDir(), "s"), IdentitiesDir: filepath.Join(t.TempDir(), "i"),
		Chrome: "/nonexistent", ViewportW: 800, ViewportH: 600, UploadTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{mgr: mgr, dir: filepath.Join(mgr.cfg.SessionsDir, "s-55556666"), meta: sessionMeta{ID: "s-55556666", Mode: modeHeadless}}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mgr.sessions[s.meta.ID] = s
	return mgr, s
}

func TestUploadHandlerHTTP(t *testing.T) {
	mgr, s := uploadTestSession(t)
	h := &uploadHandler{mgr: mgr}
	tok, _ := mgr.uploads.issue(s.meta.ID, "")

	do := func(method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req := httptest.NewRequest(method, path, r)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	for _, tt := range []struct {
		what   string
		method string
		path   string
		code   int
	}{
		{"no token", "GET", "/upload/", 404},
		{"a token nobody issued", "GET", "/upload/badtoken/", 403},
		{"the page", "GET", "/upload/" + tok + "/", 200},
		{"without the trailing slash", "GET", "/upload/" + tok, 302},
		{"a method it does not take", "DELETE", "/upload/" + tok + "/x.txt", 405},
		{"a PUT with no name to give the file", "PUT", "/upload/" + tok, 400},
		{"a name no file may have", "PUT", "/upload/" + tok + "/-dash.txt", 400},
	} {
		w := do(tt.method, tt.path, []byte("x"), nil)
		if w.Code != tt.code {
			t.Errorf("%s: got %d, want %d (%s)", tt.what, w.Code, tt.code, strings.TrimSpace(w.Body.String()))
		}
	}

	// The page is self-contained, says which session it feeds, and may not
	// pull anything in from anywhere else.
	w := do("GET", "/upload/"+tok+"/", nil, nil)
	if body := w.Body.String(); !strings.Contains(body, s.meta.ID) || !strings.Contains(body, "Drop files here") {
		t.Errorf("the drop page: %q", body[:min(len(body), 200)])
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "nonce-") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a token URL was made cacheable: %v", w.Header())
	}

	// A name with a path in it — which is what some browsers put in a form
	// part — keeps only its last element, and so lands in the session's
	// files directory like any other.
	if w := do("PUT", "/upload/"+tok+"/../escape.txt", []byte("x"), nil); w.Code != 200 {
		t.Errorf("a path-shaped name: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if _, err := os.Stat(filepath.Join(s.filesDir(), "escape.txt")); err != nil {
		t.Errorf("the sanitized name did not land in the files directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "escape.txt")); !os.IsNotExist(err) {
		t.Error("a file escaped the files directory")
	}
	if err := s.deleteFile("escape.txt"); err != nil {
		t.Fatal(err)
	}

	// A plain PUT is the whole of what curl -T does.
	if w := do("PUT", "/upload/"+tok+"/report.pdf", []byte("%PDF-1.4 sent over the link"), nil); w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	files, err := s.listFiles()
	if err != nil || len(files) != 1 || files[0].Name != "report.pdf" || files[0].Size != 27 {
		t.Fatalf("after the PUT: %+v %v", files, err)
	}
	// Sent twice, it is there once: a transfer that has to be repeated
	// should not have to be renamed.
	if w := do("PUT", "/upload/"+tok+"/report.pdf", []byte("%PDF-1.4 again"), nil); w.Code != 200 {
		t.Errorf("the second PUT: %d %s", w.Code, w.Body.String())
	}
	if files, _ := s.listFiles(); len(files) != 1 || files[0].Size != 14 {
		t.Errorf("resending did not replace: %+v", files)
	}

	// The drop page asks for JSON, and reads the error out of it.
	w = do("PUT", "/upload/"+tok+"/rows.csv", []byte("a,b\n1,2\n"), map[string]string{"Accept": "application/json"})
	var res struct {
		OK    bool `json:"ok"`
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Type string `json:"type"`
		} `json:"files"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil || !res.OK || len(res.Files) != 1 {
		t.Fatalf("JSON reply: %+v %v", res, err)
	}
	if f := res.Files[0]; f.Name != "rows.csv" || f.Size != 8 || f.Type != "text/csv" {
		t.Errorf("JSON reply file: %+v", f)
	}

	// A form post, which is both the drop page's fallback and curl -F.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"one.txt", "first"}, {"two.txt", "second"}} {
		part, _ := mw.CreateFormFile("file", f.name)
		part.Write([]byte(f.body))
	}
	mw.WriteField("comment", "not a file")
	mw.Close()
	if w := do("POST", "/upload/"+tok+"/", buf.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()}); w.Code != 200 {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	if files, _ := s.listFiles(); len(files) != 4 {
		t.Errorf("after the form post: %+v", files)
	}

	// Over the per-file limit: refused, and nothing left behind.
	w = do("PUT", "/upload/"+tok+"/huge.bin", make([]byte, maxFileBytes+1), nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "limit") {
		t.Errorf("an oversize PUT: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if _, err := os.Stat(filepath.Join(s.filesDir(), "huge.bin")); !os.IsNotExist(err) {
		t.Errorf("the refused upload left a file behind: %v", err)
	}

	// A pinned link writes that name and no other.
	pinned, _ := mgr.uploads.issue(s.meta.ID, "resume.pdf")
	if w := do("PUT", "/upload/"+pinned+"/resume.pdf", []byte("%PDF-1.4 cv"), nil); w.Code != 200 {
		t.Errorf("the pinned name: %d %s", w.Code, w.Body.String())
	}
	w = do("PUT", "/upload/"+pinned+"/something-else.pdf", []byte("nope"), nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "resume.pdf") {
		t.Errorf("a pinned link took another name: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	// A pinned link needs no name in the path at all.
	if w := do("PUT", "/upload/"+pinned, []byte("%PDF-1.4 cv again"), nil); w.Code != 200 {
		t.Errorf("the pinned link without a path: %d %s", w.Code, w.Body.String())
	}

	// Deleting the session takes its links with it.
	if err := mgr.remove(s.meta.ID); err != nil {
		t.Fatal(err)
	}
	if w := do("PUT", "/upload/"+tok+"/after.txt", []byte("x"), nil); w.Code != 403 {
		t.Errorf("a link outlived its session: %d", w.Code)
	}
}

// The upload link and the live view are separate authorities: neither
// token opens the other's door.
func TestUploadAndViewTokensAreSeparate(t *testing.T) {
	mgr, s := uploadTestSession(t)
	mgr.views = newViewTokens(time.Minute)
	viewTok, _ := mgr.views.issue(s.meta.ID)
	uploadTok, _ := mgr.uploads.issue(s.meta.ID, "")

	uh := &uploadHandler{mgr: mgr}
	w := httptest.NewRecorder()
	uh.ServeHTTP(w, httptest.NewRequest("PUT", "/upload/"+viewTok+"/x.txt", strings.NewReader("x")))
	if w.Code != http.StatusForbidden {
		t.Errorf("a view token uploaded a file: %d", w.Code)
	}
	if _, ok := mgr.views.lookup(uploadTok); ok {
		t.Error("an upload token opened a live view")
	}
}

// The drop page is the half of this that a person uses, and its JavaScript
// only ever runs in a browser — so drive it in one: choose a file the way
// the page's own file picker does, and see it land on the session.
func TestDropPageInChrome(t *testing.T) {
	mgr := testChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Label: "drop"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	srv := httptest.NewServer(&uploadHandler{mgr: mgr})
	defer srv.Close()
	tok, _ := mgr.uploads.issue(s.meta.ID, "")

	// On disk, not on the session: this is a file from the sender's own
	// machine, which is the case the link exists for.
	src := filepath.Join(t.TempDir(), "rows.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n3,4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL+"/upload/"+tok+"/", 20*time.Second); err != nil {
			return err
		}
		var title string
		if err := evalJSON(ctx, tb, "document.title", &title); err != nil {
			return err
		}
		if title != "Send a file to this browser" {
			t.Errorf("the page did not render: title %q", title)
		}
		// The page's own hidden input, reached exactly as browser_upload
		// reaches one behind a styled button.
		if _, err := findFileTarget(ctx, tb, targetSpec{Selector: "#pick"}); err != nil {
			return err
		}
		if err := setFilesOnElement(ctx, tb, []string{src}); err != nil {
			return err
		}
		// Its XHR reports into the list; wait for it to say so.
		deadline := time.Now().Add(20 * time.Second)
		var status string
		for time.Now().Before(deadline) {
			if err := evalJSON(ctx, tb, "(document.querySelector('#list li .st')||{}).textContent || ''", &status); err != nil {
				return err
			}
			if status == "sent" {
				break
			}
			if strings.Contains(status, "fail") || strings.Contains(status, "limit") {
				t.Fatalf("the page reported %q", status)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if status != "sent" {
			t.Fatalf("the upload never finished; the page says %q", status)
		}
		// Nothing may have been refused on the way: the page's own style
		// and script are inline under a nonce, and a CSP that got that
		// wrong reports it here and nowhere else.
		if c := tb.consoleText(false, "error"); strings.Contains(c, "Content Security Policy") || strings.Contains(c, "SyntaxError") {
			t.Errorf("the page reported: %s", c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := s.listFiles()
	if err != nil || len(files) != 1 || files[0].Name != "rows.csv" || files[0].Size != 12 {
		t.Fatalf("after the drop: %+v %v", files, err)
	}
	// And it is a file like any other, ready for a page's file picker.
	if paths, err := s.filePaths([]string{"rows.csv"}); err != nil || len(paths) != 1 {
		t.Errorf("filePaths: %v %v", paths, err)
	}
}
