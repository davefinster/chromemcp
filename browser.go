package main

// Driving a tab: navigation, the element snapshot, clicking and typing by
// ref / selector / text / coordinates, keys, scrolling, screenshots, text
// extraction, JavaScript. Every operation runs under the session lock and
// ends with a result the MCP layer renders as text plus, usually, a
// screenshot: the agent is meant to see the page after each step.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// result is what a tool hands back: lines of text and an optional image.
type result struct {
	lines []string
	image []byte
	mime  string
}

func (r *result) addf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// target names an element three ways; exactly one is used.
type targetSpec struct {
	Ref      string `json:"ref,omitempty" jsonschema:"an element ref from browser_snapshot, e.g. e12"`
	Selector string `json:"selector,omitempty" jsonschema:"a CSS selector, when there is no ref for it"`
	Text     string `json:"text,omitempty" jsonschema:"visible text of the element (exact, else substring; the innermost match, promoted to its link/button)"`
}

func (t targetSpec) empty() bool { return t.Ref == "" && t.Selector == "" && t.Text == "" }

func (t targetSpec) String() string {
	switch {
	case t.Ref != "":
		return t.Ref
	case t.Selector != "":
		return "selector " + strconv.Quote(t.Selector)
	default:
		return "text " + strconv.Quote(t.Text)
	}
}

// screenshotOpts control a capture.
type screenshotOpts struct {
	FullPage bool
	Format   string  // jpeg (default) or png
	Quality  int     // jpeg quality, default 70
	Scale    float64 // 0 < scale <= 1, default 1
	Labels   bool    // draw the refs of the last snapshot
	Clip     *[4]int // viewport-relative CSS px box to crop to
}

// withTab runs fn on a tab of a session, resuming the session if parked,
// with the session locked. tabRef "" is the current tab.
func (m *manager) withTab(ctx context.Context, sessionID, tabRef string, fn func(ctx context.Context, s *session, t *tab) error) error {
	s, err := m.running(ctx, sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch()
	if !s.isRunning() {
		return fmt.Errorf("session %s: chrome exited; session_resume restarts it", sessionID)
	}
	if err := s.syncTabs(ctx); err != nil {
		return err
	}
	t, err := s.resolveTab(tabRef)
	if err != nil {
		return err
	}
	s.focus(ctx, t)
	return fn(ctx, s, t)
}

// focus brings a tab to the front before it is worked on. Headless Chrome
// treats every tab but the front one as hidden and stops rendering it, and
// a screenshot of a hidden tab waits for a frame that never comes.
func (s *session) focus(ctx context.Context, t *tab) {
	if s.front == t.id {
		return
	}
	if err := s.bringToFront(ctx, t); err != nil {
		logf("session %s: bringing %s to front: %v", s.meta.ID, t.alias, err)
		return
	}
	s.front = t.id
}

// header is the first line of every result: where the agent is.
func (s *session) header(ctx context.Context, t *tab) string {
	var u, title string
	tctx, cancel := context.WithTimeout(t.ctx, 3*time.Second)
	chromedp.Run(tctx, chromedp.Location(&u), chromedp.Title(&title))
	cancel()
	s.meta.LastURL, s.meta.LastTitle = u, title
	tabs := ""
	if len(s.tabs) > 1 {
		tabs = fmt.Sprintf(", %d tabs", len(s.tabs))
	}
	return fmt.Sprintf("[%s %s%s] %s — %q", s.meta.ID, t.alias, tabs, u, title)
}

// footer appends what happened on the side: dialogs answered, console
// errors, tabs that appeared.
func (s *session) footer(r *result, t *tab) {
	for _, d := range t.takeDialogs() {
		r.addf("note: %s", d)
	}
}

// settle waits for the document to finish loading, tolerating the moment
// when a navigation has destroyed the old execution context.
func settle(ctx context.Context, t *tab, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		var state string
		ectx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
		err := chromedp.Run(ectx, chromedp.Evaluate("document.readyState", &state))
		cancel()
		if err == nil && state == "complete" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func normalizeURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", errors.New("url is required")
	}
	if !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") && !strings.HasPrefix(u, "chrome:") && !strings.HasPrefix(u, "data:") && !strings.HasPrefix(u, "file:") {
		u = "https://" + u
	}
	if _, err := url.Parse(u); err != nil {
		return "", fmt.Errorf("url %q: %w", u, err)
	}
	return u, nil
}

