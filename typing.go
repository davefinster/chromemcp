package main

// Typing the way a person types (the keyboard half of human.go). A session
// has one typist, with a speed and a rhythm of their own. Each key goes
// down and comes up: keydown, keypress, the input and keyup a page listens
// for, with the key held as long as a finger holds it. Keys follow each
// other at intervals that vary the way a person's do: quicker across hands,
// slower on the same finger, a beat at a word, longer after a sentence, a
// pause to think now and then. A fast typist presses the next key before
// releasing the last, which falls out of laying presses and releases on one
// timeline. Capitals and symbols get a real Shift, pressed a moment before
// and released after, with the keyCode, code and location a keyboard gives
// it. Chords (Control+a, Shift+Tab) press their modifiers as keys of their
// own.
//
// What has no key on a US keyboard (é, emoji, CJK) goes in whole, as an IME
// or the character viewer inserts it. A newline is Enter in a textarea or an
// editor, and left out of a one-line field, which would drop it anyway. A tab
// is inserted, not pressed, because the Tab key would leave the field. Text
// longer than typeMax goes in at once, as a paste, so a tool call does not
// type for minutes. Everything goes out on the hand's connection
// (human.go): in order, at its times, without waiting on a slow link.

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp/kb"
)

// typeMax is the longest text typed key by key; longer is pasted. About
// half a minute at a typist's pace.
const typeMax = 160

// keyDef is a key as a keyboard reports it.
type keyDef struct {
	key, code, text, unmod string
	vk                     int64 // keyCode, as on Windows (never the native code: keyParams)
	location               int64 // 1 for the left-hand modifier keys
	shift                  bool  // typed with Shift
}

// The modifiers as keys, in the order a hand presses them. The keyCodes are
// the DOM's (16, 17, 18, 91), not chromedp's left-hand Windows codes
// (160...), which no page ever sees from a keyboard.
var modifierKeys = []struct {
	mod input.Modifier
	key keyDef
}{
	{input.ModifierCtrl, keyDef{key: "Control", code: "ControlLeft", vk: 17, location: 1}},
	{input.ModifierAlt, keyDef{key: "Alt", code: "AltLeft", vk: 18, location: 1}},
	{input.ModifierMeta, keyDef{key: "Meta", code: "MetaLeft", vk: 91, location: 1}},
	{input.ModifierShift, keyDef{key: "Shift", code: "ShiftLeft", vk: 16, location: 1}},
}

var shiftKey = modifierKeys[3].key

func fromKB(k *kb.Key) keyDef {
	return keyDef{key: k.Key, code: k.Code, text: k.Text, unmod: k.Unmodified, vk: k.Windows, shift: k.Shift}
}

// keyOf is the key that types r on a US keyboard, if there is one.
func keyOf(r rune) (keyDef, bool) {
	switch r {
	case '\t':
		return keyDef{}, false
	case '\n':
		r = '\r' // the table has Enter only as \r
	}
	k, ok := kb.Keys[r]
	if !ok || !k.Print || k.Text == "" {
		return keyDef{}, false
	}
	return fromKB(k), true
}

// keyEvent is one thing the keyboard does, at a time from the start.
type keyEvent struct {
	at       time.Duration
	down     bool
	key      keyDef
	mods     input.Modifier
	insert   string   // text that goes in whole instead of a key
	commands []string // editing commands for the key (macOS shortcuts)
}

// typist is how a person types: the middle of their gaps between keys and
// how widely those vary, and how long they hold a key.
type typist struct {
	gap, spread, hold float64 // seconds; spread is log-normal sigma
}

func newTypist(r *rand.Rand) *typist {
	// 0.11-0.19 s a key is about 110 down to 65 words a minute.
	return &typist{gap: uniform(r, 0.11, 0.19), spread: uniform(r, 0.3, 0.45), hold: uniform(r, 0.075, 0.11)}
}

func (s *session) typist() *typist {
	if s.typer == nil {
		s.typer = newTypist(s.rand())
	}
	return s.typer
}

