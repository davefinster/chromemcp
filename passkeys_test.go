package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// With disable_passkeys a page -- and a frame in it -- sees no WebAuthn, and
// a security-key request is refused at once rather than opening the
// browser's dialog; without it, WebAuthn is untouched. The switch outlives a
// park.
func TestNoPasskeys(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/frame" {
			w.Write([]byte(`<title>frame</title>`))
			return
		}
		w.Write([]byte(`<title>page</title><iframe id="f" src="/frame"></iframe>`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const probe = `(async () => {
	  const f = document.getElementById('f');
	  if (f && !(f.contentDocument && f.contentDocument.readyState === 'complete')) await new Promise(r => f.addEventListener('load', r, {once: true}));
	  let get;
	  try {
	    await Promise.race([
	      navigator.credentials.get({publicKey: {challenge: new Uint8Array(32), timeout: 2000}}),
	      new Promise((_, reject) => setTimeout(() => reject(new Error('pending')), 1000))]);
	    get = 'resolved';
	  } catch (e) { get = e.name === 'NotAllowedError' ? 'refused' : e.message; }
	  return {page: typeof PublicKeyCredential, frame: typeof f.contentWindow.PublicKeyCredential, get,
	          password: typeof navigator.credentials.store};
	})()`
	type result struct {
		Page, Frame, Get, Password string
	}
	run := func(sid string) result {
		t.Helper()
		var got result
		err := mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
			if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil {
				return err
			}
			return evalJSON(ctx, tb, probe, &got)
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	off, err := mgr.start(ctx, startOptions{Device: nativeDevice, NoPasskeys: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(off.meta.ID); got.Page != "undefined" || got.Frame != "undefined" || got.Get != "refused" || got.Password != "function" {
		t.Errorf("passkeys hidden: %+v", got)
	}
	off.mu.Lock()
	off.park()
	off.mu.Unlock()
	if got := run(off.meta.ID); got.Page != "undefined" || got.Get != "refused" {
		t.Errorf("after a park and resume: %+v", got)
	}

	on, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(on.meta.ID); got.Page != "function" || got.Frame != "function" || got.Get == "refused" {
		t.Errorf("an ordinary session: %+v", got)
	}
}