// navigate loads a URL in the tab and waits for it.
func navigate(ctx context.Context, t *tab, u string, timeout time.Duration) error {
	nctx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()
	err := chromedp.Run(nctx, chromedp.Navigate(u))
	if err != nil && errors.Is(nctx.Err(), context.DeadlineExceeded) {
		// The page is still loading; what is there is still useful.
		return nil
	}
	if err != nil {
		return fmt.Errorf("navigating to %s: %w", u, err)
	}
	settle(ctx, t, 3*time.Second)
	return nil
}

// evalJSON evaluates an expression and decodes its JSON value into out.
func evalJSON(ctx context.Context, t *tab, expr string, out any) error {
	ectx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := chromedp.Run(ectx, chromedp.Evaluate(expr, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true).WithReturnByValue(true)
	})); err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// snapshot takes the element snapshot of the tab.
func snapshot(ctx context.Context, t *tab, max int, viewportOnly bool) (*snapResult, error) {
	var res snapResult
	if err := evalJSON(ctx, t, jsCall("snapshot", map[string]any{"max": max, "viewportOnly": viewportOnly}), &res); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return &res, nil
}

// resolved is the page's answer to a target lookup.
type resolved struct {
	Error string  `json:"error"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	How   string  `json:"how"`
	Tag   string  `json:"tag"`
	Role  string  `json:"role"`
	Name  string  `json:"name"`
	Box   [4]int  `json:"box"`
}

func (r *resolved) describe() string {
	d := r.How
	if r.Role != "" {
		d += " " + r.Role
	} else {
		d += " <" + r.Tag + ">"
	}
	if r.Name != "" {
		d += " " + strconv.Quote(r.Name)
	}
	return d
}

func resolveTarget(ctx context.Context, t *tab, spec targetSpec) (*resolved, error) {
	if spec.empty() {
		return nil, errors.New("give one of ref, selector or text (or x and y)")
	}
	var r resolved
	if err := evalJSON(ctx, t, jsCall("resolve", spec), &r); err != nil {
		return nil, fmt.Errorf("finding %s: %w", spec, err)
	}
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	return &r, nil
}

// clickAt dispatches a real mouse click at viewport coordinates.
func clickAt(ctx context.Context, t *tab, x, y float64, button string, count int, mods input.Modifier) error {
	var btn input.MouseButton
	switch strings.ToLower(button) {
	case "", "left":
		btn = input.Left
	case "right":
		btn = input.Right
	case "middle":
		btn = input.Middle
	default:
		return fmt.Errorf("button %q: want left, right or middle", button)
	}
	if count <= 0 {
		count = 1
	}
	cctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(cctx,
		chromedp.MouseEvent(input.MouseMoved, x, y, chromedp.ButtonModifiers(mods)),
		chromedp.ActionFunc(func(ctx context.Context) error {
			for i := 1; i <= count; i++ {
				if err := input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(btn).WithClickCount(int64(i)).WithModifiers(mods).Do(ctx); err != nil {
					return err
				}
				if err := input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(btn).WithClickCount(int64(i)).WithModifiers(mods).Do(ctx); err != nil {
					return err
				}
			}
			return nil
		}),
	)
}

func hoverAt(ctx context.Context, t *tab, x, y float64) error {
	cctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.MouseEvent(input.MouseMoved, x, y))
}

// insertText types into whatever has focus, as an IME would: one input
// event per call, which every framework understands.
func insertText(ctx context.Context, t *tab, text string) error {
	cctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	}))
}

// namedKeys maps the names an agent will use to chromedp's key runes.
var namedKeys = map[string]string{
	"enter": kb.Enter, "return": kb.Enter, "tab": kb.Tab, "escape": kb.Escape, "esc": kb.Escape,
	"backspace": kb.Backspace, "delete": kb.Delete, "del": kb.Delete, "space": " ",
	"arrowup": kb.ArrowUp, "up": kb.ArrowUp, "arrowdown": kb.ArrowDown, "down": kb.ArrowDown,
	"arrowleft": kb.ArrowLeft, "left": kb.ArrowLeft, "arrowright": kb.ArrowRight, "right": kb.ArrowRight,
	"home": kb.Home, "end": kb.End, "pageup": kb.PageUp, "pagedown": kb.PageDown, "insert": kb.Insert,
	"f1": kb.F1, "f2": kb.F2, "f3": kb.F3, "f4": kb.F4, "f5": kb.F5, "f6": kb.F6,
	"f7": kb.F7, "f8": kb.F8, "f9": kb.F9, "f10": kb.F10, "f11": kb.F11, "f12": kb.F12,
}

// parseKey turns "Control+Shift+a" into a key definition and modifiers.
func parseKey(spec string) (*kb.Key, input.Modifier, error) {
	parts := strings.Split(strings.TrimSpace(spec), "+")
	if len(parts) == 1 && spec == "+" {
		parts = []string{"+"}
	}
	var mods input.Modifier
	for _, p := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "control", "ctrl":
			mods |= input.ModifierCtrl
		case "shift":
			mods |= input.ModifierShift
		case "alt", "option":
			mods |= input.ModifierAlt
		case "meta", "cmd", "command", "super", "win":
			mods |= input.ModifierMeta
		default:
			return nil, 0, fmt.Errorf("modifier %q: want Control, Shift, Alt or Meta", p)
		}
	}
	name := parts[len(parts)-1]
	var r rune
	if s, ok := namedKeys[strings.ToLower(name)]; ok {
		r = []rune(s)[0]
	} else {
		rs := []rune(name)
		if len(rs) != 1 {
			return nil, 0, fmt.Errorf("key %q: a single character or a name like Enter, Tab, Escape, ArrowDown, F5", name)
		}
		r = rs[0]
	}
	k, ok := kb.Keys[r]
	if !ok {
		// A character Chrome has no key definition for: type it instead.
		k = &kb.Key{Key: string(r), Text: string(r), Unmodified: string(r), Print: true}
	}
	if k.Shift {
		mods |= input.ModifierShift
	}
	return k, mods, nil
}

// pressKey sends keydown/keyup for one key with modifiers, producing text
// only when the combination would (no Control/Alt/Meta held).
func pressKey(ctx context.Context, t *tab, k *kb.Key, mods input.Modifier) error {
	down := &input.DispatchKeyEventParams{
		Type:                  input.KeyRawDown,
		Key:                   k.Key,
		Code:                  k.Code,
		WindowsVirtualKeyCode: k.Windows,
		NativeVirtualKeyCode:  k.Native,
		Modifiers:             mods,
	}
	if k.Text != "" && mods&^input.ModifierShift == 0 {
		down.Type = input.KeyDown
		down.Text = k.Text
		down.UnmodifiedText = k.Unmodified
	}
	up := *down
	up.Type = input.KeyUp
	up.Text, up.UnmodifiedText = "", ""
	cctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := down.Do(ctx); err != nil {
			return err
		}
		return up.Do(ctx)
	}))
}

// scrollBy sends a wheel event at a point; pages that lazy-load react to
// it the way they react to a person.
func scrollBy(ctx context.Context, t *tab, x, y, dx, dy float64) error {
	cctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(input.MouseWheel, x, y).WithDeltaX(dx).WithDeltaY(dy).Do(ctx)
	}))
}

// viewportSize is the page's inner size in CSS pixels.
func viewportSize(ctx context.Context, t *tab) (w, h int, err error) {
	var wh [2]int
	if err := evalJSON(ctx, t, "[innerWidth, innerHeight]", &wh); err != nil {
		return 0, 0, err
	}
	return wh[0], wh[1], nil
}

// screenshot captures the tab.
func screenshot(ctx context.Context, t *tab, o screenshotOpts) ([]byte, string, error) {
	format := page.CaptureScreenshotFormatJpeg
	mime := "image/jpeg"
	if strings.EqualFold(o.Format, "png") {
		format, mime = page.CaptureScreenshotFormatPng, "image/png"
	}
	quality := o.Quality
	if quality <= 0 || quality > 100 {
		quality = 70
	}
	if o.Labels {
		var n int
		if err := evalJSON(ctx, t, jsCall("label", true), &n); err != nil {
			return nil, "", fmt.Errorf("drawing labels: %w", err)
		}
		defer evalJSON(context.Background(), t, jsCall("label", false), nil)
	}
	ensureVisible(ctx, t)
	var buf []byte
	sctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	if o.FullPage {
		q := quality
		if format == page.CaptureScreenshotFormatPng {
			q = 100 // chromedp picks png at 100
		}
		if err := chromedp.Run(sctx, chromedp.FullScreenshot(&buf, q)); err != nil {
			return nil, "", fmt.Errorf("screenshot: %w", err)
		}
	} else {
		err := chromedp.Run(sctx, chromedp.ActionFunc(func(ctx context.Context) error {
			p := page.CaptureScreenshot().WithFormat(format).WithCaptureBeyondViewport(false)
			if format == page.CaptureScreenshotFormatJpeg {
				p = p.WithQuality(int64(quality))
			}
			var err error
			buf, err = p.Do(ctx)
			return err
		}))
		if err != nil {
			return nil, "", fmt.Errorf("screenshot: %w", err)
		}
	}
	if o.Clip != nil || (o.Scale > 0 && o.Scale < 1) {
		var err error
		buf, err = transformImage(buf, mime, o.Clip, o.Scale, quality)
		if err != nil {
			return nil, "", err
		}
	}
	return buf, mime, nil
}

// transformImage crops and/or downscales an encoded image. Cropping is by
// a CSS-pixel box; the image is in device pixels, and the two match here
// (device scale factor 1), which the crop assumes.
func transformImage(data []byte, mime string, clip *[4]int, scale float64, quality int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding screenshot: %w", err)
	}
	if clip != nil {
		b := img.Bounds()
		r := image.Rect(clip[0], clip[1], clip[0]+clip[2], clip[1]+clip[3]).Intersect(b)
		if r.Empty() {
			return nil, errors.New("the element is outside the captured area")
		}
		type subImager interface {
			SubImage(image.Rectangle) image.Image
		}
		if si, ok := img.(subImager); ok {
			img = si.SubImage(r)
		}
	}
	if scale > 0 && scale < 1 {
		img = downscale(img, scale)
	}
	var out bytes.Buffer
	if mime == "image/png" {
		err = png.Encode(&out, img)
	} else {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: quality})
	}
	return out.Bytes(), err
}

// downscale is a box filter: adequate for screenshots an agent reads, and
// dependency-free.
func downscale(src image.Image, scale float64) image.Image {
	b := src.Bounds()
	w := int(float64(b.Dx())*scale + 0.5)
	h := int(float64(b.Dy())*scale + 0.5)
	if w < 1 || h < 1 {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	fx := float64(b.Dx()) / float64(w)
	fy := float64(b.Dy()) / float64(h)
	for y := 0; y < h; y++ {
		sy0 := b.Min.Y + int(float64(y)*fy)
		sy1 := b.Min.Y + int(float64(y+1)*fy)
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < w; x++ {
			sx0 := b.Min.X + int(float64(x)*fx)
			sx1 := b.Min.X + int(float64(x+1)*fx)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := sy0; yy < sy1 && yy < b.Max.Y; yy++ {
				for xx := sx0; xx < sx1 && xx < b.Max.X; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r += uint64(cr >> 8)
					g += uint64(cg >> 8)
					bl += uint64(cb >> 8)
					a += uint64(ca >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n)
			dst.Pix[i+1] = uint8(g / n)
			dst.Pix[i+2] = uint8(bl / n)
			dst.Pix[i+3] = uint8(a / n)
		}
	}
	return dst
}

// readText is the page's rendered text.
type textResult struct {
	Error     string `json:"error"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
}

