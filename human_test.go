package main

import (
	"context"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A planned move starts where the pointer is, ends exactly on the target,
// bows off the straight line, and is slow at both ends and fast between.
func TestPlanMove(t *testing.T) {
	from, to := pt{40, 50}, pt{700, 420}
	var bowed, curvedAll int
	for seed := uint64(0); seed < 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 1))
		strokes := planMove(r, from, to, 30)
		if len(strokes) == 0 || len(strokes) > 2 {
			t.Fatalf("seed %d: %d strokes", seed, len(strokes))
		}
		if p := strokes[0].at(0); p != from {
			t.Fatalf("seed %d: starts at %v", seed, p)
		}
		if p := strokes[len(strokes)-1].at(1); p != to {
			t.Fatalf("seed %d: ends at %v", seed, p)
		}
		var total time.Duration
		for _, st := range strokes {
			total += st.pause + st.dur
		}
		if total < 250*time.Millisecond || total > 2500*time.Millisecond {
			t.Errorf("seed %d: %s to go %.0f px", seed, total, from.dist(to))
		}
		// How far it strays from the straight line, and how fast it goes
		// at the start, the middle and the end of the first stroke.
		st := strokes[0]
		end := st.at(1)
		dir := end.sub(from).unit()
		var dev float64
		for i := 0; i <= 100; i++ {
			p := st.at(float64(i) / 100).sub(from)
			dev = math.Max(dev, math.Abs(p.X*dir.perp().X+p.Y*dir.perp().Y))
		}
		if dev > 3 {
			bowed++
		}
		speed := func(u float64) float64 { return st.at(u + 0.01).dist(st.at(u)) }
		if mid, start, stop := speed(0.45), speed(0.02), speed(0.97); mid > 3*start && mid > 3*stop {
			curvedAll++
		}
	}
	if bowed < 190 {
		t.Errorf("only %d of 200 moves bowed off the straight line", bowed)
	}
	if curvedAll < 195 {
		t.Errorf("only %d of 200 moves were slow-fast-slow", curvedAll)
	}
	// Different every time.
	a := planMove(rand.New(rand.NewPCG(1, 2)), from, to, 30)[0].at(0.5)
	b := planMove(rand.New(rand.NewPCG(3, 4)), from, to, 30)[0].at(0.5)
	if a.dist(b) < 1 {
		t.Errorf("two moves took the same path: %v %v", a, b)
	}
	if planMove(rand.New(rand.NewPCG(1, 1)), to, to, 30) != nil {
		t.Error("a move to where the pointer is planned something")
	}
}

// Fitts: farther and smaller take longer.
func TestFittsTime(t *testing.T) {
	avg := func(d, w float64) time.Duration {
		r := rand.New(rand.NewPCG(7, 7))
		var sum time.Duration
		for i := 0; i < 100; i++ {
			sum += fittsTime(r, d, w)
		}
		return sum / 100
	}
	if near, far := avg(100, 40), avg(800, 40); far <= near {
		t.Errorf("800 px took %s, 100 px %s", far, near)
	}
	if big, small := avg(500, 200), avg(500, 10); small <= big {
		t.Errorf("a 10 px target took %s, a 200 px one %s", small, big)
	}
}

// Swipes add up to exactly the distance, in its direction; a long way is
// several; each starts fast and decays.
func TestPlanSwipes(t *testing.T) {
	for _, total := range []float64{90, 540, -800, 5000, 20000} {
		r := rand.New(rand.NewPCG(uint64(math.Abs(total)), 9))
		sw := planSwipes(r, total)
		var sum float64
		for _, s := range sw {
			if s.dist*total <= 0 {
				t.Fatalf("%.0f: a swipe of %.0f", total, s.dist)
			}
			if s.frac(0) != 0 || s.frac(1) != 1 {
				t.Errorf("%.0f: frac(0)=%g frac(1)=%g", total, s.frac(0), s.frac(1))
			}
			early := s.frac(0.15) - s.frac(0.1)
			late := s.frac(0.75) - s.frac(0.7)
			if early < 3*late {
				t.Errorf("%.0f: no momentum decay: early %g late %g", total, early, late)
			}
			sum += s.dist
		}
		if sum != total {
			t.Errorf("%.0f: swipes add up to %.0f", total, sum)
		}
		if math.Abs(total) >= 5000 && (len(sw) < 2 || len(sw) > 12) {
			t.Errorf("%.0f: %d swipes", total, len(sw))
		}
	}
	if planSwipes(rand.New(rand.NewPCG(1, 1)), 0.4) != nil {
		t.Error("a scroll of nothing planned a swipe")
	}
}

