package main

// Device emulation: a second, minimal CDP client on the session's browser
// websocket that auto-attaches to every target Chrome creates — pages,
// out-of-process iframes, dedicated / shared / service workers — and applies
// the session's device profile to each before it runs a line of script.
//
// It is separate from the chromedp contexts that drive the tabs for two
// reasons. Emulation must land before a target's first request or script
// (chromedp adopts tabs by polling, which is late for a popup's navigation),
// and the user agent a page reports has to hold in its workers and service
// workers too, which chromedp never attaches to. Target.setAutoAttach with
// waitForDebuggerOnStart gives both: Chrome pauses each new target until
// Runtime.runIfWaitingForDebugger, and this client releases it only once
// the overrides are in place.
//
// Overrides live as long as the session that set them, so the connection is
// held open for the life of the Chrome; a parked session starts a new one
// on resume.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/coder/websocket"
)

// cdpConn is a flat-session CDP client: commands addressed to the browser
// or to a session, events queued to a handler that may itself send commands.
type cdpConn struct {
	c      *websocket.Conn
	logf   func(string, ...any)
	next   atomic.Int64
	mu     sync.Mutex
	pend   map[int64]chan *cdproto.Message
	queue  []*cdproto.Message
	wake   chan struct{}
	closed chan struct{}
	err    error
}

func dialCDP(ctx context.Context, wsURL string, logf func(string, ...any)) (*cdpConn, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing devtools: %w", err)
	}
	c.SetReadLimit(64 << 20)
	conn := &cdpConn{c: c, logf: logf, pend: map[int64]chan *cdproto.Message{}, wake: make(chan struct{}, 1), closed: make(chan struct{})}
	go conn.read()
	return conn, nil
}

// read delivers responses to their waiters and queues events for dispatch.
func (cc *cdpConn) read() {
	defer close(cc.closed)
	for {
		_, data, err := cc.c.Read(context.Background())
		if err != nil {
			cc.mu.Lock()
			cc.err = err
			for id, ch := range cc.pend {
				close(ch)
				delete(cc.pend, id)
			}
			cc.mu.Unlock()
			cc.signal()
			return
		}
		var msg cdproto.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		cc.mu.Lock()
		if msg.ID != 0 {
			if ch, ok := cc.pend[msg.ID]; ok {
				ch <- &msg
				delete(cc.pend, msg.ID)
			}
		} else if msg.Method != "" {
			cc.queue = append(cc.queue, &msg)
		}
		cc.mu.Unlock()
		if msg.ID == 0 {
			cc.signal()
		}
	}
}

func (cc *cdpConn) signal() {
	select {
	case cc.wake <- struct{}{}:
	default:
	}
}

// events runs handler on queued events, in order, until the connection
// ends. It must run on its own goroutine: the handler may call cc.call.
func (cc *cdpConn) events(handler func(*cdproto.Message)) {
	for {
		cc.mu.Lock()
		q := cc.queue
		cc.queue = nil
		done := cc.err != nil
		cc.mu.Unlock()
		for _, m := range q {
			handler(m)
		}
		if done {
			return
		}
		<-cc.wake
	}
}

// send issues a command and returns a channel that yields its outcome
// once, when the response arrives or the context ends. sessionID ""
// addresses the browser.
func (cc *cdpConn) send(ctx context.Context, sessionID target.SessionID, method string, params any) <-chan error {
	errc := make(chan error, 1)
	_, err := cc.issue(ctx, sessionID, method, params, nil, errc)
	if err != nil {
		errc <- err
	}
	return errc
}

// call sends a command and waits for its response, decoding the result
// into out when given.
func (cc *cdpConn) call(ctx context.Context, sessionID target.SessionID, method string, params any, out any) error {
	errc := make(chan error, 1)
	if _, err := cc.issue(ctx, sessionID, method, params, out, errc); err != nil {
		return err
	}
	return <-errc
}

// issue writes a command and arranges for its response to be decoded into
// out and reported on errc.
func (cc *cdpConn) issue(ctx context.Context, sessionID target.SessionID, method string, params any, out any, errc chan<- error) (int64, error) {
	id := cc.next.Add(1)
	msg := cdproto.Message{ID: id, SessionID: sessionID, Method: cdproto.MethodType(method)}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		msg.Params = b
	}
	b, err := json.Marshal(&msg)
	if err != nil {
		return 0, err
	}
	ch := make(chan *cdproto.Message, 1)
	cc.mu.Lock()
	if cc.err != nil {
		cc.mu.Unlock()
		return 0, fmt.Errorf("devtools connection closed: %w", cc.err)
	}
	cc.pend[id] = ch
	cc.mu.Unlock()
	if err := cc.c.Write(ctx, websocket.MessageText, b); err != nil {
		cc.mu.Lock()
		delete(cc.pend, id)
		cc.mu.Unlock()
		return 0, err
	}
	go func() {
		select {
		case resp, ok := <-ch:
			switch {
			case !ok:
				errc <- errors.New("devtools connection closed")
			case resp.Error != nil:
				errc <- fmt.Errorf("%s: %s", method, resp.Error.Message)
			case out != nil && len(resp.Result) > 0:
				errc <- json.Unmarshal(resp.Result, out)
			default:
				errc <- nil
			}
		case <-ctx.Done():
			cc.mu.Lock()
			delete(cc.pend, id)
			cc.mu.Unlock()
			errc <- ctx.Err()
		}
	}()
	return id, nil
}

