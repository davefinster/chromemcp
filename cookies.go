package main

// Cookies are carried by this server as well as by Chrome's disk store.
// Chrome commits cookie changes to its SQLite store on a 30-second timer
// and flushes them on a graceful Browser.close — but not when it has to
// be signalled instead (a Chrome that does not answer, or a kill), and
// then a login made in the last half minute would be lost and an identity
// saved right after signing in would be empty. So every park exports the
// whole jar over CDP to cookies.json beside the profile, and every launch
// imports it back (replacing whatever the on-disk store held), which makes
// the file the truth whenever it exists and the database the fallback.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

const cookiesFile = "cookies.json"

// exportCookies writes the session's cookie jar to dir/cookies.json.
// Caller holds s.mu; the session is running.
func (s *session) exportCookies() error {
	t, err := s.resolveTab("")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	var cookies []*network.Cookie
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = storage.GetCookies().Do(ctx)
		return err
	}))
	if err != nil {
		return fmt.Errorf("reading cookies: %w", err)
	}
	if cookies == nil {
		cookies = []*network.Cookie{}
	}
	b, err := json.Marshal(cookies)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, cookiesFile)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// importCookies loads dir/cookies.json into the running Chrome, replacing
// the jar it started with, and removes the file so a later launch never
// re-imports a jar older than the one Chrome then holds. Caller holds s.mu.
func (s *session) importCookies(ctx context.Context) error {
	path := filepath.Join(s.dir, cookiesFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cookies []*network.Cookie
	if err := json.Unmarshal(b, &cookies); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	params := make([]*network.CookieParam, 0, len(cookies))
	now := time.Now()
	for _, c := range cookies {
		p := &network.CookieParam{
			Name:         c.Name,
			Value:        c.Value,
			Domain:       c.Domain,
			Path:         c.Path,
			Secure:       c.Secure,
			HTTPOnly:     c.HTTPOnly,
			SameSite:     c.SameSite,
			Priority:     c.Priority,
			SourceScheme: c.SourceScheme,
			SourcePort:   c.SourcePort,
			PartitionKey: c.PartitionKey,
		}
		if c.Expires > 0 {
			exp := time.Unix(0, int64(c.Expires*1e9))
			if exp.Before(now) {
				continue
			}
			t := cdp.TimeSinceEpoch(exp)
			p.Expires = &t
		}
		params = append(params, p)
	}
	t, err := s.resolveTab("")
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(t.ctx, 20*time.Second)
	defer cancel()
	err = chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := network.ClearBrowserCookies().Do(ctx); err != nil {
			return err
		}
		if len(params) == 0 {
			return nil
		}
		return storage.SetCookies(params).Do(ctx)
	}))
	if err != nil {
		return fmt.Errorf("restoring %d cookies: %w", len(params), err)
	}
	os.Remove(path)
	return nil
}
