package main

// Session files: bytes the calling agent hands the server so a page can be
// given them later through its file picker — an attachment, a photo, a CSV
// to import. They live in the session directory beside the profile, so
// their lifecycle is the session's exactly: kept while it is parked,
// deleted with it (manager.remove removes the whole directory) and reaped
// with it. identity_save copies only the profile and the cookie jar, so a
// file never follows an identity into another session.

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	maxSessionFileBytes = 100 << 20 // everything a session holds
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

// sessionFile is one file a session holds.
type sessionFile struct {
	Name     string
	Size     int64
	Modified time.Time
	MIME     string // what the extension says; Chrome decides the File.type the same way
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

// ---- the store, one directory per session ----

func (s *session) listFiles() ([]sessionFile, error) {
	entries, err := os.ReadDir(s.filesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []sessionFile
	for _, e := range entries {
		if e.IsDir() || checkFileName(e.Name()) != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, sessionFile{Name: e.Name(), Size: fi.Size(), Modified: fi.ModTime(), MIME: fileMIME(e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *session) filesTotal(files []sessionFile) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// putFile writes one file into the session, replacing a file of that name
// only with overwrite. Written beside and renamed, so a half-written file
// is never picked up by a listing or handed to a page.
func (s *session) putFile(name string, data []byte, overwrite bool) (*sessionFile, error) {
	if err := checkFileName(name); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("no content: give the bytes as content (base64) or the text as text")
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("file %q is %s: the limit is %s per file", name, humanBytes(int64(len(data))), humanBytes(maxFileBytes))
	}
	existing, err := s.listFiles()
	if err != nil {
		return nil, err
	}
	var other int64
	replacing := false
	for _, f := range existing {
		if f.Name == name {
			if !overwrite {
				return nil, fmt.Errorf("session %s already has a file %q (%s, put %s); pass overwrite=true to replace it",
					s.meta.ID, name, humanBytes(f.Size), f.Modified.Format("15:04:05"))
			}
			replacing = true
			continue
		}
		other += f.Size
	}
	if !replacing && len(existing) >= maxSessionFiles {
		return nil, fmt.Errorf("session %s already holds %d files, the limit; file_delete makes room", s.meta.ID, len(existing))
	}
	if other+int64(len(data)) > maxSessionFileBytes {
		return nil, fmt.Errorf("session %s holds %s of files; %s more would pass the %s limit; file_delete makes room",
			s.meta.ID, humanBytes(other), humanBytes(int64(len(data))), humanBytes(maxSessionFileBytes))
	}
	dir := s.filesDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session files directory: %w", err)
	}
	tmp := filepath.Join(dir, ".incoming")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return &sessionFile{Name: name, Size: int64(len(data)), Modified: time.Now(), MIME: fileMIME(name)}, nil
}

func (s *session) deleteFile(name string) error {
	if err := checkFileName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.filesDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session %s has no file %q (file_list shows what it holds)", s.meta.ID, name)
	}
	return err
}

// filePaths resolves names to absolute paths for Chrome, which opens the
// files itself when an input is set. Order is kept: it is the order the
// input will hold them in.
func (s *session) filePaths(names []string) ([]string, error) {
	have, err := s.listFiles()
	if err != nil {
		return nil, err
	}
	index := make(map[string]sessionFile, len(have))
	for _, f := range have {
		index[f.Name] = f
	}
	dir, err := filepath.Abs(s.filesDir())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	var missing []string
	for _, n := range names {
		if _, ok := index[n]; !ok {
			missing = append(missing, n)
			continue
		}
		out = append(out, filepath.Join(dir, n))
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
