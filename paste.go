package main

// Pasting into a session from the live view: the person's own clipboard --
// typically a password out of a password manager on their machine -- into
// the field that has focus in the session's browser. Both views catch the
// paste shortcut over the remote screen, take the text from the browser's
// own paste (no clipboard permission to ask for), and hand it to the server,
// which types it in as Input.insertText: one IME commit, which every page
// hears as ordinary input, and which reaches a field inside a cross-origin
// frame too, because Chrome sends it to the focused widget.
//
// The text never passes through a clipboard in the session -- not Xvnc's,
// not Chrome's -- so nothing pasted is left behind for a later paste, a
// middle click or a page to read. Nor is it logged. And it goes only where
// a person put the focus: the noVNC view (a whole display, any of whose tabs
// and windows could be the one in front) pastes into the page whose
// document has the focus, and refuses when none does -- the address bar or
// a dialog of Chrome's own has it -- rather than guess; the screencast view
// pastes into the tab it shows. Either refuses when nothing in the page that
// could take text has the focus, so the person hears that it went nowhere.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/target"
)

// pasteMax is the most text one paste carries: plenty for a password, a
// one-time code or a paragraph. A file is file_upload_url's business.
const pasteMax = 64 << 10

// errPasteNoFocus is the noVNC view's refusal when no page has the focus.
var errPasteNoFocus = errors.New("Nothing was pasted: no page has the keyboard focus (the address bar or a browser dialog may have it). Click into the field, then paste again.")

// focusState is what focusProbe reports about a page.
type focusState struct {
	Focus bool   `json:"focus"` // the document has the browser's focus
	Field bool   `json:"field"` // what has the focus in it could take text
	Title string `json:"title"`
}

// focusProbe runs in a world of its own (probeFocus), where a page's
// scripts cannot redefine what it calls. It follows the focus into open
// shadow roots. A frame, or an element whose shadow root is closed, may hold
// a field it cannot see into, so those count as one.
const focusProbe = `(() => {
  let e = document.activeElement;
  while (e && e.shadowRoot && e.shadowRoot.activeElement) e = e.shadowRoot.activeElement;
  const field = !!e && e !== document.body && e !== document.documentElement && (e.isContentEditable ||
    e.tagName === 'TEXTAREA' ||
    (e.tagName === 'INPUT' && !/^(button|checkbox|color|file|hidden|image|radio|range|reset|submit)$/i.test(e.type)) ||
    /^(IFRAME|FRAME|OBJECT|EMBED)$/.test(e.tagName) || e.tagName.includes('-'));
  return {focus: document.hasFocus(), field, title: document.title};
})()`