func readText(ctx context.Context, t *tab, selector string, max int) (*textResult, error) {
	var r textResult
	if err := evalJSON(ctx, t, jsCall("readText", selector, max), &r); err != nil {
		return nil, fmt.Errorf("reading text: %w", err)
	}
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	return &r, nil
}

// evaluate runs arbitrary JavaScript and renders its value.
func evaluate(ctx context.Context, t *tab, expr string, timeout time.Duration) (string, error) {
	ectx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()
	var out string
	err := chromedp.Run(ectx, chromedp.ActionFunc(func(ctx context.Context) error {
		res, exc, err := runtime.Evaluate(expr).WithAwaitPromise(true).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			msg := exc.Text
			if exc.Exception != nil {
				msg = remoteObjectString(exc.Exception)
			}
			return fmt.Errorf("javascript threw: %s", msg)
		}
		switch {
		case res == nil:
			out = "undefined"
		case res.Value != nil:
			var v any
			if json.Unmarshal(res.Value, &v) == nil {
				b, _ := json.MarshalIndent(v, "", "  ")
				out = string(b)
			} else {
				out = string(res.Value)
			}
		case res.Description != "":
			out = fmt.Sprintf("(%s) %s", res.Type, res.Description)
		default:
			out = string(res.Type)
		}
		return nil
	}))
	return out, err
}

