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

	f, err := s.putFile("rows.csv", []byte("a,b\n1,2\n"), putCreate)
	if err != nil || f.Size != 8 || f.MIME != "text/csv" {
		t.Fatalf("putFile: %+v %v", f, err)
	}
	if _, err := s.putFile("rows.csv", []byte("x"), putCreate); err == nil || !strings.Contains(err.Error(), "overwrite=true") {
		t.Errorf("second put without overwrite: %v", err)
	}
	if _, err := s.putFile("rows.csv", []byte("x"), putOverwrite); err != nil {
		t.Errorf("overwrite: %v", err)
	}
	if _, err := s.putFile("bad/name.csv", []byte("x"), putCreate); err == nil {
		t.Error("a name with a separator was accepted")
	}
	if _, err := s.putFile("empty.txt", nil, putCreate); err == nil {
		t.Error("an empty file was accepted")
	}
	if _, err := s.putFile("huge.bin", make([]byte, maxFileBytes+1), putCreate); err == nil {
		t.Error("a file over the per-file limit was accepted")
	}

	if _, err := s.putFile("photo.jpg", []byte{0xff, 0xd8, 0xff}, putCreate); err != nil {
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
		if _, err := s.putFile(fmt.Sprintf("f%d.txt", i), []byte("x"), putCreate); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := s.putFile("one-too-many.txt", []byte("x"), putCreate); err == nil {
		t.Error("the file-count limit was not enforced")
	}
	// Replacing one of the files it already holds is not one more file.
	if _, err := s.putFile("f0.txt", []byte("yy"), putOverwrite); err != nil {
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
	if _, err := s.putFile("secret.pdf", []byte("%PDF-1.4 hello"), putCreate); err != nil {
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
	if _, err := s.putFile("rows.csv", []byte("a,b\n1,2\n"), putCreate); err != nil {
		t.Fatal(err)
	}
	// A one-pixel PNG, so the page gets something a real image type.
	png, _ := decodeFileContent("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if _, err := s.putFile("pixel.png", png, putCreate); err != nil {
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

// Appending is how a file too big for one tool call arrives: chunk by
// chunk onto the same name, complete only once the last one has landed.
func TestSessionFileAppend(t *testing.T) {
	s := testSession(t)

	// The first chunk creates the file: there is nothing to add to yet.
	if f, err := s.putFile("big.txt", []byte("one "), putAppend); err != nil || f.Size != 4 {
		t.Fatalf("first chunk: %+v %v", f, err)
	}
	f, err := s.putFile("big.txt", []byte("two "), putAppend)
	if err != nil || f.Size != 8 {
		t.Fatalf("second chunk: %+v %v", f, err)
	}
	if f, err := s.putFile("big.txt", []byte("three"), putAppend); err != nil || f.Size != 13 {
		t.Fatalf("third chunk: %+v %v", f, err)
	}
	got, err := os.ReadFile(filepath.Join(s.filesDir(), "big.txt"))
	if err != nil || string(got) != "one two three" {
		t.Fatalf("the chunks did not accumulate: %q %v", got, err)
	}
	// The count limit counts the file once, however many chunks built it.
	if files, _ := s.listFiles(); len(files) != 1 {
		t.Errorf("appending made %d files", len(files))
	}

	// A chunk that would pass the per-file limit leaves the file exactly
	// as long as it was, rather than half-written.
	if _, err := s.putFile("big.txt", make([]byte, maxFileBytes), putAppend); err == nil {
		t.Error("an append over the per-file limit was accepted")
	}
	if fi, err := os.Stat(filepath.Join(s.filesDir(), "big.txt")); err != nil || fi.Size() != 13 {
		t.Errorf("the refused chunk changed the file: %v %v", fi, err)
	}
	// And one that fails while creating leaves nothing behind at all.
	if _, err := s.putFile("toobig.bin", make([]byte, maxFileBytes+1), putAppend); err == nil {
		t.Error("an oversize first chunk was accepted")
	}
	if _, err := os.Stat(filepath.Join(s.filesDir(), "toobig.bin")); !os.IsNotExist(err) {
		t.Errorf("a refused first chunk left the file behind: %v", err)
	}
	if _, err := s.putFile("empty.txt", nil, putAppend); err == nil {
		t.Error("an empty chunk was accepted")
	}
}

// What Chrome downloads is a session file like any other — the whole point
// being that those bytes never pass through an agent's context.
func TestSessionDownloadsAreFiles(t *testing.T) {
	s := testSession(t)
	if err := os.MkdirAll(s.downloadsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(s.downloadsDir(), "invoice.pdf", "%PDF-1.4 downloaded")
	write(s.downloadsDir(), "big.zip.crdownload", "half of it")
	write(s.downloadsDir(), ".com.google.Chrome.tmp", "chrome's own")
	if _, err := s.putFile("notes.txt", []byte("mine"), putCreate); err != nil {
		t.Fatal(err)
	}

	files, err := s.listFiles()
	if err != nil || len(files) != 3 {
		t.Fatalf("listFiles: %+v %v", files, err)
	}
	byName := map[string]sessionFile{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if f := byName["invoice.pdf"]; !f.Downloaded || f.Partial || f.MIME != "application/pdf" {
		t.Errorf("the download: %+v", f)
	}
	if f := byName["notes.txt"]; f.Downloaded {
		t.Errorf("the put file came back as a download: %+v", f)
	}
	// A download still arriving is listed under the name it will have, and
	// is not handed to a page half-written.
	if f := byName["big.zip"]; !f.Partial {
		t.Errorf("the part file: %+v", f)
	}
	if _, err := s.filePaths([]string{"big.zip"}); err == nil || !strings.Contains(err.Error(), "still downloading") {
		t.Errorf("filePaths on a part file: %v", err)
	}

	// Downloads are Chrome's, not the agent's, so they are not metered.
	put, _ := s.listPutFiles()
	if got := s.filesTotal(put); got != 4 {
		t.Errorf("filesTotal over put files = %d, want 4", got)
	}

	// browser_upload reaches a download by name, from the downloads dir.
	paths, err := s.filePaths([]string{"invoice.pdf", "notes.txt"})
	if err != nil || len(paths) != 2 {
		t.Fatalf("filePaths: %v %v", paths, err)
	}
	if filepath.Dir(paths[0]) != mustAbs(t, s.downloadsDir()) || filepath.Dir(paths[1]) != mustAbs(t, s.filesDir()) {
		t.Errorf("paths came from the wrong directories: %v", paths)
	}

	// Once the download finishes, the part file no longer doubles it.
	write(s.downloadsDir(), "big.zip", "all of it")
	files, _ = s.listFiles()
	if len(files) != 3 {
		t.Errorf("the finished download and its part file were both listed: %+v", files)
	}
	if f := findFile(files, "big.zip"); f == nil || f.Partial {
		t.Errorf("after finishing: %+v", f)
	}

	// A put file of the same name wins, and the listing says the download
	// is there rather than letting it vanish.
	if _, err := s.putFile("invoice.pdf", []byte("mine, not theirs"), putCreate); err != nil {
		t.Fatal(err)
	}
	files, _ = s.listFiles()
	if f := findFile(files, "invoice.pdf"); f == nil || f.Downloaded {
		t.Errorf("the put file did not win: %+v", f)
	}
	if shadowed := shadowedDownloads(s, mustPut(t, s)); len(shadowed) != 1 || shadowed[0] != "invoice.pdf" {
		t.Errorf("shadowedDownloads = %v", shadowed)
	}

	// Deleting reaches either directory, and only through the listing, so
	// a name that is not in it cannot name a path.
	if err := s.deleteFile("big.zip"); err != nil {
		t.Errorf("deleting a download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.downloadsDir(), "big.zip")); !os.IsNotExist(err) {
		t.Error("the download survived file_delete")
	}
	if err := s.deleteFile("../../etc/passwd"); err == nil {
		t.Error("deleteFile accepted a path")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func mustPut(t *testing.T, s *session) []sessionFile {
	t.Helper()
	put, err := s.listPutFiles()
	if err != nil {
		t.Fatal(err)
	}
	return put
}

func findFile(files []sessionFile, name string) *sessionFile {
	for i, f := range files {
		if f.Name == name {
			return &files[i]
		}
	}
	return nil
}
