package main

// Session files: bytes a page can be given through its file picker — an
// attachment, a photo, a CSV to import. They reach a session three ways:
// file_put carries them in the tool call, the upload link (upload.go)
// takes them over plain HTTP from whatever holds them, and Chrome puts
// whatever the session downloads there by itself.
//
// They live in the session directory beside the profile, so their
// lifecycle is the session's exactly: kept while it is parked, deleted
// with it (manager.remove removes the whole directory) and reaped with it.
// identity_save copies only the profile and the cookie jar, so a file
// never follows an identity into another session.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxFileBytes        = 20 << 20  // one file, decoded
	maxSessionFileBytes = 100 << 20 // everything the agent may give one session
	maxSessionFiles     = 50
)

// fileNameRe is what a name may look like: a plain file name as a person
// would type it into a Save dialog. It starts with a letter or digit, so
// no dotfile and no ".."; it has no separator, so nothing escapes the
// session's files directory; and everything this server writes there
// itself (the upload temporary) starts with a dot and is therefore
// invisible to it.
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+-]{0,127}$`)

func checkFileName(name string) error {
	if !fileNameRe.MatchString(name) {
		return fmt.Errorf("file name %q: a plain name of up to 128 letters, digits, spaces, '.', '_', '-', '(', ')' or '+', "+
			"starting with a letter or digit — no directories", name)
	}
	return nil
}

// sessionFile is one file a session holds, from either directory.
type sessionFile struct {
	Name       string
	Size       int64
	Modified   time.Time
	MIME       string // what the extension says; Chrome decides the File.type the same way
	Downloaded bool   // Chrome downloaded it into the session, rather than the agent putting it there
	Partial    bool   // a download still arriving; Size is what has landed so far
	path       string // absolute, which is what Chrome is given
}

func (f sessionFile) String() string {
	s := fmt.Sprintf("%s  %s", f.Name, humanBytes(f.Size))
	if f.MIME != "" {
		s += ", " + f.MIME
	}
	return s
}

// mimeSupplement covers the everyday upload types Go's built-in table
// misses, so a listing says what a site will make of a name even where the
// image has no /etc/mime.types.
var mimeSupplement = map[string]string{
	".txt": "text/plain", ".csv": "text/csv", ".tsv": "text/tab-separated-values",
	".md": "text/markdown", ".rtf": "application/rtf", ".ics": "text/calendar",
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
	".doc": "application/msword", ".xls": "application/vnd.ms-excel", ".ppt": "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".bmp":  "image/bmp", ".ico": "image/x-icon", ".tif": "image/tiff", ".tiff": "image/tiff",
	".heic": "image/heic", ".avif": "image/avif",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
}

// fileMIME is the type a site will infer from the name. It is advisory:
// the File object the page receives is typed by Chrome from the same
// extension, not by anything this server declares.
func fileMIME(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	t := mime.TypeByExtension(ext)
	if t == "" {
		t = mimeSupplement[ext]
	}
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// decodeFileContent turns what an agent sent into bytes: base64 in any of
// its spellings, wrapped or not, and the data: URL shape a model reaches
// for when it has an image to hand.
func decodeFileContent(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		i := strings.IndexByte(s, ',')
		if i < 0 {
			return nil, errors.New("content looks like a data: URL but has no comma")
		}
		if !strings.Contains(s[:i], ";base64") {
			return nil, errors.New("content is a data: URL that is not base64-encoded; send the bytes as base64, or the text as text")
		}
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("content is not valid base64 (send the raw bytes base64-encoded, or plain text as text)")
}

// ---- the store: two directories per session ----
//
// files/     what the agent has given the session — file_put, or the
//            upload link — metered against the limits above.
// downloads/ what Chrome downloaded while the session browsed (session.go
//            points the browser's download path here). Not metered: the
//            browser wrote them, not the agent, and a site that offers a
//            download is the cheapest way there is to get bytes onto a
//            session, since none of them pass through the agent's context.
//
// A listing merges the two, because to the page being handed a file there
// is no difference between them. A name held on both sides resolves to the
// put file: the agent chose that name deliberately.

// crdownload is the suffix Chrome gives a download until it finishes.
const crdownload = ".crdownload"

// listPutFiles is what the agent has given the session.
func (s *session) listPutFiles() ([]sessionFile, error) { return listFileDir(s.filesDir(), false) }

// listDownloads is what Chrome has downloaded into the session, the ones
// still arriving included.
func (s *session) listDownloads() ([]sessionFile, error) { return listFileDir(s.downloadsDir(), true) }

// listFiles is everything the session could hand a page, both kinds at
// once, sorted by name.
func (s *session) listFiles() ([]sessionFile, error) {
	put, err := s.listPutFiles()
	if err != nil {
		return nil, err
	}
	downloads, err := s.listDownloads()
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(put))
	for _, f := range put {
		taken[f.Name] = true
	}
	out := put
	for _, f := range downloads {
		if taken[f.Name] {
			continue // shadowed by a put file; fileList says so
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// listFileDir reads one of the two directories. Names in downloads/ are
// Chrome's, so they are taken as they come rather than held to the names
// this server would allow itself: os.ReadDir yields one path element and
// never a separator or "..", and every caller that resolves a name
// resolves it against a listing instead of joining what it was handed.
func listFileDir(dir string, downloaded bool) ([]sessionFile, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []sessionFile
	partial := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// Hidden: this server's own ".incoming", and Chrome's temporaries.
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !downloaded && checkFileName(name) != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		f := sessionFile{
			Name: name, Size: fi.Size(), Modified: fi.ModTime(),
			Downloaded: downloaded, path: filepath.Join(abs, name),
		}
		if downloaded && strings.HasSuffix(name, crdownload) {
			f.Name = strings.TrimSuffix(name, crdownload)
			f.Partial = true
			partial[f.Name] = true
		}
		f.MIME = fileMIME(f.Name)
		out = append(out, f)
	}
	// A finished download and its own leftover part file would otherwise
	// appear twice under the one name.
	if len(partial) > 0 {
		kept := out[:0]
		for _, f := range out {
			if f.Partial && !onlyPartial(out, f.Name) {
				continue
			}
			kept = append(kept, f)
		}
		out = kept
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// onlyPartial reports whether nothing complete goes by that name.
func onlyPartial(files []sessionFile, name string) bool {
	for _, f := range files {
		if f.Name == name && !f.Partial {
			return false
		}
	}
	return true
}

func (s *session) filesTotal(files []sessionFile) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// putMode says what a write does about a file of that name the session
// already holds.
type putMode int

const (
	putCreate    putMode = iota // refuse to touch an existing file
	putOverwrite                // replace it
	putAppend                   // add to the end of it, creating it if absent
)

// putFile writes one file into the session.
func (s *session) putFile(name string, data []byte, mode putMode) (*sessionFile, error) {
	return s.putFileFrom(name, bytes.NewReader(data), mode)
}

// putFileFrom is the same from a stream, which is how the upload link
// arrives: the bytes reach disk as they come rather than being held in
// memory whole, and the limits are enforced against the stream, so an
// over-long body is cut off rather than swallowed first and judged after.
func (s *session) putFileFrom(name string, r io.Reader, mode putMode) (*sessionFile, error) {
	if err := checkFileName(name); err != nil {
		return nil, err
	}
	existing, err := s.listPutFiles()
	if err != nil {
		return nil, err
	}
	var other int64 // every put file that is not this one
	var cur *sessionFile
	for i, f := range existing {
		if f.Name == name {
			cur = &existing[i]
			continue
		}
		other += f.Size
	}
	if cur != nil && mode == putCreate {
		return nil, fmt.Errorf("session %s already has a file %q (%s, put %s); pass overwrite=true to replace it, "+
			"or append=true to add to the end of it", s.meta.ID, name, humanBytes(cur.Size), cur.Modified.Format("15:04:05"))
	}
	if cur == nil && len(existing) >= maxSessionFiles {
		return nil, fmt.Errorf("session %s already holds %d files, the limit; file_delete makes room", s.meta.ID, len(existing))
	}
	var start int64 // what is already there and staying: appending builds on it
	if mode == putAppend && cur != nil {
		start = cur.Size
	}
	room := min(maxFileBytes-start, maxSessionFileBytes-other-start)
	if room <= 0 {
		return nil, s.noRoomErr(name, room, start)
	}

	dir := s.filesDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session files directory: %w", err)
	}
	path := filepath.Join(dir, name)
	var n int64

	if mode == putAppend {
		// Appended in place. A file built up a chunk at a time is
		// incomplete between chunks by its nature — only the caller knows
		// when the last one has landed — so there is no temporary to
		// rename here. A chunk that would pass a limit leaves the file
		// exactly as long as it already was.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		n, err = copyLimited(f, r, room)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n == 0 {
			err = errNoContent
		}
		if err != nil {
			if cur == nil {
				os.Remove(path) // nothing was there before this call
			} else {
				os.Truncate(path, start)
			}
			return nil, s.writeErr(name, err, room, start)
		}
	} else {
		// Written beside and renamed, so a half-written file is never
		// picked up by a listing or handed to a page.
		tmp := filepath.Join(dir, ".incoming")
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		n, err = copyLimited(f, r, room)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n == 0 {
			err = errNoContent
		}
		if err != nil {
			os.Remove(tmp)
			return nil, s.writeErr(name, err, room, start)
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return nil, err
		}
	}
	return &sessionFile{Name: name, Size: start + n, Modified: time.Now(), MIME: fileMIME(name), path: path}, nil
}

var (
	errNoContent = errors.New("no content: give the bytes as content (base64) or the text as text")
	errTooBig    = errors.New("too big")
)

// copyLimited writes at most limit bytes and says so when the source had
// more, by reading one byte past the limit rather than trusting a length
// the sender declared.
func copyLimited(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return limit, errTooBig
	}
	return n, nil
}

func (s *session) writeErr(name string, err error, room, start int64) error {
	if errors.Is(err, errTooBig) {
		return s.noRoomErr(name, room, start)
	}
	return err
}

// noRoomErr names whichever limit is the binding one.
func (s *session) noRoomErr(name string, room, start int64) error {
	if room == maxFileBytes-start {
		if start > 0 {
			return fmt.Errorf("file %q is already %s: the limit is %s per file", name, humanBytes(start), humanBytes(maxFileBytes))
		}
		return fmt.Errorf("file %q is over the %s limit for one file", name, humanBytes(maxFileBytes))
	}
	return fmt.Errorf("session %s has room for %s more (of the %s it may be given); file_delete makes room",
		s.meta.ID, humanBytes(max(room, 0)), humanBytes(maxSessionFileBytes))
}

func (s *session) deleteFile(name string) error {
	have, err := s.listFiles()
	if err != nil {
		return err
	}
	for _, f := range have {
		if f.Name == name {
			return os.Remove(f.path)
		}
	}
	return fmt.Errorf("session %s has no file %q (file_list shows what it holds)", s.meta.ID, name)
}

// filePaths resolves names to the absolute paths Chrome opens for itself
// when an input is set. Order is kept: it is the order the input will hold
// them in. A name is only ever resolved through a listing, so nothing an
// agent passes is joined onto a directory.
func (s *session) filePaths(names []string) ([]string, error) {
	have, err := s.listFiles()
	if err != nil {
		return nil, err
	}
	index := make(map[string]sessionFile, len(have))
	for _, f := range have {
		index[f.Name] = f
	}
	out := make([]string, 0, len(names))
	var missing []string
	for _, n := range names {
		f, ok := index[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		if f.Partial {
			return nil, fmt.Errorf("%q is still downloading (%s so far); wait for it to finish — file_list shows when it has", n, humanBytes(f.Size))
		}
		out = append(out, f.path)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("session %s has no file %s; it holds %s (file_put adds one)",
			s.meta.ID, strings.Join(quoteAll(missing), ", "), fileNameList(have))
	}
	return out, nil
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func fileNameList(files []sessionFile) string {
	if len(files) == 0 {
		return "nothing"
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

// acceptsFile answers whether a file input's accept attribute covers a
// file — a comma-separated list of extensions (".pdf"), MIME types and
// wildcards ("image/*"). An empty accept takes anything. Only used to warn:
// a site that filters with accept simply ignores what does not match, which
// looks like nothing happening at all.
func acceptsFile(accept, name string) bool {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	typ := fileMIME(name)
	for _, tok := range strings.Split(accept, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		switch {
		case tok == "":
			continue
		case tok == "*" || tok == "*/*":
			return true
		case strings.HasPrefix(tok, "."):
			if tok == ext {
				return true
			}
		case strings.HasSuffix(tok, "/*"):
			if typ != "" && strings.HasPrefix(typ, strings.TrimSuffix(tok, "*")) {
				return true
			}
		default:
			if typ != "" && tok == typ {
				return true
			}
		}
	}
	return false
}