// probeFocus asks a page, attached as sid, where its focus is.
func probeFocus(ctx context.Context, cc *cdpConn, sid target.SessionID, id target.ID) (*focusState, error) {
	var world struct {
		ID int64 `json:"executionContextId"`
	}
	// A page target's main frame shares its id.
	if err := cc.call(ctx, sid, "Page.createIsolatedWorld", map[string]any{"frameId": id, "worldName": "chromemcp-paste"}, &world); err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := cc.call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": focusProbe, "contextId": world.ID, "returnByValue": true}, &res); err != nil {
		return nil, err
	}
	if res.Exception != nil {
		return nil, errors.New(res.Exception.Text)
	}
	var f focusState
	if err := json.Unmarshal(res.Result.Value, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// focusedPage finds the page whose document has the focus -- the front tab
// of the window the person last clicked in -- and returns it attached.
func focusedPage(ctx context.Context, cc *cdpConn) (target.SessionID, *focusState, error) {
	var res target.GetTargetsReturns
	if err := cc.call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return "", nil, err
	}
	for _, info := range res.TargetInfos {
		if info.Type != "page" || internalTarget(info) {
			continue
		}
		var a struct {
			SessionID target.SessionID `json:"sessionId"`
		}
		if err := cc.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": info.TargetID, "flatten": true}, &a); err != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second) // a page stuck in a script is not the one
		f, err := probeFocus(pctx, cc, a.SessionID, info.TargetID)
		cancel()
		if err == nil && f.Focus {
			return a.SessionID, f, nil
		}
		cc.send(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": a.SessionID})
	}
	return "", nil, errPasteNoFocus
}

// pasteInto types text into the focused field of a page whose focus has
// been probed, and says what happened in words for the person who pasted.
func pasteInto(ctx context.Context, cc *cdpConn, sid target.SessionID, f *focusState, text string) (string, error) {
	if !f.Field {
		return "", errors.New("Nothing was pasted: click into the text field first, then paste again.")
	}
	if err := cc.call(ctx, sid, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return "", fmt.Errorf("Nothing was pasted: %v", err)
	}
	n := utf8.RuneCountInString(text)
	msg := fmt.Sprintf("Pasted %d characters", n)
	if n == 1 {
		msg = "Pasted 1 character"
	}
	if t := strings.TrimSpace(f.Title); t != "" {
		if utf8.RuneCountInString(t) > 60 {
			t = string([]rune(t)[:59]) + "…"
		}
		msg += " into “" + t + "”"
	}
	return msg, nil
}

// paste serves the noVNC view's POST /view/<tok>/paste: the body is the
// text, the reply is what to tell the person, whatever the status.
func (h *viewHandler) paste(w http.ResponseWriter, r *http.Request, sid string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST the text to paste", http.StatusMethodNotAllowed)
		return
	}
	// Only the view page posts here. A page elsewhere could not know the
	// token, but a browser that says where a request came from is believed.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "Nothing was pasted: the request did not come from the live view", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, pasteMax))
	if err != nil {
		http.Error(w, fmt.Sprintf("Nothing was pasted: the text is longer than %d KB", pasteMax>>10), http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		http.Error(w, "Nothing was pasted: the clipboard has no text", http.StatusBadRequest)
		return
	}
	s, err := h.mgr.get(sid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	wsURL := s.cdpURL.Load() // not s.mu: an agent may be waiting for this sign-in
	if wsURL == nil {
		http.Error(w, "Nothing was pasted: the session is parked; any browser tool resumes it, then reload this page", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cc, err := dialCDP(ctx, *wsURL, logf)
	if err != nil {
		http.Error(w, "Nothing was pasted: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer cc.close()
	tsid, f, err := focusedPage(ctx, cc)
	if err == nil {
		var msg string
		if msg, err = pasteInto(ctx, cc, tsid, f, string(body)); err == nil {
			s.touch()
			logf("session %s: pasted from the live view", sid)
			fmt.Fprint(w, msg)
			return
		}
	}
	http.Error(w, err.Error(), http.StatusConflict)
}

// vncPage serves noVNC's vnc.html with the paste hook added before </body>.
func (h *viewHandler) vncPage(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(filepath.Join(h.dir, "vnc.html"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := string(b)
	if i := strings.LastIndex(page, "</body>"); i >= 0 {
		page = page[:i] + "<script>\n" + pasteJS + `
chromemcpPaste(t => t instanceof HTMLCanvasElement && !!t.closest('#noVNC_container'),
  text => fetch('paste', {method: 'POST', body: text, headers: {'Content-Type': 'text/plain; charset=utf-8'}})
    .then(r => r.text(), e => 'Nothing was pasted: ' + e.message));
</script>
` + page[i:]
	} else {
		logf("noVNC's vnc.html has no </body>: the live view cannot paste")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "vnc.html", time.Time{}, strings.NewReader(page))
}

// pasteJS is the viewer's half, shared by the noVNC page and the screencast
// page. chromemcpPaste(isScreen, deliver) makes the paste shortcut over the
// remote screen -- Cmd+V on a Mac, Ctrl+V elsewhere; any other key is the
// screen's -- move the focus, for that one keystroke, to a hidden text field,
// so that the browser's own paste lands there in every browser, and hands
// the text to deliver instead of letting it in. A paste from the browser's
// menu while the screen has the focus is caught too. deliver may return a
// promise of what to tell the person, and the screen's keys wait for it;
// the function chromemcpPaste returns tells them anything later.
const pasteJS = `function chromemcpPaste(isScreen, deliver) {
  const mac = /mac|iphone|ipad|ipod/i.test((navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform);
  const sink = document.createElement('textarea'), note = document.createElement('div');
  sink.tabIndex = -1; sink.setAttribute('aria-hidden', 'true'); sink.autocomplete = 'off'; sink.spellcheck = false;
  Object.assign(sink.style, {position: 'fixed', left: '-10000px', top: '0', width: '1px', height: '1px', opacity: '0'});
  note.setAttribute('role', 'status');
  Object.assign(note.style, {position: 'fixed', left: '50%', top: '12px', transform: 'translateX(-50%)', zIndex: '2147483647',
    maxWidth: '80vw', padding: '6px 12px', borderRadius: '6px', background: 'rgba(20,20,22,.92)', color: '#e9e9e4',
    font: '13px/1.4 system-ui, sans-serif', pointerEvents: 'none', opacity: '0', transition: 'opacity .2s'});
  document.body.append(sink, note);
  let back = null, timer = 0, hide = 0, pending = 0, held = [];
  const say = msg => {
    if (!msg) return;
    note.textContent = msg; note.style.opacity = '1';
    clearTimeout(hide); hide = setTimeout(() => { note.style.opacity = '0'; }, 4000);
  };
  // Back to the screen, which keeps the keyboard.
  const restore = () => {
    clearTimeout(timer); sink.value = '';
    const b = back; back = null;
    if (b && document.activeElement === sink) b.focus({preventScroll: true});
    return b;
  };
  // While a paste is on its way, the screen's keys wait for it: an Enter
  // straight after pasting a password must not arrive before the password.
  const hold = e => {
    if (!pending || !isScreen(e.target)) return false;
    e.preventDefault(); e.stopImmediatePropagation();
    held.push([e.target, new KeyboardEvent(e.type, e)]);
    return true;
  };
  window.addEventListener('keydown', e => {
    if (hold(e) || !e.isTrusted) return; // one of ours, replayed: no paste follows it
    const v = e.key.toLowerCase() === 'v' || e.keyCode === 86;
    if (!v || e.altKey || !(mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey) || !isScreen(e.target)) return;
    e.stopImmediatePropagation(); // not the screen's: the browser pastes instead
    back = e.target; sink.focus({preventScroll: true});
    timer = setTimeout(restore, 1500); // no paste came
  }, true);
  window.addEventListener('keyup', hold, true);
  // A key that reaches the field before the paste does is the screen's.
  const forward = e => { const b = restore(); if (b) b.dispatchEvent(new KeyboardEvent(e.type, e)); };
  sink.addEventListener('keydown', forward);
  sink.addEventListener('keyup', forward);
  document.addEventListener('paste', e => {
    if (e.target !== sink && !isScreen(e.target)) return;
    e.preventDefault(); e.stopImmediatePropagation();
    restore();
    const text = e.clipboardData ? e.clipboardData.getData('text/plain') : '';
    if (!text) { say('Nothing was pasted: the clipboard has no text'); return; }
    const p = deliver(text);
    if (!p || !p.then) return;
    pending++;
    p.then(say, err => say('Nothing was pasted: ' + err)).finally(() => {
      if (--pending) return;
      for (const [t, k] of held.splice(0)) t.dispatchEvent(k);
    });
  }, true);
  return say;
}`