const humanTestPage = `<!doctype html><html><head><title>Hand</title><style>
body { margin: 0; font: 14px sans-serif }
#b { position: absolute; left: 600px; top: 400px; width: 120px; height: 40px }
#box { position: absolute; top: 80px; left: 40px; width: 300px; height: 150px; overflow: auto; border: 1px solid }
#far { position: absolute; top: 2600px; left: 100px }
#q { position: absolute; top: 20px; left: 420px; width: 200px }
</style></head><body>
<input id="q">
<div id="box"><div style="height: 1200px"></div><button id="inner">Inner</button><div style="height: 100px"></div></div>
<button id="b">Target</button>
<button id="far">Far</button>
<div style="height: 5000px"></div>
<script>
window.log = {moves: [], downs: [], ups: [], clicks: [], wheels: [], keys: []};
const now = () => performance.now();
addEventListener('mousemove', e => log.moves.push([e.clientX, e.clientY, now()]), true);
addEventListener('mousedown', e => log.downs.push([e.clientX, e.clientY, e.buttons, now()]), true);
addEventListener('mouseup', e => log.ups.push([e.clientX, e.clientY, e.buttons, now()]), true);
addEventListener('click', e => log.clicks.push([e.target.id, e.clientX, e.clientY]), true);
addEventListener('wheel', e => log.wheels.push([e.deltaX, e.deltaY, now()]), {capture: true, passive: true});
addEventListener('keydown', e => log.keys.push([1, e.keyCode, e.timeStamp]), true);
addEventListener('keyup', e => log.keys.push([0, e.keyCode, e.timeStamp]), true);
</script></body></html>`

type handLog struct {
	Moves  [][3]float64 `json:"moves"`
	Downs  [][4]float64 `json:"downs"`
	Ups    [][4]float64 `json:"ups"`
	Clicks [][3]any     `json:"clicks"`
	Wheels [][3]float64 `json:"wheels"`
	Keys   [][3]float64 `json:"keys"`
}

