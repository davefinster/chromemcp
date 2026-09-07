package main

// Identities: named snapshots of a Chrome profile that has been logged in
// to whatever accounts the owner wants an agent to act as — a Google
// account, most likely, whose sign-in (passkeys, 2FA) no agent can do
// itself. A session started "as" an identity begins on a copy of that
// profile, so it is logged in from its first page. Identities persist
// (they are the only thing here that does); sessions are ephemeral.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type identityMeta struct {
	Name    string    `json:"name"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Session string    `json:"session,omitempty"` // the session it was last saved from
	Bytes   int64     `json:"bytes"`
}

type identityStore struct {
	dir string
	mu  sync.Mutex
}

var identityNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func newIdentityStore(dir string) (*identityStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identities dir: %w", err)
	}
	return &identityStore{dir: dir}, nil
}

func (st *identityStore) path(name string) string { return filepath.Join(st.dir, name) }

func checkIdentityName(name string) error {
	if !identityNameRe.MatchString(name) {
		return fmt.Errorf("identity name %q: use lowercase letters, digits, '.', '_' or '-' (max 64)", name)
	}
	return nil
}

func (st *identityStore) get(name string) (*identityMeta, error) {
	if err := checkIdentityName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(st.path(name), "identity.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no identity %q (identity_list shows the saved ones)", name)
	}
	if err != nil {
		return nil, err
	}
	var m identityMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("identity %q: %w", name, err)
	}
	return &m, nil
}

func (st *identityStore) list() ([]*identityMeta, error) {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return nil, err
	}
	var out []*identityMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := st.get(e.Name())
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// seed copies an identity — its profile and its cookie jar — into a
// session directory whose profile does not exist yet.
func (st *identityStore) seed(name, sessionDir string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := st.get(name); err != nil {
		return err
	}
	if _, err := copyProfile(filepath.Join(st.path(name), "profile"), filepath.Join(sessionDir, "profile")); err != nil {
		return err
	}
	if _, err := copyFile(filepath.Join(st.path(name), cookiesFile), filepath.Join(sessionDir, cookiesFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// save snapshots a session directory — the profile (Chrome must not be
// running on it) and the cookie jar exported when it was parked — as an
// identity, replacing an existing one only with overwrite.
func (st *identityStore) save(name, note, session, sessionDir string, overwrite bool) (*identityMeta, error) {
	if err := checkIdentityName(name); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	dir := st.path(name)
	existing, _ := st.get(name)
	if existing != nil && !overwrite {
		return nil, fmt.Errorf("identity %q exists (saved %s); pass overwrite=true to replace it", name, existing.Updated.Format(time.RFC3339))
	}
	// Build beside, then swap: a failed copy never leaves a half identity.
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	n, err := copyProfile(filepath.Join(sessionDir, "profile"), filepath.Join(tmp, "profile"))
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if cn, err := copyFile(filepath.Join(sessionDir, cookiesFile), filepath.Join(tmp, cookiesFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.RemoveAll(tmp)
		return nil, err
	} else {
		n += cn
	}
	now := time.Now()
	m := &identityMeta{Name: name, Note: note, Created: now, Updated: now, Session: session, Bytes: n}
	if existing != nil {
		m.Created = existing.Created
		if note == "" {
			m.Note = existing.Note
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "identity.json"), b, 0o600); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	old := dir + ".old"
	os.RemoveAll(old)
	if existing != nil {
		if err := os.Rename(dir, old); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.Rename(old, dir)
		os.RemoveAll(tmp)
		return nil, err
	}
	os.RemoveAll(old)
	return m, nil
}

func (st *identityStore) remove(name string) error {
	if _, err := st.get(name); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return os.RemoveAll(st.path(name))
}

// profileSkip lists what a profile copy leaves behind: caches (large and
// regenerated), the lock files of the Chrome that used it, and the
// DevTools port file, which is per launch.
var profileSkipDirs = map[string]bool{
	"Cache": true, "Code Cache": true, "GPUCache": true, "ShaderCache": true,
	"GrShaderCache": true, "GraphiteDawnCache": true, "DawnCache": true,
	"DawnGraphiteCache": true, "DawnWebGPUCache": true, "component_crx_cache": true,
	"extensions_crx_cache": true, "BrowserMetrics": true, "Crashpad": true,
	"CacheStorage": true, "Service Worker": true, "optimization_guide_model_store": true,
	"OptimizationGuidePredictionModels": true, "Safe Browsing": true, "segmentation_platform": true,
	"MEIPreload": true, "ZxcvbnData": true, "OnDeviceHeadSuggestModel": true,
	"blob_storage": true,
}

var profileSkipFiles = map[string]bool{
	"SingletonLock": true, "SingletonSocket": true, "SingletonCookie": true,
	"DevToolsActivePort": true, "lockfile": true, "LOCK": true,
	"RunningChromeVersion": true, "BrowserMetrics-spare.pma": true,
}

// copyProfile copies a Chrome user-data-dir, minus the caches, and reports
// the bytes written. Symlinks (Chrome's lock is one) are skipped.
func copyProfile(src, dst string) (int64, error) {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return 0, fmt.Errorf("profile directory %s: %v", src, err)
	}
	var total int64
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return os.MkdirAll(dst, 0o700)
		}
		name := d.Name()
		if d.IsDir() {
			if profileSkipDirs[name] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if profileSkipFiles[name] || strings.HasSuffix(name, ".pma") || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		n, err := copyFile(path, filepath.Join(dst, rel))
		total += n
		return err
	})
	if err != nil {
		return total, fmt.Errorf("copying profile: %w", err)
	}
	return total, nil
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}
