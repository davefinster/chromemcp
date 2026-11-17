package main

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp/kb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The plan types the text: every key goes down and comes up, in time
// order; what the downs type is the text; Shift brackets the capitals
// without overlapping a key; the same key is up before it goes down again.
func TestTypingPlan(t *testing.T) {
	const text = "Hello, World! all good?"
	for seed := uint64(0); seed < 100; seed++ {
		r := rand.New(rand.NewPCG(seed, 3))
		evs := newTypist(r).plan(r, text, false)
		var typed strings.Builder
		downAt := map[string]time.Duration{}
		shift := false
		var prevAt time.Duration
		for i, e := range evs {
			if e.at < prevAt {
				t.Fatalf("seed %d: event %d goes back in time", seed, i)
			}
			prevAt = e.at
			if e.insert != "" {
				typed.WriteString(e.insert)
				continue
			}
			if e.key.code == "ShiftLeft" {
				if e.down == shift {
					t.Fatalf("seed %d: Shift %v twice", seed, e.down)
				}
				for code := range downAt {
					t.Fatalf("seed %d: Shift changed with %s held", seed, code)
				}
				shift = e.down
				continue
			}
			if e.down {
				if _, held := downAt[e.key.code]; held {
					t.Fatalf("seed %d: %s down twice", seed, e.key.code)
				}
				if e.key.shift != shift || (e.mods&input.ModifierShift != 0) != shift {
					t.Fatalf("seed %d: %q down with Shift %v", seed, e.key.text, shift)
				}
				downAt[e.key.code] = e.at
				typed.WriteString(e.key.text)
			} else {
				if _, held := downAt[e.key.code]; !held {
					t.Fatalf("seed %d: %s up without down", seed, e.key.code)
				}
				delete(downAt, e.key.code)
			}
		}
		if typed.String() != text || shift || len(downAt) != 0 {
			t.Fatalf("seed %d: typed %q, Shift held %v, keys held %v", seed, typed.String(), shift, downAt)
		}
	}
}

// A person's rhythm: gaps vary, a fast typist rolls over keys, and
// nothing is a metronome.
func TestTypingRhythm(t *testing.T) {
	const text = "the quick brown fox jumps over the lazy dog and keeps on running"
	var rollovers, pairs int
	for seed := uint64(0); seed < 40; seed++ {
		r := rand.New(rand.NewPCG(seed, 4))
		ty := newTypist(r)
		evs := ty.plan(r, text, false)
		var downs []time.Duration
		var order []bool // down/up, for rollover: a down while another key is held
		held := 0
		for _, e := range evs {
			if e.down {
				if held > 0 {
					rollovers++
				}
				held++
				downs = append(downs, e.at)
			} else {
				held--
			}
			order = append(order, e.down)
		}
		pairs += len(downs) - 1
		var gaps []float64
		for i := 1; i < len(downs); i++ {
			gaps = append(gaps, (downs[i] - downs[i-1]).Seconds())
		}
		sort.Float64s(gaps)
		med := gaps[len(gaps)/2]
		var mean, sd float64
		for _, g := range gaps {
			mean += g
		}
		mean /= float64(len(gaps))
		for _, g := range gaps {
			sd += (g - mean) * (g - mean)
		}
		sd = math.Sqrt(sd / float64(len(gaps)))
		if med < 0.07 || med > 0.3 || sd < 0.2*mean {
			t.Errorf("seed %d: median gap %.3fs, sd %.3f of mean %.3f", seed, med, sd, mean)
		}
	}
	if frac := float64(rollovers) / float64(pairs); frac < 0.03 || frac > 0.6 {
		t.Errorf("rollover on %.0f%% of keys", 100*frac)
	}
}

// What has no key goes in whole; a newline is Enter only where it can be.
func TestTypingSpecials(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	ty := newTypist(r)
	var inserts []string
	for _, e := range ty.plan(r, "café\tok", false) {
		if e.insert != "" {
			inserts = append(inserts, e.insert)
		}
	}
	if strings.Join(inserts, "|") != "é|\t" {
		t.Errorf("inserted %q", inserts)
	}
	enters := func(multiline bool) int {
		n := 0
		for _, e := range ty.plan(r, "a\nb", multiline) {
			if e.down && e.key.code == "Enter" {
				n++
			}
		}
		return n
	}
	if enters(false) != 0 || enters(true) != 1 {
		t.Errorf("Enter for a newline: one-line %d, multi-line %d", enters(false), enters(true))
	}
}