// What a page sees of the hand: the pointer travels a curved, uneven path
// to a point inside the control and presses there with the button held a
// moment; offscreen targets, in the page or in a scrolling box, are swiped
// to with wheel events of uneven sizes; the page scrolls by what is asked.
func TestHumanInput(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(humanTestPage)) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	act := func(name string, fn func(ctx context.Context, s *session, tb *tab) error) handLog {
		t.Helper()
		var l handLog
		err := mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
			if err := evalJSON(ctx, tb, "if (window.log) { log.moves = []; log.downs = []; log.ups = []; log.clicks = []; log.wheels = []; } true", new(bool)); err != nil {
				return err
			}
			if err := fn(ctx, s, tb); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			return evalJSON(ctx, tb, "log", &l)
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return l
	}
	act("load", func(ctx context.Context, s *session, tb *tab) error {
		return navigate(ctx, tb, srv.URL, 20*time.Second)
	})

	// A click, from a known place, on a target in view, five times.
	var points []pt
	for i := 0; i < 5; i++ {
		l := act("click", func(ctx context.Context, s *session, tb *tab) error {
			s.ptr.set(pt{60, 560})
			if _, err := s.pointAt(ctx, tb, targetSpec{Selector: "#b"}); err != nil {
				return err
			}
			return s.press(ctx, tb, "left", 1, 0)
		})
		if len(l.Clicks) != 1 || l.Clicks[0][0] != "b" {
			t.Fatalf("clicks: %v", l.Clicks)
		}
		if len(l.Moves) < 8 {
			t.Fatalf("only %d mousemoves on the way", len(l.Moves))
		}
		if len(l.Downs) != 1 || len(l.Ups) != 1 {
			t.Fatalf("downs %v ups %v", l.Downs, l.Ups)
		}
		down, up := l.Downs[0], l.Ups[0]
		if down[2] != 1 || up[2] != 0 {
			t.Errorf("buttons on press %v, on release %v; want 1 and 0", down[2], up[2])
		}
		if held := up[3] - down[3]; held < 40 {
			t.Errorf("button held %.0f ms", held)
		}
		last := l.Moves[len(l.Moves)-1]
		if last[0] != down[0] || last[1] != down[1] {
			t.Errorf("pressed at (%v, %v), last move to (%v, %v)", down[0], down[1], last[0], last[1])
		}
		if down[0] < 600 || down[0] > 720 || down[1] < 400 || down[1] > 440 {
			t.Errorf("pressed at (%v, %v), outside the button", down[0], down[1])
		}
		points = append(points, pt{down[0], down[1]})
		// Off the straight line, and uneven in speed.
		first := pt{l.Moves[0][0], l.Moves[0][1]}
		dir := pt{last[0], last[1]}.sub(first).unit()
		var dev, peak float64
		for j, m := range l.Moves {
			p := pt{m[0], m[1]}.sub(first)
			dev = math.Max(dev, math.Abs(p.X*dir.perp().X+p.Y*dir.perp().Y))
			if j > 0 {
				if dt := m[2] - l.Moves[j-1][2]; dt > 0 {
					peak = math.Max(peak, pt{m[0], m[1]}.dist(pt{l.Moves[j-1][0], l.Moves[j-1][1]})/dt)
				}
			}
		}
		if dev < 2 {
			t.Errorf("the path strayed %.1f px from a straight line", dev)
		}
		n := len(l.Moves)
		tail := pt{l.Moves[n-1][0], l.Moves[n-1][1]}.dist(pt{l.Moves[n-2][0], l.Moves[n-2][1]}) / math.Max(1, l.Moves[n-1][2]-l.Moves[n-2][2])
		if peak < 2*tail {
			t.Errorf("peak speed %.2f px/ms, arriving at %.2f: no slowing onto the target", peak, tail)
		}
		if at, ok := s.ptr.get(); !ok || at.X != down[0] || at.Y != down[1] {
			t.Errorf("the session's pointer is at %v, the press was at (%v, %v)", at, down[0], down[1])
		}
	}
	distinct := map[pt]bool{}
	for _, p := range points {
		distinct[p] = true
	}
	if len(distinct) < 4 {
		t.Errorf("five clicks landed on %d distinct points: %v", len(distinct), points)
	}

	// A button out of sight in a scrolling box: wheeled to, over the box.
	l := act("inner", func(ctx context.Context, s *session, tb *tab) error {
		if _, err := s.pointAt(ctx, tb, targetSpec{Selector: "#inner"}); err != nil {
			return err
		}
		return s.press(ctx, tb, "left", 1, 0)
	})
	if len(l.Clicks) != 1 || l.Clicks[0][0] != "inner" {
		t.Errorf("inner clicks: %v", l.Clicks)
	}
	checkWheels(t, "inner", l.Wheels)
	var scrolled [2]float64
	evalOn(t, mgr, ctx, s, "[document.getElementById('box').scrollTop, scrollY]", &scrolled)
	if scrolled[0] < 900 || scrolled[1] != 0 {
		t.Errorf("box scrolled to %.0f and the page to %.0f; want the box only", scrolled[0], scrolled[1])
	}

	// A button far down the page.
	l = act("far", func(ctx context.Context, s *session, tb *tab) error {
		if _, err := s.pointAt(ctx, tb, targetSpec{Selector: "#far"}); err != nil {
			return err
		}
		return s.press(ctx, tb, "left", 1, 0)
	})
	if len(l.Clicks) != 1 || l.Clicks[0][0] != "far" {
		t.Errorf("far clicks: %v", l.Clicks)
	}
	checkWheels(t, "far", l.Wheels)

	// The page by a set amount.
	var before, after float64
	evalOn(t, mgr, ctx, s, "scrollY", &before)
	l = act("scroll", func(ctx context.Context, s *session, tb *tab) error {
		_, _, err := s.scrollPage(ctx, tb, 0, 700)
		return err
	})
	evalOn(t, mgr, ctx, s, "scrollY", &after)
	var sum float64
	for _, w := range l.Wheels {
		sum += w[1]
	}
	if sum != 700 || math.Abs(after-before-700) > 2 {
		t.Errorf("wheels added up to %.0f and the page moved %.0f; want 700", sum, after-before)
	}
	checkWheels(t, "scroll", l.Wheels)
}

// checkWheels: a swipe's wheel events vary in size, and it ends slower
// than its fastest.
func checkWheels(t *testing.T, name string, w [][3]float64) {
	t.Helper()
	if len(w) < 4 {
		t.Errorf("%s: %d wheel events", name, len(w))
		return
	}
	var peak float64
	sizes := map[float64]bool{}
	for _, e := range w {
		d := math.Abs(e[0]) + math.Abs(e[1])
		peak = math.Max(peak, d)
		sizes[d] = true
	}
	lastSize := math.Abs(w[len(w)-1][0]) + math.Abs(w[len(w)-1][1])
	if len(sizes) < 3 || lastSize*2 > peak {
		t.Errorf("%s: wheel deltas %v", name, w)
	}
}

func evalOn(t *testing.T, mgr *manager, ctx context.Context, s *session, expr string, out any) {
	t.Helper()
	err := mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		return evalJSON(ctx, tb, expr, out)
	})
	if err != nil {
		t.Fatal(err)
	}
}

