package main

// The live view of a session with no display of its own: a node's Chrome
// (whose headful window WindowServer never shows) or a headless one here.
// There is no framebuffer to serve over VNC, so the view is built from
// DevTools instead: Page.startScreencast sends the tab's frames as JPEGs, and
// what the person does on the page -- mouse, wheel, keys, a paste -- goes back
// as Input.dispatch* on the same tab. It is served beside the noVNC view under
// the same token (view.go): /view/<tok>/cast is the page, /view/<tok>/cast.ws
// its websocket, and the token is the whole of its authentication.
//
// The view connects to the browser on a DevTools connection of its own, so
// it neither shares nor disturbs chromedp's. It follows new tabs as they open
// (a sign-in popup is the usual one), shows the session's tabs to switch
// between, and brings the tab it shows to the front, because a headless
// Chrome renders only that one; and then tells the session its idea of the
// front tab is stale, so the agent's next action brings its own tab back.
//
// What it cannot do is what the page never sees: a native dialog, a passkey
// on the machine's own authenticator, the browser's own menus.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/coder/websocket"
)

// castIn is one message from the viewer page.
type castIn struct {
	T          string  `json:"t"`              // mouse, wheel, key, paste, tab
	Type       string  `json:"type,omitempty"` // mousePressed/Released/Moved; keyDown/keyUp
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Button     string  `json:"button,omitempty"`
	Buttons    int64   `json:"buttons,omitempty"`
	ClickCount int64   `json:"clickCount,omitempty"`
	DX         float64 `json:"dx,omitempty"`
	DY         float64 `json:"dy,omitempty"`
	Key        string  `json:"key,omitempty"`
	Code       string  `json:"code,omitempty"`
	KeyCode    int64   `json:"keyCode,omitempty"`
	Text       string  `json:"text,omitempty"`
	Modifiers  int64   `json:"modifiers,omitempty"`
	ID         string  `json:"id,omitempty"`
}

type castTab struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Current bool   `json:"current,omitempty"`
}

// caster is one viewer's connection.
type caster struct {
	s   *session
	cc  *cdpConn
	ws  *websocket.Conn
	ctx context.Context

	mu      sync.Mutex
	target  target.ID
	sid     target.SessionID
	pages   map[target.ID]*target.Info
	order   []target.ID // oldest first
	lastErr string
}

// cast serves the view's websocket.
func (h *viewHandler) cast(w http.ResponseWriter, r *http.Request, sid string) {
	s, err := h.mgr.get(sid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.mu.Lock()
	running := s.isRunning()
	var wsURL string
	var current target.ID
	if running {
		wsURL, current = s.chrome.wsURL(), s.current
	}
	s.mu.Unlock()
	if !running {
		http.Error(w, "the session is parked; any browser tool resumes it, then reload this page", http.StatusConflict)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The page and the socket share an origin (the token URL), as for
		// the noVNC bridge.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cc, err := dialCDP(ctx, wsURL, logf)
	if err != nil {
		c.Close(websocket.StatusInternalError, "devtools: "+err.Error())
		return
	}
	defer cc.close()

	s.viewers.Add(1)
	s.touch()
	defer s.viewers.Add(-1)
	logf("session %s: screencast view connected from %s", sid, r.RemoteAddr)
	defer logf("session %s: screencast view disconnected", sid)

	v := &caster{s: s, cc: cc, ws: c, ctx: ctx, pages: map[target.ID]*target.Info{}}
	go cc.events(v.handle)
	if err := v.start(current); err != nil {
		c.Close(websocket.StatusInternalError, err.Error())
		return
	}
	// Keep the session marked in use while someone is watching.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.touch()
			}
		}
	}()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var in castIn
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		s.touch()
		v.input(in)
	}
}

// start learns the browser's pages and attaches to the session's current
// one (or the first).
func (v *caster) start(current target.ID) error {
	if err := v.cc.call(v.ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		return fmt.Errorf("discovering tabs: %w", err)
	}
	var res target.GetTargetsReturns
	if err := v.cc.call(v.ctx, "", "Target.getTargets", nil, &res); err != nil {
		return fmt.Errorf("listing tabs: %w", err)
	}
	v.mu.Lock()
	for _, info := range res.TargetInfos {
		v.addLocked(info)
	}
	pick := current
	if _, ok := v.pages[pick]; !ok && len(v.order) > 0 {
		pick = v.order[0]
	}
	v.mu.Unlock()
	if pick == "" {
		return fmt.Errorf("the browser has no tab to show")
	}
	return v.attach(pick)
}

