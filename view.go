package main

// The live view of a headful session: noVNC served under /view/<token>/,
// with the websocket at /view/<token>/ws bridged to the session's Xvnc
// socket. The token is the whole authentication — a browser opening a
// link cannot carry the bearer token the rest of the server requires — so
// tokens are unguessable, short-lived, and bound to one session. This is
// how the owner logs in to an account inside a session (Google, with its
// passkeys and 2FA, which no agent can do) before saving it as an identity.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type viewToken struct {
	session string
	expires time.Time
}

type viewTokens struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]viewToken
}

func newViewTokens(ttl time.Duration) *viewTokens {
	return &viewTokens{ttl: ttl, m: map[string]viewToken{}}
}

func (v *viewTokens) issue(session string) (string, time.Time) {
	var b [24]byte
	rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	exp := time.Now().Add(v.ttl)
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, t := range v.m {
		if time.Now().After(t.expires) {
			delete(v.m, k)
		}
	}
	v.m[tok] = viewToken{session: session, expires: exp}
	return tok, exp
}

// lookup returns the session a live token belongs to.
func (v *viewTokens) lookup(tok string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.m[tok]
	if !ok || time.Now().After(t.expires) {
		return "", false
	}
	return t.session, true
}

func (v *viewTokens) revokeSession(session string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, t := range v.m {
		if t.session == session {
			delete(v.m, k)
		}
	}
}

type viewHandler struct {
	mgr   *manager
	files http.Handler
	ok    bool
}

func newViewHandler(mgr *manager, novncDir string) *viewHandler {
	h := &viewHandler{mgr: mgr}
	if fi, err := os.Stat(filepath.Join(novncDir, "vnc.html")); err == nil && !fi.IsDir() {
		h.files = http.FileServer(http.Dir(novncDir))
		h.ok = true
	} else {
		logf("no noVNC at %s: live-view links are unavailable (install novnc or set -novnc)", novncDir)
	}
	return h
}

// available reports whether live views can be served at all.
func (h *viewHandler) available() bool { return h.ok && h.mgr.cfg.Xvnc != "" }

// ServeHTTP handles /view/<token>/<path>.
func (h *viewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/view/")
	tok, path, _ := strings.Cut(rest, "/")
	if tok == "" || tok == rest {
		http.NotFound(w, r)
		return
	}
	sid, ok := h.mgr.views.lookup(tok)
	if !ok {
		http.Error(w, "this view link has expired; ask for a new one (session_view)", http.StatusForbidden)
		return
	}
	if path == "ws" {
		h.bridge(w, r, sid)
		return
	}
	if !h.ok {
		http.Error(w, "noVNC is not installed on this server", http.StatusNotFound)
		return
	}
	if path == "" {
		http.Redirect(w, r, r.URL.Path+"vnc.html?autoconnect=1&resize=scale&path=ws", http.StatusFound)
		return
	}
	// Nothing under noVNC needs to be cached by a browser against a token.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + path
	h.files.ServeHTTP(w, r2)
}

// bridge carries RFB between the noVNC websocket and the session's Xvnc.
func (h *viewHandler) bridge(w http.ResponseWriter, r *http.Request, sid string) {
	s, err := h.mgr.get(sid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.mu.Lock()
	disp := s.disp
	running := s.isRunning()
	s.mu.Unlock()
	if !running || disp == nil {
		http.Error(w, "the session is not running headful; resume it (session_resume) and ask for a new link", http.StatusConflict)
		return
	}
	vnc, err := net.DialTimeout(disp.Net, disp.Addr, 5*time.Second)
	if err != nil {
		http.Error(w, "vnc: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer vnc.Close()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"binary"},
		// The page and the socket share an origin (the token URL); a
		// cross-origin page could not have the token in the first place.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()

	s.viewers.Add(1)
	s.touch()
	defer s.viewers.Add(-1)
	logf("session %s: live view connected from %s", sid, r.RemoteAddr)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() { io.Copy(ws, vnc); done <- struct{}{} }()
	go func() { io.Copy(vnc, ws); done <- struct{}{} }()
	// Keep the session marked in use while someone is watching.
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-done:
			logf("session %s: live view disconnected", sid)
			return
		case <-tick.C:
			s.touch()
		}
	}
}