const humanToolsPage = `<!doctype html><html><head><title>Tools</title><style>
body { margin: 0; font: 14px sans-serif }
#menu { display: none } #hov:hover + #menu { display: block }
</style></head><body>
<input id="q" placeholder="search">
<button id="hov">Hover me</button><div id="menu">Menu open</div>
<select id="sel"><option value="a">Alpha</option><option value="b">Beta</option></select>
<div style="height: 2500px"></div>
<a id="link" href="#done" onclick="document.title = 'clicked'">Way down</a>
<div style="height: 2500px"></div>
<script>
window.seen = {down: 0, wheel: 0, focusByClick: false};
addEventListener('mousedown', () => seen.down++, true);
addEventListener('wheel', () => seen.wheel++, {capture: true, passive: true});
document.getElementById('q').addEventListener('focus', () => { seen.focusByClick = seen.down > 0; });
</script></body></html>`

// The tools act through the hand: typing clicks into the field first,
// hovering opens a :hover menu, a link far down is swiped to and clicked,
// the page scrolls by wheel, a select gets its value with the pointer over it.
func TestHumanTools(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(humanToolsPage)) }))
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
	tab := tabIn{SessionID: s.meta.ID, Screenshot: &no}
	text := func(name string, res *mcp.CallToolResult, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	page := func(expr string, out any) { t.Helper(); evalOn(t, mgr, ctx, s, expr, out) }

	res, _, err := a.typeText(ctx, nil, typeIn{tabIn: tab, targetSpec: targetSpec{Selector: "#q"}, Text: "hello"})
	text("type", res, err)
	var typed struct {
		Value   string `json:"value"`
		ByClick bool   `json:"byClick"`
	}
	page("({value: document.getElementById('q').value, byClick: seen.focusByClick})", &typed)
	if typed.Value != "hello" || !typed.ByClick {
		t.Errorf("typed %+v; want hello, focused by a click", typed)
	}

	res, _, err = a.hover(ctx, nil, hoverIn{tabIn: tab, targetSpec: targetSpec{Text: "Hover me"}})
	text("hover", res, err)
	var menu string
	page("getComputedStyle(document.getElementById('menu')).display", &menu)
	if menu != "block" {
		t.Errorf("menu display %q after hovering", menu)
	}

	res, _, err = a.selectOption(ctx, nil, selectIn{tabIn: tab, targetSpec: targetSpec{Selector: "#sel"}, Value: "Beta"})
	text("select", res, err)
	if p, _ := s.ptr.get(); p.Y > 60 {
		t.Errorf("pointer at %v after choosing in the select at the top", p)
	}

	res, _, err = a.click(ctx, nil, clickIn{tabIn: tab, targetSpec: targetSpec{Text: "Way down"}})
	out := text("click", res, err)
	var title string
	var wheels float64
	page("document.title", &title)
	page("seen.wheel", &wheels)
	if title != "clicked" || wheels < 4 {
		t.Errorf("after clicking the link far down: title %q, %v wheel events; %s", title, wheels, out)
	}

	var y0, y1 float64
	page("scrollY", &y0)
	res, _, err = a.scroll(ctx, nil, scrollIn{tabIn: tab, Direction: "up", Amount: 600})
	out = text("scroll", res, err)
	page("scrollY", &y1)
	if math.Abs(y0-y1-600) > 2 || !strings.Contains(out, "scrolled up by 600 px") {
		t.Errorf("scrolled from %.0f to %.0f: %s", y0, y1, out)
	}

	res, _, err = a.screenshot(ctx, nil, screenshotIn{tabIn: tab, targetSpec: targetSpec{Selector: "#q"}})
	text("screenshot", res, err)
	page("scrollY", &y1)
	if y1 > 0 {
		t.Errorf("the page is at %.0f after showing the field at the top", y1)
	}

	// Coordinates are exact: no random point.
	x, y := 123.0, 45.0
	res, _, err = a.click(ctx, nil, clickIn{tabIn: tab, X: &x, Y: &y})
	if out := text("click xy", res, err); !strings.Contains(out, "(123, 45)") {
		t.Errorf("clicking at a point: %s", out)
	}
	if p, _ := s.ptr.get(); p != (pt{123, 45}) {
		t.Errorf("pointer at %v after clicking (123, 45)", p)
	}
}