func lognormal(r *rand.Rand, median, sigma float64) float64 {
	return median * math.Exp(sigma*r.NormFloat64())
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// fingers maps the keys of a US keyboard to the finger that types them:
// 0-3 the left hand, little finger to index; 4-7 the right, index to
// little.
var fingers = func() map[rune]int {
	m := map[rune]int{}
	for f, keys := range []string{"`1qaz", "2wsx", "3edc", "45rtfgvb", "67yhujnm", "8ik,", "9ol.", "0-=p[]\\;'/"} {
		for _, r := range keys {
			m[r] = f
		}
	}
	return m
}()

func fingerOf(r rune) (int, bool) {
	if k, ok := kb.Keys[r]; ok && k.Unmodified != "" {
		r, _ = utf8.DecodeRuneInString(k.Unmodified)
	}
	f, ok := fingers[unicode.ToLower(r)]
	return f, ok
}

// pairFactor is how a pair of keys changes the gap between them: hands
// that alternate are quick, the same finger reaching for another key slow.
func pairFactor(a, b rune) float64 {
	if a == ' ' || b == ' ' {
		return 0.95
	}
	fa, oka := fingerOf(a)
	fb, okb := fingerOf(b)
	switch {
	case !oka || !okb:
		return 1
	case unicode.ToLower(a) == unicode.ToLower(b):
		return 1.2
	case fa == fb:
		return 1.35
	case (fa < 4) == (fb < 4):
		return 1.05
	}
	return 0.85
}

// plan lays out the presses and releases that type text. multiline says
// whether a newline is Enter (a textarea, an editor) or is left out.
func (ty *typist) plan(r *rand.Rand, text string, multiline bool) []keyEvent {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []keyEvent
	var last, held time.Duration // the last press; when every key so far is up
	var lastKey keyDef
	shift, first := false, true
	prev := rune(-1)
	for _, ch := range text {
		if ch == '\n' && !multiline {
			continue
		}
		down := time.Duration(0)
		if !first {
			gap := lognormal(r, ty.gap, ty.spread) * pairFactor(prev, ch)
			switch {
			case strings.ContainsRune(".!?", prev):
				gap *= 1.8
			case strings.ContainsRune(",;:", prev):
				gap *= 1.3
			case prev == ' ':
				gap *= 1.1
			}
			if r.Float64() < 0.03 {
				gap += uniform(r, 0.3, 1.0) // a moment's thought
			}
			down = last + secs(gap)
		}
		k, ok := keyOf(ch)
		if !ok {
			// Nothing on the keyboard types it: in whole, after the keys.
			if down < held {
				down = held + secs(uniform(r, 0.02, 0.06))
			}
			if shift {
				out = append(out, keyEvent{at: held + secs(uniform(r, 0.005, 0.02)), key: shiftKey})
				shift = false
			}
			out = append(out, keyEvent{at: down, insert: string(ch)})
			last, held, prev, first = down, down, ch, false
			continue
		}
		if k.code == lastKey.code && !first && down < held+secs(0.015) {
			down = held + secs(uniform(r, 0.015, 0.04)) // the same key is up before it goes down again
		}
		if k.shift != shift {
			// Shift goes down or up between the keys, never across them.
			if down < held+secs(0.04) {
				down = held + secs(uniform(r, 0.04, 0.07))
			}
			at := down - secs(uniform(r, 0.03, 0.08))
			if at < held+secs(0.005) {
				at = held + secs(0.005)
			}
			ev := keyEvent{at: at, down: k.shift, key: shiftKey}
			if k.shift {
				ev.mods = input.ModifierShift
			}
			out = append(out, ev)
			shift = k.shift
		}
		var mods input.Modifier
		if shift {
			mods = input.ModifierShift
		}
		up := down + secs(lognormal(r, ty.hold, 0.25))
		out = append(out, keyEvent{at: down, down: true, key: k, mods: mods}, keyEvent{at: up, key: k, mods: mods})
		last, prev, lastKey, first = down, ch, k, false
		if up > held {
			held = up
		}
	}
	if shift {
		out = append(out, keyEvent{at: held + secs(uniform(r, 0.01, 0.05)), key: shiftKey})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// macCommands are the editing commands macOS Chrome runs for a shortcut.
// There the browser, not the page, turns ⌘A into select-all, so a key event
// from DevTools does nothing without the command beside it.
var macCommands = map[string]string{
	"KeyA": "selectAll", "KeyC": "copy", "KeyX": "cut", "KeyV": "paste", "KeyZ": "undo",
}

// planChord lays out one press of a key with modifiers: the modifiers go
// down one after another, the key is held, and they come up in reverse.
func planChord(r *rand.Rand, k keyDef, mods input.Modifier, mac bool) []keyEvent {
	var out []keyEvent
	var at time.Duration
	var held input.Modifier
	for _, m := range modifierKeys {
		if mods&m.mod != 0 {
			held |= m.mod
			out = append(out, keyEvent{at: at, down: true, key: m.key, mods: held})
			at += secs(uniform(r, 0.025, 0.07))
		}
	}
	down := keyEvent{at: at, down: true, key: k, mods: held}
	if mac && mods&input.ModifierMeta != 0 && mods&(input.ModifierCtrl|input.ModifierAlt) == 0 {
		if c, ok := macCommands[k.code]; ok {
			if c == "undo" && mods&input.ModifierShift != 0 {
				c = "redo"
			}
			down.commands = []string{c}
		}
	}
	at += secs(uniform(r, 0.06, 0.12))
	out = append(out, down, keyEvent{at: at, key: k, mods: held})
	for i := len(modifierKeys) - 1; i >= 0; i-- {
		if m := modifierKeys[i]; held&m.mod != 0 {
			at += secs(uniform(r, 0.015, 0.05))
			held &^= m.mod
			out = append(out, keyEvent{at: at, key: m.key, mods: held})
		}
	}
	return out
}

// keyParams is a key event as DevTools takes it. A key that types something
// goes down as keyDown with its text, which Chrome turns into keypress and
// input; one held with Control, Alt or Meta, or that types nothing, as
// rawKeyDown.
func keyParams(e keyEvent) (string, map[string]any) {
	if e.insert != "" {
		return "Input.insertText", map[string]any{"text": e.insert}
	}
	k := e.key
	// No nativeVirtualKeyCode: on macOS Chrome rebuilds the native event
	// from it, and a Windows code there names another key (17, Control, is
	// the T key; 65, a, the keypad's period). The mismatch looped on the
	// node's Chrome with a modifier held, flooding keydowns until the browser
	// hung. Chrome works the native event out from key and code instead,
	// which is what Puppeteer and Playwright rely on.
	p := map[string]any{"key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": e.mods}
	if k.location != 0 {
		p["location"] = k.location
	}
	switch {
	case !e.down:
		p["type"] = "keyUp"
	case k.text != "" && e.mods&^input.ModifierShift == 0:
		p["type"], p["text"], p["unmodifiedText"] = "keyDown", k.text, k.unmod
	default:
		p["type"] = "rawKeyDown"
	}
	if len(e.commands) > 0 {
		p["commands"] = e.commands
	}
	return "Input.dispatchKeyEvent", p
}

// keys plays a timeline on the hand's connection, each event at its time,
// and returns once the page has taken them all.
func (s *session) keys(ctx context.Context, t *tab, evs []keyEvent) error {
	if len(evs) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, evs[len(evs)-1].at+15*time.Second)
	defer cancel()
	hd, err := s.handOn(cctx, t)
	if err != nil {
		return err
	}
	start := time.Now()
	var sent []<-chan error
	for _, e := range evs {
		if err := sleepCtx(cctx, time.Until(start.Add(e.at))); err != nil {
			return err
		}
		method, params := keyParams(e)
		sent = append(sent, hd.send(cctx, method, params))
	}
	return s.taken(cctx, t, sent)
}

// onMac is whether the session's Chrome is a Mac's, where shortcuts are ⌘
// and need their editing commands.
func (s *session) onMac() bool {
	d, err := lookupDevice(s.meta.deviceName())
	return err == nil && d.OS == "darwin"
}

// typeKeys types text into what has focus, key by key; text longer than
// typeMax goes in at once, as pasted. It says which it did.
func (s *session) typeKeys(ctx context.Context, t *tab, text string) (pasted bool, err error) {
	if utf8.RuneCountInString(text) > typeMax {
		return true, insertText(ctx, t, text)
	}
	var multiline bool
	evalJSON(ctx, t, focusedMultilineJS, &multiline)
	return false, s.keys(ctx, t, s.typist().plan(s.rand(), text, multiline))
}

// focusedMultilineJS is whether what has focus takes a newline: a textarea
// or an editor, through shadow roots and same-origin frames.
const focusedMultilineJS = `(() => {
  let a = document.activeElement;
  for (let g = 0; a && g < 32; g++) {
    if (a.shadowRoot && a.shadowRoot.activeElement) { a = a.shadowRoot.activeElement; continue; }
    if (a.tagName === 'IFRAME' || a.tagName === 'FRAME') {
      let d = null; try { d = a.contentDocument; } catch (e) {}
      if (d && d.activeElement) { a = d.activeElement; continue; }
    }
    break;
  }
  return !!a && (a.tagName === 'TEXTAREA' || a.isContentEditable);
})()`

// pressChord presses a key with modifiers, n times, a typist's gap apart.
func (s *session) pressChord(ctx context.Context, t *tab, k *kb.Key, mods input.Modifier, n int) error {
	r, mac := s.rand(), s.onMac()
	var evs []keyEvent
	var at time.Duration
	for i := 0; i < n; i++ {
		chord := planChord(r, fromKB(k), mods, mac)
		for _, e := range chord {
			e.at += at
			evs = append(evs, e)
		}
		at = evs[len(evs)-1].at + secs(lognormal(r, s.typist().gap, s.typist().spread))
	}
	return s.keys(ctx, t, evs)
}

// clearFocused empties the field that has focus as a person does: select
// all, then Backspace. If the field is not empty after that (a widget that
// handles keys its own way), clearField (snapshot.go) empties it, when the
// target is known.
func (s *session) clearFocused(ctx context.Context, t *tab, spec targetSpec) error {
	mods := input.ModifierCtrl
	if s.onMac() {
		mods = input.ModifierMeta
	}
	if err := s.pressChord(ctx, t, kb.Keys['a'], mods, 1); err != nil {
		return err
	}
	if err := sleepCtx(ctx, uniformDur(s.rand(), 80*time.Millisecond, 200*time.Millisecond)); err != nil {
		return err
	}
	if err := s.pressChord(ctx, t, kb.Keys['\b'], 0, 1); err != nil {
		return err
	}
	var empty bool
	if err := evalJSON(ctx, t, focusedEmptyJS, &empty); err == nil && empty {
		return nil
	}
	if spec.empty() {
		return errors.New("the field did not empty with select-all and Backspace")
	}
	var ok bool
	if err := evalJSON(ctx, t, jsCall("clearField", spec), &ok); err != nil || !ok {
		return errors.New("the field did not empty with select-all and Backspace")
	}
	return nil
}

// focusedEmptyJS is whether the field that has focus is empty.
const focusedEmptyJS = `(() => {
  let a = document.activeElement;
  for (let g = 0; a && g < 32; g++) {
    if (a.shadowRoot && a.shadowRoot.activeElement) { a = a.shadowRoot.activeElement; continue; }
    if (a.tagName === 'IFRAME' || a.tagName === 'FRAME') {
      let d = null; try { d = a.contentDocument; } catch (e) {}
      if (d && d.activeElement) { a = d.activeElement; continue; }
    }
    break;
  }
  if (!a) return true;
  if (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA') return a.value === '';
  if (a.isContentEditable) return a.textContent.trim() === '';
  return true;
})()`