// A chord presses its modifiers as keys, in a hand's order, and releases
// them in reverse; on a Mac, ⌘A carries the command that makes it select.
func TestPlanChord(t *testing.T) {
	r := rand.New(rand.NewPCG(6, 6))
	k, mods, err := parseKey("Control+Shift+Tab")
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, e := range planChord(r, fromKB(k), mods, false) {
		dir := "up"
		if e.down {
			dir = "down"
		}
		seq = append(seq, e.key.key+" "+dir)
	}
	if got := strings.Join(seq, ", "); got != "Control down, Shift down, Tab down, Tab up, Shift up, Control up" {
		t.Errorf("chord: %s", got)
	}
	ka, ma, _ := parseKey("Meta+a")
	has := func(mac bool) bool {
		for _, e := range planChord(r, fromKB(ka), ma, mac) {
			if e.down && e.key.code == "KeyA" {
				return len(e.commands) == 1 && e.commands[0] == "selectAll"
			}
		}
		return false
	}
	if !has(true) || has(false) {
		t.Errorf("⌘A selectAll command: mac %v, elsewhere %v", has(true), has(false))
	}
	// The modifier keys as a keyboard reports them.
	_, p := keyParams(keyEvent{down: true, key: shiftKey, mods: input.ModifierShift})
	if p["type"] != "rawKeyDown" || p["windowsVirtualKeyCode"] != int64(16) || p["location"] != int64(1) {
		t.Errorf("Shift down: %v", p)
	}
	_, p = keyParams(keyEvent{down: true, key: fromKB(kb.Keys['A']), mods: input.ModifierShift})
	if p["type"] != "keyDown" || p["text"] != "A" || p["windowsVirtualKeyCode"] != int64(65) {
		t.Errorf("A down: %v", p)
	}
	// Never a native code: macOS reads a Windows one as another key, and on
	// the node a held modifier then looped until Chrome hung.
	for _, k := range []keyDef{shiftKey, modifierKeys[0].key, fromKB(kb.Keys['a']), fromKB(kb.Keys['\r'])} {
		for _, down := range []bool{true, false} {
			if _, p := keyParams(keyEvent{down: down, key: k}); p["nativeVirtualKeyCode"] != nil {
				t.Errorf("%s carries a native code: %v", k.key, p)
			}
		}
	}
}

const typingTestPage = `<!doctype html><html><head><title>Typing</title></head><body>
<form id="f" onsubmit="event.preventDefault(); document.title = 'submitted ' + document.getElementById('i').value">
<input id="i" value="old text"> <textarea id="ta"></textarea>
</form>
<script>
window.kl = [];
for (const type of ['keydown', 'keyup']) addEventListener(type, e => kl.push({type, key: e.key, code: e.code, keyCode: e.keyCode, shift: e.shiftKey, ctrl: e.ctrlKey, location: e.location, t: e.timeStamp, trusted: e.isTrusted}), true);
addEventListener('input', e => kl.push({type: 'input', inputType: e.inputType, t: e.timeStamp}), true);
</script></body></html>`

type keyLog struct {
	Type      string  `json:"type"`
	Key       string  `json:"key"`
	Code      string  `json:"code"`
	KeyCode   int     `json:"keyCode"`
	Shift     bool    `json:"shift"`
	Ctrl      bool    `json:"ctrl"`
	Location  int     `json:"location"`
	T         float64 `json:"t"`
	Trusted   bool    `json:"trusted"`
	InputType string  `json:"inputType"`
}