// waitFor polls until a condition holds or the timeout passes.
type waitSpec struct {
	Selector    string
	Text        string
	URLContains string
	Gone        bool // wait for the selector/text to disappear instead
}

func waitFor(ctx context.Context, t *tab, w waitSpec, timeout time.Duration) (string, error) {
	var expr string
	switch {
	case w.Selector != "":
		expr = fmt.Sprintf(`(() => { const el = document.querySelector(%s); if (!el) return false; const r = el.getBoundingClientRect(); const st = getComputedStyle(el); return r.width > 0 && r.height > 0 && st.visibility !== 'hidden' && st.display !== 'none'; })()`, strconv.Quote(w.Selector))
	case w.Text != "":
		expr = fmt.Sprintf(`(document.body && document.body.innerText || '').toLowerCase().includes(%s.toLowerCase())`, strconv.Quote(w.Text))
	case w.URLContains != "":
		expr = fmt.Sprintf(`location.href.includes(%s)`, strconv.Quote(w.URLContains))
	default:
		return "", errors.New("give one of selector, text or url_contains (or just ms)")
	}
	if w.Gone {
		expr = "!(" + expr + ")"
	}
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		err := evalJSON(ctx, t, expr, &ok)
		if err == nil && ok {
			return "condition met", nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("timed out after %s (last error: %v)", timeout, err)
			}
			return "", fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// bringToFront makes a tab the visible one and the current one.
func (s *session) bringToFront(ctx context.Context, t *tab) error {
	s.current = t.id
	cctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	err := chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.BringToFront().Do(ctx)
	}))
	if err == nil {
		s.front = t.id
	}
	return err
}