func (v *caster) addLocked(info *target.Info) {
	if info == nil || info.Type != "page" || internalTarget(info) {
		return
	}
	if _, ok := v.pages[info.TargetID]; !ok {
		v.order = append(v.order, info.TargetID)
	}
	v.pages[info.TargetID] = info
}

func (v *caster) removeLocked(id target.ID) {
	delete(v.pages, id)
	for i, t := range v.order {
		if t == id {
			v.order = append(v.order[:i], v.order[i+1:]...)
			break
		}
	}
}

// attach moves the view to a tab: the screencast stops on the old one and
// starts on the new, which is brought to the front first.
func (v *caster) attach(id target.ID) error {
	v.mu.Lock()
	old := v.sid
	v.mu.Unlock()
	if old != "" {
		v.cc.send(v.ctx, old, "Page.stopScreencast", nil)
		v.cc.send(v.ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": old})
	}
	var res struct {
		SessionID target.SessionID `json:"sessionId"`
	}
	if err := v.cc.call(v.ctx, "", "Target.attachToTarget", map[string]any{"targetId": id, "flatten": true}, &res); err != nil {
		return fmt.Errorf("attaching to the tab: %w", err)
	}
	v.mu.Lock()
	v.target, v.sid = id, res.SessionID
	v.mu.Unlock()
	v.cc.send(v.ctx, res.SessionID, "Page.bringToFront", nil)
	// The session's cached front tab is no longer in front; its next action
	// brings its own tab back (session.focus). Off this goroutine: the
	// session's lock is held for as long as an agent's action runs.
	go func() {
		v.s.mu.Lock()
		v.s.front = ""
		v.s.mu.Unlock()
	}()
	if err := v.cc.call(v.ctx, res.SessionID, "Page.startScreencast",
		map[string]any{"format": "jpeg", "quality": 75, "everyNthFrame": 1}, nil); err != nil {
		return fmt.Errorf("starting the screencast: %w", err)
	}
	v.sendTabs()
	return nil
}

// handle runs on the connection's event goroutine.
func (v *caster) handle(m *cdproto.Message) {
	switch m.Method {
	case "Page.screencastFrame":
		var ev page.EventScreencastFrame
		if json.Unmarshal(m.Params, &ev) != nil {
			return
		}
		// Every frame is acknowledged, or Chrome sends no more.
		v.cc.send(v.ctx, m.SessionID, "Page.screencastFrameAck", map[string]any{"sessionId": ev.SessionID})
		v.mu.Lock()
		current := m.SessionID == v.sid
		v.mu.Unlock()
		if !current {
			return
		}
		w, h := frameSize(&ev)
		b, _ := json.Marshal(map[string]any{"t": "frame", "data": ev.Data, "w": w, "h": h})
		v.ws.Write(v.ctx, websocket.MessageText, b)
	case "Target.targetCreated":
		var ev target.EventTargetCreated
		if json.Unmarshal(m.Params, &ev) != nil || ev.TargetInfo == nil {
			return
		}
		v.mu.Lock()
		_, known := v.pages[ev.TargetInfo.TargetID]
		v.addLocked(ev.TargetInfo)
		_, isPage := v.pages[ev.TargetInfo.TargetID]
		v.mu.Unlock()
		// A tab that opens while someone is watching is almost always the
		// one they need to see: a sign-in popup, a link opened in a new tab.
		if isPage && !known {
			go v.attach(ev.TargetInfo.TargetID)
		}
	case "Target.targetInfoChanged":
		var ev target.EventTargetInfoChanged
		if json.Unmarshal(m.Params, &ev) != nil || ev.TargetInfo == nil {
			return
		}
		v.mu.Lock()
		if _, ok := v.pages[ev.TargetInfo.TargetID]; ok {
			v.pages[ev.TargetInfo.TargetID] = ev.TargetInfo
		}
		v.mu.Unlock()
		v.sendTabs()
	case "Target.targetDestroyed":
		var ev target.EventTargetDestroyed
		if json.Unmarshal(m.Params, &ev) != nil {
			return
		}
		v.mu.Lock()
		v.removeLocked(ev.TargetID)
		wasCurrent := ev.TargetID == v.target
		var next target.ID
		if wasCurrent && len(v.order) > 0 {
			next = v.order[len(v.order)-1]
		}
		if wasCurrent {
			v.target, v.sid = "", ""
		}
		v.mu.Unlock()
		if next != "" {
			go v.attach(next)
		} else {
			v.sendTabs()
		}
	}
}

// frameSize is the size, in the page's CSS pixels, that a frame shows --
// what the viewer scales a click on it by. It is the frame's metadata, which
// Chrome sometimes sends as 0x0: seen on a headful Mac session while Google
// waited on a security key, where every click then landed at (0, 0). Then it
// is the JPEG's own size, which is the viewport's at the scale sessions run
// at (1).
func frameSize(ev *page.EventScreencastFrame) (float64, float64) {
	if m := ev.Metadata; m != nil && m.DeviceWidth > 0 && m.DeviceHeight > 0 {
		return m.DeviceWidth, m.DeviceHeight
	}
	b, err := base64.StdEncoding.DecodeString(ev.Data)
	if err != nil {
		return 0, 0
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	return float64(cfg.Width), float64(cfg.Height)
}

func (v *caster) sendTabs() {
	v.mu.Lock()
	tabs := make([]castTab, 0, len(v.order))
	for _, id := range v.order {
		info := v.pages[id]
		tabs = append(tabs, castTab{ID: string(id), Title: info.Title, URL: info.URL, Current: id == v.target})
	}
	v.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"t": "tabs", "tabs": tabs})
	v.ws.Write(v.ctx, websocket.MessageText, b)
}