func (cc *cdpConn) close() {
	cc.c.Close(websocket.StatusNormalClosure, "")
	<-cc.closed
}

// emulationSpec is what the emulator applies to each target.
type emulationSpec struct {
	// UserAgent, when set, overrides the user agent string and the
	// client-hint metadata in every target, workers included.
	UserAgent *emulation.SetUserAgentOverrideParams
	// Metrics, when set, is the page viewport and screen (headless: the
	// viewport is emulated; headful: width/height 0 keep the window's).
	Metrics *emulation.SetDeviceMetricsOverrideParams
	// Script runs in every new document of every page and frame before the
	// page's own scripts.
	Script string
	// CHHeaders, when set, are low-entropy client-hint request headers
	// (lowercase name → value) stamped onto every request the browser makes,
	// so a tab's first navigation — which the per-target override cannot
	// reach — carries the profile's platform. Enables browser-level request
	// interception.
	CHHeaders map[string]string
}

// emulator applies an emulationSpec to every target of one Chrome.
type emulator struct {
	conn *cdpConn
	spec *emulationSpec
	logf func(string, ...any)

	mu      sync.Mutex
	initial []chan struct{} // applies in flight for the targets found at start; nil once started
	closing bool
}

// startEmulator connects to the browser websocket, attaches to the targets
// that already exist (with the spec applied to them by the time it returns)
// and goes on attaching to new ones until the connection ends.
func startEmulator(ctx context.Context, wsURL string, spec *emulationSpec, logf func(string, ...any)) (*emulator, error) {
	conn, err := dialCDP(ctx, wsURL, logf)
	if err != nil {
		return nil, err
	}
	e := &emulator{conn: conn, spec: spec, logf: logf, initial: []chan struct{}{}}
	// Existing targets attach as a burst of events before the command
	// answers; a marker queued behind them separates them from the rest.
	ready := make(chan []chan struct{}, 1)
	go func() {
		conn.events(func(m *cdproto.Message) {
			if m.Method == "" && m.ID == -1 {
				e.mu.Lock()
				ready <- e.initial
				e.initial = nil
				e.mu.Unlock()
				return
			}
			e.handle(m)
		})
		e.mu.Lock()
		quiet := e.closing
		e.mu.Unlock()
		if !quiet && logf != nil {
			logf("emulation: devtools connection lost; new targets go unemulated")
		}
	}()
	if err := e.setAutoAttach(ctx, ""); err != nil {
		conn.close()
		return nil, err
	}
	// Browser-level request interception, to stamp the low-entropy client
	// hints onto requests the per-target override cannot reach — a new tab's
	// or popup's first navigation, which the browser dispatches before the
	// target exists. Enabled on the browser session, it sees those.
	if len(spec.CHHeaders) > 0 {
		if err := e.conn.call(ctx, "", "Fetch.enable", map[string]any{
			"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}},
		}, nil); err != nil {
			conn.close()
			return nil, fmt.Errorf("emulation: enabling request interception: %w", err)
		}
	}
	conn.mu.Lock()
	conn.queue = append(conn.queue, &cdproto.Message{ID: -1})
	conn.mu.Unlock()
	conn.signal()
	deadline := time.After(15 * time.Second)
	var applies []chan struct{}
	select {
	case applies = <-ready:
	case <-ctx.Done():
		conn.close()
		return nil, ctx.Err()
	case <-deadline:
		conn.close()
		return nil, errors.New("emulation: timed out attaching to the initial targets")
	}
	for _, done := range applies {
		select {
		case <-done:
		case <-ctx.Done():
			conn.close()
			return nil, ctx.Err()
		case <-deadline:
			conn.close()
			return nil, errors.New("emulation: timed out applying to the initial targets")
		}
	}
	return e, nil
}

func (e *emulator) close() {
	if e == nil || e.conn == nil {
		return
	}
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	e.conn.close()
}

// autoAttachParams asks for every target related to the one addressed,
// paused until released. Chrome's own UI surfaces are of no interest.
func autoAttachParams() *target.SetAutoAttachParams {
	return &target.SetAutoAttachParams{
		AutoAttach: true, WaitForDebuggerOnStart: true, Flatten: true,
		Filter: target.Filter{
			{Type: "browser", Exclude: true}, {Type: "tab", Exclude: true},
			{Type: "browser_ui", Exclude: true}, {Type: "background_page", Exclude: true},
			{},
		},
	}
}

func (e *emulator) setAutoAttach(ctx context.Context, session target.SessionID) error {
	return e.conn.call(ctx, session, "Target.setAutoAttach", autoAttachParams(), nil)
}

