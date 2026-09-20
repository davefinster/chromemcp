package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckFileName(t *testing.T) {
	ok := []string{"a", "photo.jpg", "My Report (final).pdf", "rows_2026-09-20.csv", "a+b.txt", "9lives.png"}
	for _, n := range ok {
		if err := checkFileName(n); err != nil {
			t.Errorf("checkFileName(%q) = %v, want ok", n, err)
		}
	}
	bad := []string{"", ".", "..", ".hidden", "../escape", "dir/file.txt", `back\slash.txt`, "nul\x00.txt",
		"a" + strings.Repeat("b", 128), ".incoming", "-leading.txt", "tab\there.txt"}
	for _, n := range bad {
		if err := checkFileName(n); err == nil {
			t.Errorf("checkFileName(%q) = nil, want an error", n)
		}
	}
}

func TestDecodeFileContent(t *testing.T) {
	want := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	std := base64.StdEncoding.EncodeToString(want)
	for _, in := range []string{
		std,
		strings.TrimRight(std, "="),             // unpadded
		std[:4] + "\n" + std[4:],                // wrapped, as a model writes it
		"  " + std + "\n",                       // surrounded by whitespace
		base64.URLEncoding.EncodeToString(want), // url-safe alphabet
		"data:application/octet-stream;base64," + std, // a data: URL
	} {
		got, err := decodeFileContent(in)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("decodeFileContent(%q) = %x, %v", in, got, err)
		}
	}
	for _, in := range []string{"not base64 at all!!", "data:text/plain,hello", "data:text/plain;base64"} {
		if _, err := decodeFileContent(in); err == nil {
			t.Errorf("decodeFileContent(%q) = nil error, want one", in)
		}
	}
}