// ensureVisible is the safety net before a capture: a tab that reports
// itself hidden is brought to the front first.
func ensureVisible(ctx context.Context, t *tab) {
	var state string
	if err := evalJSON(ctx, t, "document.visibilityState", &state); err == nil && state == "visible" {
		return
	}
	cctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.BringToFront().Do(ctx)
	}))
}

// tabList renders the session's tabs.
func (s *session) tabList(ctx context.Context) (string, error) {
	infos, err := chromedp.Targets(s.browserCtx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	n := 0
	for _, t := range s.sortedTabs() {
		for _, info := range infos {
			if info.TargetID != t.id {
				continue
			}
			mark := " "
			if t.id == s.current {
				mark = "*"
			}
			fmt.Fprintf(&sb, "%s %s  %s — %q\n", mark, t.alias, info.URL, info.Title)
			n++
		}
	}
	if n == 0 {
		return "no tabs open", nil
	}
	return fmt.Sprintf("%d tabs in %s (* = current):\n%s", n, s.meta.ID, sb.String()), nil
}

func (s *session) sortedTabs() []*tab {
	out := make([]*tab, 0, len(s.tabs))
	for _, t := range s.tabs {
		out = append(out, t)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].seq < out[j-1].seq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// consoleText renders a tab's collected console since the last clear.
func (t *tab) consoleText(clear bool, levels string) string {
	t.mu.Lock()
	entries := append([]consoleEntry(nil), t.console...)
	if clear {
		t.console = nil
	}
	t.mu.Unlock()
	want := map[string]bool{}
	for _, l := range strings.Split(levels, ",") {
		if l = strings.TrimSpace(strings.ToLower(l)); l != "" {
			want[l] = true
		}
	}
	var sb strings.Builder
	n := 0
	for _, e := range entries {
		if len(want) > 0 && !want[e.Level] && !(want["error"] && e.Level == "exception") {
			continue
		}
		fmt.Fprintf(&sb, "%s [%s] %s\n", e.When.Format("15:04:05.000"), e.Level, truncate(e.Text, 2000))
		n++
	}
	if n == 0 {
		return "no console output collected"
	}
	return fmt.Sprintf("%d console entries:\n%s", n, sb.String())
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary.
	i := max
	for i > 0 && !unicode.IsSpace(rune(s[i])) && i > max-40 {
		i--
	}
	return s[:i] + "…"
}

// pageInfo is a cheap URL/title read used in listings.
func (s *session) pageInfo() (string, string) {
	if !s.isRunning() || s.browserCtx == nil {
		return s.meta.LastURL, s.meta.LastTitle
	}
	ctx, cancel := context.WithTimeout(s.browserCtx, 3*time.Second)
	defer cancel()
	infos, err := chromedp.Targets(ctx)
	if err != nil {
		return s.meta.LastURL, s.meta.LastTitle
	}
	for _, info := range infos {
		if info.TargetID == s.current {
			return info.URL, info.Title
		}
	}
	return s.meta.LastURL, s.meta.LastTitle
}