func (e *emulator) handle(m *cdproto.Message) {
	switch m.Method {
	case cdproto.EventTargetAttachedToTarget:
		var ev target.EventAttachedToTarget
		if json.Unmarshal(m.Params, &ev) != nil || ev.TargetInfo == nil {
			return
		}
		done := make(chan struct{})
		e.mu.Lock()
		if e.initial != nil {
			e.initial = append(e.initial, done)
		}
		e.mu.Unlock()
		// Each target on its own goroutine: a target that never answers (an
		// extension's idle service worker) must not hold up the rest.
		go func() {
			defer close(done)
			e.apply(ev.SessionID, ev.TargetInfo, ev.WaitingForDebugger)
		}()
	case "Fetch.requestPaused":
		var ev struct {
			RequestID string `json:"requestId"`
			Request   struct {
				Headers map[string]string `json:"headers"`
			} `json:"request"`
		}
		if json.Unmarshal(m.Params, &ev) != nil {
			return
		}
		// A paused request must always be released, or the page hangs; do it
		// off the event loop so interception never blocks target attachment.
		go e.stamp(m.SessionID, ev.RequestID, ev.Request.Headers)
	}
}

// stamp rewrites the client-hint headers already present on a paused request
// to the profile's values and continues it. The request is continued
// whatever happens; a header only Chrome added but the profile does not
// change is left as it is.
func (e *emulator) stamp(sid target.SessionID, requestID string, headers map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	changed := false
	var list []map[string]string
	for name, value := range headers {
		if want, ok := e.spec.CHHeaders[strings.ToLower(name)]; ok && value != want {
			value = want
			changed = true
		}
		list = append(list, map[string]string{"name": name, "value": value})
	}
	args := map[string]any{"requestId": requestID}
	if changed {
		args["headers"] = list
	}
	err := <-e.conn.send(ctx, sid, "Fetch.continueRequest", args)
	if err == nil || e.logf == nil {
		return
	}
	// Parking closes the connection with requests still paused; those
	// failures are expected and Chrome drops interception on its own.
	e.mu.Lock()
	closing := e.closing
	e.mu.Unlock()
	if !closing {
		e.logf("emulation: continuing request: %v", err)
	}
}

// internalTarget is one of Chrome's own: a component extension, a chrome://
// page. Nothing is emulated there.
func internalTarget(info *target.Info) bool {
	for _, p := range []string{"chrome-extension://", "chrome://", "devtools://", "chrome-untrusted://"} {
		if strings.HasPrefix(info.URL, p) {
			return true
		}
	}
	return false
}

// apply puts the spec on one newly attached target and releases it.
//
// Nothing is waited for before the release: Chrome runs a session's
// commands in order, so the overrides land before the target's first
// script either way, and several of them (device metrics, the user agent)
// are answered by the renderer, which for a new page is held until the
// release itself. The replies are collected afterwards, for the log.
func (e *emulator) apply(sid target.SessionID, info *target.Info, paused bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type pending struct {
		what string
		err  <-chan error
	}
	var sent []pending
	send := func(what, method string, params any) {
		sent = append(sent, pending{what, e.conn.send(ctx, sid, method, params)})
	}
	if !internalTarget(info) {
		isPage := info.Type == "page"
		isFrame := isPage || info.Type == "iframe"
		if isFrame && e.spec.Script != "" {
			// Blink's page agent only hears about new documents — and so
			// only runs the init script in them — once enabled.
			send("page", "Page.enable", nil)
		}
		if e.spec.UserAgent != nil {
			// Network's form of the command is the one worker targets
			// accept; for frames Chrome routes it to Emulation's.
			send("user agent", "Network.setUserAgentOverride", e.spec.UserAgent)
		}
		if isPage && e.spec.Metrics != nil {
			send("device metrics", "Emulation.setDeviceMetricsOverride", e.spec.Metrics)
		}
		if isFrame && e.spec.Script != "" {
			send("init script", "Page.addScriptToEvaluateOnNewDocument",
				&page.AddScriptToEvaluateOnNewDocumentParams{Source: e.spec.Script, RunImmediately: true})
		} else if !isFrame && e.spec.Script != "" && paused {
			// Workers have no new-document hook; a paused one evaluates
			// before its own script runs.
			send("init script", "Runtime.evaluate", map[string]any{"expression": e.spec.Script})
		}
		// The target's own children (frames, workers) come through it.
		send("auto-attach", "Target.setAutoAttach", autoAttachParams())
	}
	if paused {
		e.conn.send(ctx, sid, "Runtime.runIfWaitingForDebugger", nil)
	}
	for _, p := range sent {
		select {
		case err := <-p.err:
			if err != nil && e.logf != nil && !(p.what == "auto-attach" && info.Type != "page" && info.Type != "iframe") {
				e.logf("emulation: %s %s %s: %v", p.what, info.Type, info.URL, err)
			}
		case <-ctx.Done():
			return
		}
	}
}