// What a page sees of the typist, through the tools: a field emptied with
// select-all and Backspace, then every key down and up with a real
// keyCode, Shift as a key of its own around the capitals, an uneven rhythm,
// characters with no key inserted, Enter in a textarea, a chord's modifiers
// as keys, a long text pasted, and a submit.
func TestTypingTools(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(typingTestPage)) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		return navigate(ctx, tb, srv.URL, 20*time.Second)
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &mcpApp{mgr: mgr}
	no := false
	tin := tabIn{SessionID: s.meta.ID, Screenshot: &no}
	logs := func() []keyLog {
		t.Helper()
		var l []keyLog
		evalOn(t, mgr, ctx, s, "(() => { const l = kl; kl = []; return l; })()", &l)
		return l
	}
	value := func(id string) string {
		t.Helper()
		var v string
		evalOn(t, mgr, ctx, s, "document.getElementById('"+id+"').value", &v)
		return v
	}

	if _, _, err := a.typeText(ctx, nil, typeIn{tabIn: tin, targetSpec: targetSpec{Selector: "#i"}, Text: "Hello World", Clear: true}); err != nil {
		t.Fatal(err)
	}
	if v := value("i"); v != "Hello World" {
		t.Fatalf("field holds %q", v)
	}
	l := logs()
	var downs []keyLog
	var sawShift, sawCapital, sawDelete bool
	holds := map[string]float64{}
	var held []float64
	for _, e := range l {
		if e.Type != "input" && !e.Trusted {
			t.Errorf("an untrusted %s", e.Type)
		}
		switch {
		case e.Type == "input" && e.InputType == "deleteContentBackward":
			sawDelete = true
		case e.Type == "keydown" && e.Key == "Shift":
			sawShift = sawShift || (e.KeyCode == 16 && e.Code == "ShiftLeft" && e.Location == 1 && e.Shift)
		case e.Type == "keydown" && e.Key == "H":
			sawCapital = e.KeyCode == 72 && e.Shift && e.Code == "KeyH"
		}
		if e.Type == "keydown" && len(e.Key) == 1 {
			downs = append(downs, e)
			holds[e.Code] = e.T
		}
		if e.Type == "keyup" {
			if at, ok := holds[e.Code]; ok {
				held = append(held, e.T-at)
				delete(holds, e.Code)
			}
		}
	}
	if !sawShift || !sawCapital || !sawDelete {
		t.Errorf("Shift as a key %v, H with Shift %v, the old text deleted by Backspace %v", sawShift, sawCapital, sawDelete)
	}
	// The downs after the clear's select-all: the eleven characters.
	typed := downs[len(downs)-11:]
	var b strings.Builder
	var gaps []float64
	for i, e := range typed {
		b.WriteString(e.Key)
		if i > 0 {
			gaps = append(gaps, e.T-typed[i-1].T)
		}
	}
	if b.String() != "Hello World" {
		t.Errorf("keys down: %q", b.String())
	}
	sort.Float64s(gaps)
	if gaps[0] == gaps[len(gaps)-1] || gaps[len(gaps)/2] < 50 || gaps[len(gaps)/2] > 400 {
		t.Errorf("gaps between keys (ms): %v", gaps)
	}
	sort.Float64s(held)
	if len(held) < 10 || held[len(held)/2] < 40 || held[len(held)/2] > 200 {
		t.Errorf("keys held (ms): %v", held)
	}

	if _, _, err := a.typeText(ctx, nil, typeIn{tabIn: tin, targetSpec: targetSpec{Selector: "#ta"}, Text: "naïve\ncafé"}); err != nil {
		t.Fatal(err)
	}
	if v := value("ta"); v != "naïve\ncafé" {
		t.Errorf("textarea holds %q", v)
	}
	var enter bool
	for _, e := range logs() {
		enter = enter || (e.Type == "keydown" && e.Key == "Enter" && e.KeyCode == 13)
	}
	if !enter {
		t.Error("no Enter for the newline")
	}

	if _, _, err := a.press(ctx, nil, pressIn{tabIn: tin, Key: "Control+a"}); err != nil {
		t.Fatal(err)
	}
	var chord []string
	for _, e := range logs() {
		if e.Type == "keydown" {
			chord = append(chord, e.Key)
			if e.Key == "Control" && (e.KeyCode != 17 || e.Location != 1 || !e.Ctrl) {
				t.Errorf("Control down: %+v", e)
			}
			if e.Key == "a" && !e.Ctrl {
				t.Errorf("a down without Control: %+v", e)
			}
		}
	}
	if strings.Join(chord, " ") != "Control a" {
		t.Errorf("Control+a pressed %v", chord)
	}

	long := strings.Repeat("long text ", 20)
	res, _, err := a.typeText(ctx, nil, typeIn{tabIn: tin, targetSpec: targetSpec{Selector: "#i"}, Text: long, Clear: true, Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := value("i"); v != long {
		t.Errorf("after pasting, the field holds %q", v)
	}
	var said strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			said.WriteString(tc.Text)
		}
	}
	if !strings.Contains(said.String(), "pasted") {
		t.Errorf("a long text was not reported as pasted: %s", said.String())
	}
	var title string
	evalOn(t, mgr, ctx, s, "document.title", &title)
	if title != "submitted "+strings.TrimSpace(long) { // a title drops trailing space
		t.Errorf("after submitting, the title is %q", title)
	}
}