// slowProxy forwards TCP to addr with delay added in each direction, in
// order: a node's link is about 100 ms there and back; the test uses 150.
func slowProxy(t *testing.T, addr string, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pipe := func(dst, src net.Conn) {
		type chunk struct {
			b  []byte
			at time.Time
		}
		ch := make(chan chunk, 4096)
		go func() {
			defer close(ch)
			for {
				b := make([]byte, 32<<10)
				n, err := src.Read(b)
				if n > 0 {
					ch <- chunk{b[:n], time.Now().Add(delay)}
				}
				if err != nil {
					return
				}
			}
		}()
		for c := range ch {
			time.Sleep(time.Until(c.at))
			if _, err := dst.Write(c.b); err != nil {
				break
			}
		}
		dst.Close()
		src.Close()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", addr)
			if err != nil {
				c.Close()
				continue
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return ln.Addr().String()
}

// Over a slow link the hand still reports at a hand's rate: the reports go
// out without waiting for each reply. Waiting spaced them by the round
// trip, so a node's moves came in jumps and its swipes in huge wheels.
func TestHumanOverSlowLink(t *testing.T) {
	mgr := testChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(humanTestPage)) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := mgr.start(ctx, startOptions{Device: nativeDevice})
	if err != nil {
		t.Fatal(err)
	}
	var l handLog
	var typed string
	err = mgr.withTab(ctx, s.meta.ID, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil {
			return err
		}
		// Only the hand goes the slow way.
		u, err := url.Parse(*s.cdpURL.Load())
		if err != nil {
			return err
		}
		u.Host = slowProxy(t, u.Host, 75*time.Millisecond)
		slow := u.String()
		s.cdpURL.Store(&slow)
		s.ptr.set(pt{60, 560})
		if _, err := s.pointAt(ctx, tb, targetSpec{Selector: "#b"}); err != nil {
			return err
		}
		if err := s.press(ctx, tb, "left", 1, 0); err != nil {
			return err
		}
		if _, _, err := s.scrollPage(ctx, tb, 0, 700); err != nil {
			return err
		}
		if _, err := s.pointAt(ctx, tb, targetSpec{Selector: "#q"}); err != nil {
			return err
		}
		if err := s.press(ctx, tb, "left", 1, 0); err != nil {
			return err
		}
		if _, err := s.typeKeys(ctx, tb, "hello world"); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		if err := evalJSON(ctx, tb, "document.getElementById('q').value", &typed); err != nil {
			return err
		}
		return evalJSON(ctx, tb, "log", &l)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Clicks) != 2 || l.Clicks[0][0] != "b" || l.Clicks[1][0] != "q" {
		t.Errorf("clicks: %v", l.Clicks)
	}
	gaps := make([]float64, 0, len(l.Moves))
	for i := 1; i < len(l.Moves); i++ {
		gaps = append(gaps, l.Moves[i][2]-l.Moves[i-1][2])
	}
	sort.Float64s(gaps)
	if len(gaps) < 8 || gaps[len(gaps)/2] > 40 {
		t.Errorf("%d moves, median gap %v ms over a 100 ms link; want a hand's ~16", len(l.Moves), gaps)
	}
	var peak, sum float64
	for _, w := range l.Wheels {
		peak = math.Max(peak, math.Abs(w[1]))
		if w[1] > 0 { // down the page; the swipes back up reach the field
			sum += w[1]
		}
	}
	if len(l.Wheels) < 15 || peak > 250 || sum != 700 {
		t.Errorf("%d wheel events, the biggest %.0f px, adding up to %.0f", len(l.Wheels), peak, sum)
	}
	if len(l.Downs) >= 1 && len(l.Ups) >= 1 {
		if held := l.Ups[0][3] - l.Downs[0][3]; held < 40 || held > 160 {
			t.Errorf("button held %.0f ms; want the hand's 55-125, not a round trip more", held)
		}
	}
	// Keys too: held as a finger holds them, a typist's gaps apart.
	if typed != "hello world" {
		t.Errorf("typed %q over the slow link", typed)
	}
	downAt := map[float64]float64{}
	var holds, keyGaps []float64
	last := -1.0
	for _, k := range l.Keys {
		if k[0] == 1 {
			downAt[k[1]] = k[2]
			if last >= 0 {
				keyGaps = append(keyGaps, k[2]-last)
			}
			last = k[2]
		} else if at, ok := downAt[k[1]]; ok {
			holds = append(holds, k[2]-at)
			delete(downAt, k[1])
		}
	}
	sort.Float64s(holds)
	sort.Float64s(keyGaps)
	if len(holds) < 11 || holds[len(holds)/2] > 140 || len(keyGaps) < 10 || keyGaps[len(keyGaps)/2] > 400 {
		t.Errorf("over the slow link, keys held %v ms, %v ms apart", holds, keyGaps)
	}
}