func TestFileMIME(t *testing.T) {
	for name, want := range map[string]string{
		"a.png": "image/png", "a.PNG": "image/png", "a.csv": "text/csv",
		"a.docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"README": "", "a.zzz": "",
	} {
		if got := fileMIME(name); got != want {
			t.Errorf("fileMIME(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAcceptsFile(t *testing.T) {
	tests := []struct {
		accept, name string
		want         bool
	}{
		{"", "anything.bin", true},
		{"image/*", "photo.jpg", true},
		{"image/*", "notes.txt", false},
		{".pdf,.docx", "resume.PDF", true},
		{".pdf,.docx", "resume.odt", false},
		{"text/csv", "rows.csv", true},
		{"text/csv", "rows.tsv", false},
		{"image/png, image/jpeg", "photo.jpeg", true},
		{"*/*", "whatever.bin", true},
		{"image/*", "unknown.zzz", false},
	}
	for _, tc := range tests {
		if got := acceptsFile(tc.accept, tc.name); got != tc.want {
			t.Errorf("acceptsFile(%q, %q) = %v, want %v", tc.accept, tc.name, got, tc.want)
		}
	}
}

// testSession is a session record with a directory but no Chrome: enough
// for the file store, which never touches the browser.
func testSession(t *testing.T) *session {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s-abcd1234")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &session{dir: dir, meta: sessionMeta{ID: "s-abcd1234"}}
}

func TestSessionFileStore(t *testing.T) {
	s := testSession(t)

	if files, err := s.listFiles(); err != nil || len(files) != 0 {
		t.Fatalf("empty session: %v %v", files, err)
	}
	if _, err := s.filePaths([]string{"nothing.txt"}); err == nil || !strings.Contains(err.Error(), "nothing.txt") {
		t.Errorf("filePaths on an empty session: %v", err)
	}

	f, err := s.putFile("rows.csv", []byte("a,b\n1,2\n"), false)
	if err != nil || f.Size != 8 || f.MIME != "text/csv" {
		t.Fatalf("putFile: %+v %v", f, err)
	}
	if _, err := s.putFile("rows.csv", []byte("x"), false); err == nil || !strings.Contains(err.Error(), "overwrite=true") {
		t.Errorf("second put without overwrite: %v", err)
	}
	if _, err := s.putFile("rows.csv", []byte("x"), true); err != nil {
		t.Errorf("overwrite: %v", err)
	}
	if _, err := s.putFile("bad/name.csv", []byte("x"), false); err == nil {
		t.Error("a name with a separator was accepted")
	}
	if _, err := s.putFile("empty.txt", nil, false); err == nil {
		t.Error("an empty file was accepted")
	}
	if _, err := s.putFile("huge.bin", make([]byte, maxFileBytes+1), false); err == nil {
		t.Error("a file over the per-file limit was accepted")
	}

	if _, err := s.putFile("photo.jpg", []byte{0xff, 0xd8, 0xff}, false); err != nil {
		t.Fatal(err)
	}
	files, err := s.listFiles()
	if err != nil || len(files) != 2 || files[0].Name != "photo.jpg" || files[1].Name != "rows.csv" {
		t.Fatalf("listFiles: %+v %v", files, err)
	}
	if got := s.filesTotal(files); got != 4 {
		t.Errorf("filesTotal = %d, want 4", got)
	}

	// The write temporary is invisible, so a listing can never hand a page
	// a half-written file.
	if err := os.WriteFile(filepath.Join(s.filesDir(), ".incoming"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.listFiles(); len(files) != 2 {
		t.Errorf("listFiles saw the temporary: %+v", files)
	}

	paths, err := s.filePaths([]string{"rows.csv", "photo.jpg"})
	if err != nil || len(paths) != 2 {
		t.Fatalf("filePaths: %v %v", paths, err)
	}
	if !filepath.IsAbs(paths[0]) || filepath.Base(paths[0]) != "rows.csv" {
		t.Errorf("filePaths keeps order and gives Chrome absolute paths: %v", paths)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stat %s: %v", p, err)
		}
	}
	if _, err := s.filePaths([]string{"rows.csv", "gone.pdf"}); err == nil || !strings.Contains(err.Error(), "gone.pdf") {
		t.Errorf("filePaths naming a missing file: %v", err)
	}

	if err := s.deleteFile("rows.csv"); err != nil {
		t.Errorf("deleteFile: %v", err)
	}
	if err := s.deleteFile("rows.csv"); err == nil {
		t.Error("deleting twice did not fail")
	}
	if err := s.deleteFile("../../etc/passwd"); err == nil {
		t.Error("deleteFile accepted a path")
	}
	if files, _ := s.listFiles(); len(files) != 1 {
		t.Errorf("after delete: %+v", files)
	}
}

func TestSessionFileCounts(t *testing.T) {
	s := testSession(t)
	for i := 0; i < maxSessionFiles; i++ {
		if _, err := s.putFile(fmt.Sprintf("f%d.txt", i), []byte("x"), false); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := s.putFile("one-too-many.txt", []byte("x"), false); err == nil {
		t.Error("the file-count limit was not enforced")
	}
	// Replacing one of the files it already holds is not one more file.
	if _, err := s.putFile("f0.txt", []byte("yy"), true); err != nil {
		t.Errorf("overwrite at the count limit: %v", err)
	}
}

// A session's files are its own: deleting it takes them, and an identity
// saved from it never carries them into another session.
func TestSessionFilesFollowTheSession(t *testing.T) {
	mgr, err := newManager(&managerConfig{
		SessionsDir: filepath.Join(t.TempDir(), "s"), IdentitiesDir: filepath.Join(t.TempDir(), "i"),
		Chrome: "/nonexistent", ViewportW: 800, ViewportH: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{mgr: mgr, dir: filepath.Join(mgr.cfg.SessionsDir, "s-11112222"), meta: sessionMeta{ID: "s-11112222", Mode: modeHeadless}}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeProfile(t, s.profileDir())
	if err := s.saveMeta(); err != nil {
		t.Fatal(err)
	}
	mgr.sessions[s.meta.ID] = s
	if _, err := s.putFile("secret.pdf", []byte("%PDF-1.4 hello"), false); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.identities.save("someone", "", s.meta.ID, s.dir, false); err != nil {
		t.Fatal(err)
	}
	other := &session{mgr: mgr, dir: filepath.Join(mgr.cfg.SessionsDir, "s-33334444"), meta: sessionMeta{ID: "s-33334444"}}
	if err := mgr.identities.seed("someone", other.dir); err != nil {
		t.Fatal(err)
	}
	if files, _ := other.listFiles(); len(files) != 0 {
		t.Errorf("a session seeded from the identity inherited files: %+v", files)
	}

	dir := s.dir
	if err := mgr.remove(s.meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("deleting the session left %s behind: %v", dir, err)
	}
}

// uploadPage exercises the three shapes a site asks for a file in: a plain
// visible input, a hidden input behind a styled label, and a button that
// only builds its input when clicked.
const uploadPage = `<!doctype html><html><head><title>Upload</title></head><body>
<h1>Upload</h1>
<input type="file" id="plain" name="plain" accept=".csv,text/plain">
<label id="styled-label" for="hidden-input">Choose a photo</label>
<input type="file" id="hidden-input" name="photo" accept="image/*" multiple style="display:none">
<button id="lazy" onclick="lazyPick()">Attach</button>
<p id="out">nothing</p>
<script>
function report(what, input) {
  document.getElementById('out').textContent = what + ':' + Array.from(input.files).map(f => f.name + '@' + f.size).join(',');
}
document.getElementById('plain').addEventListener('change', e => report('plain', e.target));
document.getElementById('hidden-input').addEventListener('change', e => report('photo', e.target));
function lazyPick() {
  const i = document.createElement('input');
  i.type = 'file';
  i.addEventListener('change', () => report('lazy', i));
  i.click();
}
</script>
</body></html>`

func TestUploadIntegration(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(uploadPage))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s, err := mgr.start(ctx, startOptions{Label: "upload"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := s.meta.ID
	if _, err := s.putFile("rows.csv", []byte("a,b\n1,2\n"), false); err != nil {
		t.Fatal(err)
	}
	// A one-pixel PNG, so the page gets something a real image type.
	png, _ := decodeFileContent("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if _, err := s.putFile("pixel.png", png, false); err != nil {
		t.Fatal(err)
	}

	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil {
			return err
		}
		paths, err := s.filePaths([]string{"rows.csv"})
		if err != nil {
			return err
		}

		// 1. The visible input, by selector.
		info, err := findFileTarget(ctx, tb, targetSpec{Selector: "#plain"})
		if err != nil {
			return err
		}
		if info.Mode != "input" || info.Multiple || info.Hidden || info.Accept != ".csv,text/plain" {
			t.Errorf("plain input: %+v", info)
		}
		if err := setFilesOnElement(ctx, tb, paths); err != nil {
			return fmt.Errorf("setting the plain input: %w", err)
		}
		var out string
		evalJSON(ctx, tb, "document.getElementById('out').textContent", &out)
		if out != "plain:rows.csv@8" {
			t.Errorf("after the plain upload, out=%q — the change event did not carry the file", out)
		}
		if have := fileInputContents(ctx, tb); len(have) != 1 || !strings.Contains(have[0], "rows.csv") {
			t.Errorf("read-back: %v", have)
		}

		// 2. The hidden input behind its label, two files at once, found
		//    through the label a person would actually click.
		info, err = findFileTarget(ctx, tb, targetSpec{Text: "Choose a photo"})
		if err != nil {
			return err
		}
		if info.Mode != "input" || !info.Multiple || !info.Hidden {
			t.Errorf("hidden input via its label: %+v", info)
		}
		both, err := s.filePaths([]string{"pixel.png", "rows.csv"})
		if err != nil {
			return err
		}
		if err := setFilesOnElement(ctx, tb, both); err != nil {
			return fmt.Errorf("setting the hidden input: %w", err)
		}
		evalJSON(ctx, tb, "document.getElementById('out').textContent", &out)
		if !strings.HasPrefix(out, "photo:pixel.png@") || !strings.Contains(out, "rows.csv@8") {
			t.Errorf("after the multi-file upload, out=%q", out)
		}
		// accept="image/*" covers the png and not the csv, which is what
		// the warning in browser_upload is built on.
		if !acceptsFile(info.Accept, "pixel.png") || acceptsFile(info.Accept, "rows.csv") {
			t.Errorf("accept %q misread", info.Accept)
		}

		// 3. The button that builds its input on click: no input to find,
		//    so the chooser it opens is intercepted.
		info, err = findFileTarget(ctx, tb, targetSpec{Selector: "#lazy"})
		if err != nil {
			return err
		}
		if info.Mode != "chooser" {
			t.Fatalf("lazy button: %+v, want mode chooser", info)
		}
		res, err := resolveTarget(ctx, tb, targetSpec{Selector: "#lazy"})
		if err != nil {
			return err
		}
		if err := setFilesViaChooser(ctx, tb, paths, func() error {
			return clickAt(ctx, tb, res.X, res.Y, "left", 1, 0)
		}); err != nil {
			return fmt.Errorf("via the chooser: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		evalJSON(ctx, tb, "document.getElementById('out').textContent", &out)
		if out != "lazy:rows.csv@8" {
			t.Errorf("after the intercepted chooser, out=%q", out)
		}
		if have := fileInputContents(ctx, tb); len(have) != 1 || !strings.Contains(have[0], "rows.csv") {
			t.Errorf("read-back after the chooser: %v", have)
		}
		// With no target there are several inputs, so it says so rather
		// than guessing.
		if _, err := findFileTarget(ctx, tb, targetSpec{}); err == nil || !strings.Contains(err.Error(), "file inputs") {
			t.Errorf("ambiguous page: %v", err)
		}
		// A page with exactly one input needs no target at all.
		if err := navigate(ctx, tb, "data:text/html,<input type=file id=only>", 10*time.Second); err != nil {
			return err
		}
		info, err = findFileTarget(ctx, tb, targetSpec{})
		if err != nil || info.Mode != "input" || info.Selector != "input#only" {
			t.Errorf("single input, no target: %+v %v", info, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUploadTools drives the same thing the agent does: file_put, then
// browser_upload, over the MCP wire.
func TestUploadTools(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(uploadPage))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	server := newMCPServer(&mcpApp{mgr: mgr}, nil)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}

	s, err := mgr.start(ctx, startOptions{Label: "upload tools"})
	if err != nil {
		t.Fatal(err)
	}
	sid := s.meta.ID

	// Text goes in as text; bytes as base64.
	if out, isErr := call("file_put", map[string]any{"session_id": sid, "name": "rows.csv", "text": "a,b\n1,2\n"}); isErr || !strings.Contains(out, "rows.csv") {
		t.Fatalf("file_put text: %q (error=%v)", out, isErr)
	}
	if out, isErr := call("file_put", map[string]any{"session_id": sid, "name": "rows.csv", "text": "x"}); !isErr || !strings.Contains(out, "overwrite") {
		t.Errorf("file_put over an existing name: %q (error=%v)", out, isErr)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 hello"))
	if out, isErr := call("file_put", map[string]any{"session_id": sid, "name": "notes.pdf", "content": b64}); isErr || !strings.Contains(out, "application/pdf") {
		t.Errorf("file_put base64: %q (error=%v)", out, isErr)
	}
	if out, isErr := call("file_list", map[string]any{"session_id": sid}); isErr || !strings.Contains(out, "rows.csv") || !strings.Contains(out, "notes.pdf") {
		t.Errorf("file_list: %q (error=%v)", out, isErr)
	}

	if out, isErr := call("browser_navigate", map[string]any{"session_id": sid, "url": srv.URL, "screenshot": false}); isErr {
		t.Fatalf("navigate: %q", out)
	}
	out, isErr := call("browser_upload", map[string]any{
		"session_id": sid, "selector": "#plain", "files": []any{"rows.csv"}, "screenshot": false,
	})
	if isErr || !strings.Contains(out, "rows.csv (8 bytes") {
		t.Errorf("browser_upload: %q (error=%v)", out, isErr)
	}
	// The site's own change handler saw the file.
	var page string
	mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		return evalJSON(ctx, tb, "document.getElementById('out').textContent", &page)
	})
	if page != "plain:rows.csv@8" {
		t.Errorf("the page saw %q", page)
	}
	// accept=".csv,text/plain" does not cover a pdf: said, not silently ignored.
	if out, isErr := call("browser_upload", map[string]any{
		"session_id": sid, "selector": "#plain", "files": []any{"notes.pdf"}, "screenshot": false,
	}); isErr || !strings.Contains(out, "does not cover notes.pdf") {
		t.Errorf("accept warning: %q (error=%v)", out, isErr)
	}
	// One file at a time into an input with no multiple.
	if out, isErr := call("browser_upload", map[string]any{
		"session_id": sid, "selector": "#plain", "files": []any{"rows.csv", "notes.pdf"}, "screenshot": false,
	}); !isErr || !strings.Contains(out, "one file at a time") {
		t.Errorf("multiple into a single input: %q (error=%v)", out, isErr)
	}
	// A name the session does not hold names what it does.
	if out, isErr := call("browser_upload", map[string]any{
		"session_id": sid, "files": []any{"nope.txt"}, "screenshot": false,
	}); !isErr || !strings.Contains(out, "nope.txt") || !strings.Contains(out, "rows.csv") {
		t.Errorf("unknown file: %q (error=%v)", out, isErr)
	}

	if out, isErr := call("file_delete", map[string]any{"session_id": sid, "name": "notes.pdf"}); isErr || !strings.Contains(out, "deleted") {
		t.Errorf("file_delete: %q (error=%v)", out, isErr)
	}
	if out, _ := call("file_list", map[string]any{"session_id": sid}); strings.Contains(out, "notes.pdf") {
		t.Errorf("file_list after delete: %q", out)
	}

	// Deleting the session takes the files with it.
	dir := s.dir
	if out, isErr := call("session_delete", map[string]any{"session_id": sid}); isErr {
		t.Fatalf("session_delete: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "files")); !os.IsNotExist(err) {
		t.Errorf("the session's files outlived it: %v", err)
	}
}