// input replays one of the viewer's actions on the tab it is showing.
func (v *caster) input(in castIn) {
	v.mu.Lock()
	sid := v.sid
	v.mu.Unlock()
	switch in.T {
	case "tab":
		v.mu.Lock()
		_, ok := v.pages[target.ID(in.ID)]
		v.mu.Unlock()
		if ok {
			go v.attach(target.ID(in.ID))
		}
		return
	}
	if sid == "" {
		return
	}
	switch in.T {
	case "mouse":
		switch in.Type {
		case "mousePressed", "mouseReleased", "mouseMoved":
		default:
			return
		}
		p := map[string]any{"type": in.Type, "x": in.X, "y": in.Y, "modifiers": in.Modifiers, "buttons": in.Buttons}
		if in.Type != "mouseMoved" {
			p["button"], p["clickCount"] = in.Button, in.ClickCount
		}
		v.cc.send(v.ctx, sid, "Input.dispatchMouseEvent", p)
	case "wheel":
		v.cc.send(v.ctx, sid, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": in.X, "y": in.Y, "deltaX": in.DX, "deltaY": in.DY, "modifiers": in.Modifiers})
	case "key":
		typ := in.Type
		switch typ {
		case "keyDown":
			// A key that types something is a keyDown with its text, which
			// Chrome turns into the keypress and input a page listens for;
			// the others (arrows, Backspace, shortcuts) are raw.
			if in.Text == "" {
				typ = "rawKeyDown"
			}
		case "keyUp":
		default:
			return
		}
		p := map[string]any{"type": typ, "key": in.Key, "code": in.Code, "modifiers": in.Modifiers,
			"windowsVirtualKeyCode": in.KeyCode, "nativeVirtualKeyCode": in.KeyCode}
		if typ == "keyDown" {
			p["text"], p["unmodifiedText"] = in.Text, in.Text
		}
		v.cc.send(v.ctx, sid, "Input.dispatchKeyEvent", p)
	case "paste":
		if in.Text != "" {
			v.cc.send(v.ctx, sid, "Input.insertText", map[string]any{"text": in.Text})
		}
	}
}

// castPage is the viewer: self-contained, as the drop page is (upload.go),
// because it is served from a token URL nothing else may load from.
func (h *viewHandler) castPage(w http.ResponseWriter, sid string) {
	var b [16]byte
	rand.Read(b[:])
	nonce := hex.EncodeToString(b[:])
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'nonce-"+nonce+
		"'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	fmt.Fprintf(w, castPageHTML, nonce, sid)
}

// castPageHTML takes: the nonce, the session id.
const castPageHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Live view · %[2]s</title>
<style nonce="%[1]s">
:root { color-scheme: dark; --bg:#141416; --bar:#1e1e22; --fg:#e9e9e4; --dim:#9a9a94; --line:#33333a; --accent:#7aa9d8; }
* { box-sizing:border-box }
html, body { margin:0; height:100%%; background:var(--bg); color:var(--fg);
  font:13px/1.4 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif; }
body { display:flex; flex-direction:column }
#bar { display:flex; gap:6px; align-items:center; padding:6px 8px; background:var(--bar); border-bottom:1px solid var(--line); overflow-x:auto }
#bar .tab { max-width:220px; padding:4px 10px; border:1px solid var(--line); border-radius:6px; background:none; color:var(--dim);
  font:inherit; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; cursor:pointer }
#bar .tab.cur { color:var(--fg); border-color:var(--accent) }
#status { margin-left:auto; color:var(--dim); white-space:nowrap; padding-left:8px }
#stage { flex:1; min-height:0; display:flex; align-items:center; justify-content:center; outline:none }
#screen { max-width:100%%; max-height:100%%; cursor:default; user-select:none; -webkit-user-drag:none }
#stage:focus-visible #screen { box-shadow:0 0 0 2px var(--accent) }
</style></head>
<body>
<div id="bar"><span id="tabs"></span><span id="status">connecting…</span></div>
<div id="stage" tabindex="0"><img id="screen" alt=""></div>
<script nonce="%[1]s">
(() => {
  const screen = document.getElementById('screen'), stage = document.getElementById('stage');
  const status = document.getElementById('status'), tabsEl = document.getElementById('tabs');
  const url = new URL('cast.ws', location.href); url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(url);
  let fw = 0, fh = 0;
  const send = m => { if (ws.readyState === 1) ws.send(JSON.stringify(m)); };
  ws.onopen = () => { status.textContent = 'live'; stage.focus(); };
  ws.onclose = e => { status.textContent = 'disconnected' + (e.reason ? ': ' + e.reason : '') + ' — reload to reconnect'; };
  ws.onmessage = e => {
    const m = JSON.parse(e.data);
    if (m.t === 'frame') { fw = m.w; fh = m.h; screen.src = 'data:image/jpeg;base64,' + m.data; }
    else if (m.t === 'tabs') {
      tabsEl.replaceChildren(...m.tabs.map(t => {
        const b = document.createElement('button');
        b.className = 'tab' + (t.current ? ' cur' : ''); b.textContent = t.title || t.url || 'tab'; b.title = t.url;
        b.onclick = () => { send({t: 'tab', id: t.id}); stage.focus(); };
        return b;
      }));
    }
  };
  const mods = e => (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);
  // The frame's size in page pixels; the image's own when none came with it.
  const at = e => {
    const r = screen.getBoundingClientRect(), w = fw || screen.naturalWidth, h = fh || screen.naturalHeight;
    return {x: (e.clientX - r.left) * w / r.width, y: (e.clientY - r.top) * h / r.height};
  };
  const btn = b => ['left', 'middle', 'right', 'back', 'forward'][b] || 'none';
  const mouse = (type, e) => { const p = at(e); send({t: 'mouse', type, x: p.x, y: p.y, button: btn(e.button), buttons: e.buttons, clickCount: e.detail || 1, modifiers: mods(e)}); };
  screen.addEventListener('mousedown', e => { e.preventDefault(); stage.focus(); mouse('mousePressed', e); });
  screen.addEventListener('mouseup', e => { e.preventDefault(); mouse('mouseReleased', e); });
  let pending = null;
  screen.addEventListener('mousemove', e => { if (!pending) requestAnimationFrame(() => { mouse('mouseMoved', pending); pending = null; }); pending = e; });
  screen.addEventListener('contextmenu', e => e.preventDefault());
  screen.addEventListener('wheel', e => { e.preventDefault(); const p = at(e); send({t: 'wheel', x: p.x, y: p.y, dx: e.deltaX, dy: e.deltaY, modifiers: mods(e)}); }, {passive: false});
  const isPaste = e => (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'v';
  const key = (type, e) => {
    if (isPaste(e)) return; // the paste event carries the text instead
    e.preventDefault();
    let text = '';
    if (type === 'keyDown' && !e.ctrlKey && !e.metaKey) text = e.key.length === 1 ? e.key : (e.key === 'Enter' ? '\r' : '');
    send({t: 'key', type, key: e.key, code: e.code, keyCode: e.keyCode, text, modifiers: mods(e)});
  };
  stage.addEventListener('keydown', e => key('keyDown', e));
  stage.addEventListener('keyup', e => key('keyUp', e));
  document.addEventListener('paste', e => { e.preventDefault(); send({t: 'paste', text: e.clipboardData.getData('text')}); });
})();
</script>
</body></html>`
