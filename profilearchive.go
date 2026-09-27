package main

// A Chrome profile as a stream: how a profile crosses between this server
// and a node (node.go, remote.go) -- an identity pushed onto a node for a
// session to start from, and a node session's profile pulled back to be
// saved as one. The same rules as copyProfile decide what goes: caches,
// locks and the per-launch port file stay behind.

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxProfileArchive bounds what an extraction will write: an identity is a
// few MB without its caches, and nothing a node is asked to hold should be
// anywhere near this.
const maxProfileArchive = 1 << 30

// writeProfileArchive streams the profile at src, minus what copyProfile
// leaves out, as a gzipped tar.
func writeProfileArchive(w io.Writer, src string) error {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("profile directory %s: %v", src, err)
	}
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if profileSkipDirs[name] {
				return filepath.SkipDir
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: filepath.ToSlash(rel) + "/", Mode: 0o700})
		}
		if profileSkipFiles[name] || strings.HasSuffix(name, ".pma") || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: filepath.ToSlash(rel), Mode: 0o600,
			Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
			return err
		}
		// A file Chrome is still growing would overrun its header; the
		// profile is only archived with Chrome stopped, but hold the line.
		_, err = io.CopyN(tw, f, fi.Size())
		return err
	})
	if err != nil {
		return fmt.Errorf("archiving profile: %w", err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// readProfileArchive unpacks a profile archive into dst, which must not
// exist yet. Only directories and regular files are taken, only beneath
// dst, and only up to maxProfileArchive bytes: the archive comes from the
// other end of a connection, and a crafted one must not write elsewhere.
func readProfileArchive(r io.Reader, dst string) (int64, error) {
	if _, err := os.Lstat(dst); err == nil {
		return 0, fmt.Errorf("%s already exists", dst)
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("profile archive: %w", err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return 0, err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("profile archive: %w", err)
		}
		name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
		if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
			return total, fmt.Errorf("profile archive: entry %q is outside the profile", h.Name)
		}
		path := filepath.Join(dst, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return total, err
			}
		case tar.TypeReg:
			if total+h.Size > maxProfileArchive {
				return total, fmt.Errorf("profile archive: over %d bytes", maxProfileArchive)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return total, err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return total, err
			}
			n, err := io.CopyN(f, tr, h.Size)
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return total, err
			}
			if !h.ModTime.IsZero() {
				os.Chtimes(path, h.ModTime, h.ModTime)
			}
		default:
			return total, fmt.Errorf("profile archive: entry %q is not a file or directory", h.Name)
		}
	}
}
