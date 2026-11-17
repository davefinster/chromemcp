package main

// Pointer and scrolling the way a person's hand does them. A session has
// one mouse pointer, as a person has one mouse for every tab, and the
// agent's actions move it to what they act on instead of firing events at
// a point out of nowhere. The path is curved and its speed changes, slow to
// start, fast in the middle, slow onto the target, with the slight tremor
// of a hand. Long moves sometimes land short or long and correct. Clicks
// land on a random point of the control, biased to its middle, after a
// moment's pause, and the button is held as long as a finger holds it.
// Scrolling is trackpad swipes: a burst of wheel events that ramps up
// quickly and decays like momentum, several swipes with pauses for a long
// way, made over the scroller that holds what is wanted. That is also how
// a target is brought into view (snapshot.go's locate), instead of a jump.
//
// Pages see what they see from a person: mousemove, mouseover/out and
// pointer events along the way, real buttons on press, and wheel events
// whose sizes and spacing vary. Frameworks that only react to the full
// sequence react, and behavioural bot checks see a hand.
//
// The model (planMove, planSwipes) is pure and tested on its own; the rest
// replays it on a tab, sampled against the clock, so a slow DevTools link
// (a node's) sends fewer events but keeps the hand's speed.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

type pt struct{ X, Y float64 }

func (a pt) add(b pt) pt             { return pt{a.X + b.X, a.Y + b.Y} }
func (a pt) sub(b pt) pt             { return pt{a.X - b.X, a.Y - b.Y} }
func (a pt) scale(k float64) pt      { return pt{a.X * k, a.Y * k} }
func (a pt) dist(b pt) float64       { return math.Hypot(a.X-b.X, a.Y-b.Y) }
func (a pt) lerp(b pt, u float64) pt { return pt{a.X + (b.X-a.X)*u, a.Y + (b.Y-a.Y)*u} }
func (a pt) unit() pt                { d := math.Hypot(a.X, a.Y); return pt{a.X / d, a.Y / d} }
func (a pt) perp() pt                { return pt{-a.Y, a.X} }
func (a pt) round() pt               { return pt{math.Round(a.X), math.Round(a.Y)} }
func (a pt) clamp(w, h float64) pt {
	return pt{math.Max(0, math.Min(w-1, a.X)), math.Max(0, math.Min(h-1, a.Y))}
}
func uniform(r *rand.Rand, lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }

func uniformDur(r *rand.Rand, lo, hi time.Duration) time.Duration {
	return lo + time.Duration(r.Int64N(int64(hi-lo)+1))
}

// pointer is where the session's mouse is, in the viewport's CSS pixels.
// The live view moves it too, so the agent's next move starts where the
// person left it. It is not under s.mu, because the live view writes it
// while an agent's action holds that lock.
type pointer struct {
	mu    sync.Mutex
	at    pt
	known bool
}

func (p *pointer) get() (pt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at, p.known
}

func (p *pointer) set(at pt) {
	p.mu.Lock()
	p.at, p.known = at, true
	p.mu.Unlock()
}

// forget is for a Chrome that has gone: the next one has seen no mouse.
func (p *pointer) forget() {
	p.mu.Lock()
	p.known = false
	p.mu.Unlock()
}

// ---- the model ----

// stroke is one continuous movement of the hand: where the pointer is at
// each fraction u of its duration, after a pause before it starts.
type stroke struct {
	pause time.Duration
	dur   time.Duration
	at    func(u float64) pt
}

// fittsTime is how long an aimed movement over d px to a target w px wide
// takes (Fitts's law, with a person's day-to-day spread around it).
func fittsTime(r *rand.Rand, d, w float64) time.Duration {
	w = math.Max(w, 4)
	bits := math.Log2(d/w + 1)
	sec := (0.08 + 0.11*bits) * uniform(r, 0.85, 1.2)
	sec = math.Max(0.1, math.Min(1.6, sec))
	return time.Duration(sec * float64(time.Second))
}

// minJerk is the smoothest way from 0 to 1, which is how hands move: no
// speed at either end, most of it in the middle.
func minJerk(s float64) float64 { return s * s * s * (10 - 15*s + 6*s*s) }

// planMove plans the pointer's way from one point to another, ending
// exactly on the second. width is the target's size, which sets the time a
// person takes to land on it.
func planMove(r *rand.Rand, from, to pt, width float64) []stroke {
	d := from.dist(to)
	if d < 1 {
		return nil
	}
	if d > 220 && r.Float64() < 0.4 {
		// A primary movement that lands a little short (or past), and a
		// slower correction onto the target.
		dir := to.sub(from).unit()
		miss := math.Min(40, d*uniform(r, 0.03, 0.08))
		if r.Float64() < 0.65 {
			miss = -miss
		}
		side := uniform(r, -1, 1) * math.Min(12, d*0.02)
		aim := to.add(dir.scale(miss)).add(dir.perp().scale(side))
		first := curve(r, from, aim, math.Max(width*2.5, 40))
		second := curve(r, aim, to, width)
		second.pause = uniformDur(r, 25*time.Millisecond, 90*time.Millisecond)
		return []stroke{first, second}
	}
	return []stroke{curve(r, from, to, width)}
}

// curve is one stroke along a cubic Bezier bowed to one side (now and then
// an S), with the minimum-jerk speed laid along its length, the peak a
// little before halfway, and a hand's tremor across it that dies away at
// both ends.
func curve(r *rand.Rand, a, b pt, width float64) stroke {
	d := a.dist(b)
	dir := b.sub(a).unit()
	n := dir.perp()
	bend := math.Min(160, d*uniform(r, 0.04, 0.2))
	side := 1.0
	if r.Float64() < 0.5 {
		side = -1
	}
	k1 := side * bend * uniform(r, 0.6, 1.4)
	k2 := side * bend * uniform(r, 0.6, 1.4)
	if r.Float64() < 0.2 {
		k2 = -k2 * 0.5
	}
	p1 := a.lerp(b, uniform(r, 0.2, 0.4)).add(n.scale(k1))
	p2 := a.lerp(b, uniform(r, 0.6, 0.8)).add(n.scale(k2))
	bez := func(t float64) pt {
		m := 1 - t
		return a.scale(m * m * m).add(p1.scale(3 * m * m * t)).add(p2.scale(3 * m * t * t)).add(b.scale(t * t * t))
	}
	// Arc length at even steps of the parameter, to walk the path by
	// distance travelled rather than by parameter.
	const N = 64
	var cum [N + 1]float64
	prev := a
	for i := 1; i <= N; i++ {
		p := bez(float64(i) / N)
		cum[i] = cum[i-1] + p.dist(prev)
		prev = p
	}
	byLength := func(s float64) float64 {
		want := s * cum[N]
		i := 1
		for i < N && cum[i] < want {
			i++
		}
		seg := cum[i] - cum[i-1]
		if seg <= 0 {
			return float64(i) / N
		}
		return (float64(i-1) + (want-cum[i-1])/seg) / N
	}
	skew := uniform(r, 0.8, 0.95)
	amp := uniform(r, 0.3, 1.3)
	f1, f2 := uniform(r, 2, 5), uniform(r, 5, 10)
	ph1, ph2 := uniform(r, 0, 2*math.Pi), uniform(r, 0, 2*math.Pi)
	at := func(u float64) pt {
		if u <= 0 {
			return a
		}
		if u >= 1 {
			return b
		}
		p := bez(byLength(minJerk(math.Pow(u, skew))))
		tremor := amp * math.Sin(math.Pi*u) * (0.6*math.Sin(2*math.Pi*f1*u+ph1) + 0.4*math.Sin(2*math.Pi*f2*u+ph2))
		return p.add(n.scale(tremor))
	}
	return stroke{dur: fittsTime(r, d, width), at: at}
}

// swipe is one trackpad swipe: dist px (signed) over dur, frac(u) of it
// covered at fraction u of the time, then a pause before the next.
type swipe struct {
	dist  float64
	dur   time.Duration
	frac  func(u float64) float64
	pause time.Duration
}

// planSwipes splits a scroll of total px (signed) into swipes. Each ramps
// up fast as the fingers start and decays like momentum after they lift: a
// gamma-shaped speed that peaks a tenth of the way in and then trails off.
func planSwipes(r *rand.Rand, total float64) []swipe {
	sign := 1.0
	if total < 0 {
		sign, total = -1, -total
	}
	total = math.Round(total)
	if total < 1 {
		return nil
	}
	// A long way gets harder flicks, as a person scrolling far flicks
	// harder rather than swiping forty times.
	reach := math.Max(600, math.Min(3500, total/3))
	var out []swipe
	for left := total; left >= 1; {
		size := math.Round(math.Min(left, reach*uniform(r, 0.65, 1)))
		if left-size < 60 {
			size = left
		}
		left -= size
		k := uniform(r, 7, 11)
		norm := 1 - (1+k)*math.Exp(-k)
		sec := (0.25 + 0.35*math.Sqrt(size/600)) * uniform(r, 0.85, 1.2)
		sec = math.Max(0.2, math.Min(1.3, sec))
		out = append(out, swipe{
			dist: sign * size,
			dur:  time.Duration(sec * float64(time.Second)),
			frac: func(u float64) float64 {
				if u >= 1 {
					return 1
				}
				return (1 - (1+k*u)*math.Exp(-k*u)) / norm
			},
			pause: uniformDur(r, 90*time.Millisecond, 320*time.Millisecond),
		})
	}
	return out
}

// ---- replaying it on a tab ----

// rand is the session's randomness, under s.mu like everything an action
// does.
func (s *session) rand() *rand.Rand {
	if s.rng == nil {
		s.rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return s.rng
}

// rnd is n uniform numbers for the page's side of the model (snapshot.go),
// so it never depends on the page's own Math.random.
func (s *session) rnd(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = s.rand().Float64()
	}
	return out
}

// sleepCtx waits, or returns early with the context's error.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// tick is the gap between a device's reports, as a mouse or trackpad
// sends them (and as Chrome coalesces them, to frames).
func (s *session) tick() time.Duration {
	return uniformDur(s.rand(), 12*time.Millisecond, 20*time.Millisecond)
}

// moveTo moves the pointer to a point along a hand's path. width is the
// size of what is there (for the time it takes); 0 for a bare point.
func (s *session) moveTo(ctx context.Context, t *tab, to pt, width float64) error {
	w, h, err := viewportSize(ctx, t)
	if err != nil {
		return err
	}
	vw, vh := float64(w), float64(h)
	// Whole pixels, as a mouse reports them: a fraction is no hand's.
	to = to.clamp(vw, vh).round()
	from, known := s.ptr.get()
	if !known {
		// A mouse nobody has moved here yet: somewhere over the page.
		from = pt{uniform(s.rand(), 0.2, 0.8) * vw, uniform(s.rand(), 0.2, 0.8) * vh}
	}
	if width <= 0 {
		width = 12
	}
	strokes := planMove(s.rand(), from.clamp(vw, vh), to, width)
	cctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		last, sent := from, false
		for _, st := range strokes {
			if err := sleepCtx(ctx, st.pause); err != nil {
				return err
			}
			start := time.Now()
			for {
				u := math.Min(1, float64(time.Since(start))/float64(st.dur))
				p := st.at(u).clamp(vw, vh).round()
				// A still mouse reports nothing.
				if p.dist(last) >= 0.5 || (u == 1 && !sent) {
					if err := input.DispatchMouseEvent(input.MouseMoved, p.X, p.Y).WithPointerType(input.Mouse).Do(ctx); err != nil {
						return err
					}
					s.ptr.set(p)
					last, sent = p, true
				}
				if u == 1 {
					break
				}
				if err := sleepCtx(ctx, s.tick()); err != nil {
					return err
				}
			}
		}
		if !sent {
			// Already there; still, the page hears the mouse arrive.
			if err := input.DispatchMouseEvent(input.MouseMoved, to.X, to.Y).WithPointerType(input.Mouse).Do(ctx); err != nil {
				return err
			}
			s.ptr.set(to)
		}
		return nil
	}))
}

// mouseButton parses a button name.
func mouseButton(name string) (input.MouseButton, int64, error) {
	switch name {
	case "", "left":
		return input.Left, 1, nil
	case "right":
		return input.Right, 2, nil
	case "middle":
		return input.Middle, 4, nil
	}
	return "", 0, fmt.Errorf("button %q: want left, right or middle", name)
}

// press clicks where the pointer is: a moment's settling first, the button
// held as long as a finger holds it, and a double click's two clicks a
// person's interval apart. The page sees the button in event.buttons.
func (s *session) press(ctx context.Context, t *tab, button string, count int, mods input.Modifier) error {
	btn, bits, err := mouseButton(button)
	if err != nil {
		return err
	}
	if count <= 0 {
		count = 1
	}
	at, known := s.ptr.get()
	if !known {
		return errors.New("the pointer is nowhere yet")
	}
	r := s.rand()
	cctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := sleepCtx(ctx, uniformDur(r, 70*time.Millisecond, 190*time.Millisecond)); err != nil {
			return err
		}
		for i := 1; i <= count; i++ {
			if err := input.DispatchMouseEvent(input.MousePressed, at.X, at.Y).WithButton(btn).WithButtons(bits).
				WithClickCount(int64(i)).WithModifiers(mods).WithPointerType(input.Mouse).Do(ctx); err != nil {
				return err
			}
			if err := sleepCtx(ctx, uniformDur(r, 55*time.Millisecond, 125*time.Millisecond)); err != nil {
				return err
			}
			if err := input.DispatchMouseEvent(input.MouseReleased, at.X, at.Y).WithButton(btn).WithButtons(0).
				WithClickCount(int64(i)).WithModifiers(mods).WithPointerType(input.Mouse).Do(ctx); err != nil {
				return err
			}
			if i < count {
				if err := sleepCtx(ctx, uniformDur(r, 70*time.Millisecond, 150*time.Millisecond)); err != nil {
					return err
				}
			}
		}
		return nil
	}))
}

// wheel scrolls by dx, dy px with trackpad swipes where the pointer is,
// vertical first. It returns once the last swipe's momentum is spent and
// the page has had a moment to follow.
func (s *session) wheel(ctx context.Context, t *tab, dx, dy float64) error {
	at, known := s.ptr.get()
	if !known {
		return errors.New("the pointer is nowhere yet")
	}
	r := s.rand()
	cctx, cancel := context.WithTimeout(t.ctx, 60*time.Second)
	defer cancel()
	return chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, axis := range []struct {
			total    float64
			vertical bool
		}{{dy, true}, {dx, false}} {
			for i, sw := range planSwipes(r, axis.total) {
				if i > 0 {
					if err := sleepCtx(ctx, sw.pause); err != nil {
						return err
					}
				}
				start, done := time.Now(), 0.0
				for {
					u := math.Min(1, float64(time.Since(start))/float64(sw.dur))
					step := sw.dist*sw.frac(u) - done
					if u < 1 {
						// Reports are uneven; what one leaves out the next makes up.
						step *= uniform(r, 0.85, 1.15)
					}
					step = math.Round(step)
					if step*sw.dist < 0 { // never back
						step = 0
					}
					if math.Abs(done+step) > math.Abs(sw.dist) || u == 1 { // never past the end, and end on it
						step = sw.dist - done
					}
					if step != 0 {
						ev := input.DispatchMouseEvent(input.MouseWheel, at.X, at.Y).WithPointerType(input.Mouse)
						if axis.vertical {
							ev = ev.WithDeltaX(0).WithDeltaY(step)
						} else {
							ev = ev.WithDeltaX(step).WithDeltaY(0)
						}
						if err := ev.Do(ctx); err != nil {
							return err
						}
						done += step
					}
					if u == 1 {
						break
					}
					if err := sleepCtx(ctx, s.tick()); err != nil {
						return err
					}
				}
			}
		}
		return sleepCtx(ctx, uniformDur(r, 150*time.Millisecond, 300*time.Millisecond))
	}))
}

// ---- acting on targets ----

// located is the page's answer to locate (snapshot.go).
type located struct {
	Error  string  `json:"error"`
	InView bool    `json:"inView"`
	Box    [4]int  `json:"box"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Stay   bool    `json:"stay"`
}

// curArg is the pointer as the page's side takes it: where it is, or null.
func (s *session) curArg() any {
	if p, ok := s.ptr.get(); ok {
		return map[string]float64{"x": p.X, "y": p.Y}
	}
	return nil
}

// reach brings a target into view the way a person scrolls to it, a swipe
// at a time over whatever holds it, and picks the point on it a person
// would act at. A page that will not scroll for the wheel (one that takes
// it for itself) gets the old jump instead, after two swipes that move
// nothing.
func (s *session) reach(ctx context.Context, t *tab, spec targetSpec) (*resolved, error) {
	if spec.empty() {
		return nil, errors.New("give one of ref, selector or text (or x and y)")
	}
	r := s.rand()
	// Where in the view a person stops: about the middle, not exactly.
	fr := [2]float64{uniform(r, 0.4, 0.6), uniform(r, 0.3, 0.6)}
	var last [4]int
	stuck := 0
	for step := 0; ; step++ {
		var loc located
		if err := evalJSON(ctx, t, jsCall("locate", spec, s.curArg(), fr, s.rnd(64)), &loc); err != nil {
			return nil, fmt.Errorf("finding %s: %w", spec, err)
		}
		if loc.Error != "" {
			return nil, errors.New(loc.Error)
		}
		if loc.InView {
			break
		}
		if step > 0 && loc.Box == last {
			stuck++
		} else {
			stuck = 0
		}
		if stuck >= 2 || step >= 12 {
			if _, err := resolveTarget(ctx, t, spec); err != nil {
				return nil, err
			}
			break
		}
		last = loc.Box
		if !loc.Stay {
			if err := s.moveTo(ctx, t, pt{loc.X, loc.Y}, 120); err != nil {
				return nil, err
			}
		}
		if err := s.wheel(ctx, t, loc.DX, loc.DY); err != nil {
			return nil, err
		}
	}
	var res resolved
	if err := evalJSON(ctx, t, jsCall("pointOn", spec, s.rnd(64)), &res); err != nil {
		return nil, fmt.Errorf("finding %s: %w", spec, err)
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return &res, nil
}

// targetWidth is the size of a target for the time a hand takes onto it.
func targetWidth(r *resolved) float64 {
	return math.Max(8, math.Min(400, math.Min(float64(r.Box[2]), float64(r.Box[3]))))
}

// pointAt brings a target into view and moves the pointer onto it. A page
// that moved it while the hand was on its way (an animation, a late
// layout) gets the correction a person makes, twice at most.
func (s *session) pointAt(ctx context.Context, t *tab, spec targetSpec) (*resolved, error) {
	res, err := s.reach(ctx, t, spec)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		if err := s.moveTo(ctx, t, pt{res.X, res.Y}, targetWidth(res)); err != nil {
			return nil, err
		}
		if res.Covered || try == 2 {
			return res, nil
		}
		var ok bool
		if err := evalJSON(ctx, t, jsCall("hitsAt", spec, res.X, res.Y), &ok); err != nil || ok {
			return res, nil
		}
		next, err := s.reach(ctx, t, spec)
		if err != nil {
			return res, nil // it went; stay where the hand is
		}
		res = next
	}
}

// scrollPage scrolls the page by dx, dy the way a person does: swipes over
// the page, where the pointer already is if a wheel there scrolls it (else
// it moves somewhere that does), as far as the page goes. It returns how far
// it swiped.
func (s *session) scrollPage(ctx context.Context, t *tab, dx, dy float64) (float64, float64, error) {
	var ps struct {
		None bool    `json:"none"`
		Stay bool    `json:"stay"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		DX   float64 `json:"dx"`
		DY   float64 `json:"dy"`
	}
	if err := evalJSON(ctx, t, jsCall("pageScroll", s.curArg(), dx, dy, s.rnd(64)), &ps); err != nil {
		return 0, 0, err
	}
	switch {
	case ps.None:
		// Nothing goes that way. A person still tries, where the mouse is.
		if _, ok := s.ptr.get(); !ok {
			w, h, err := viewportSize(ctx, t)
			if err != nil {
				return 0, 0, err
			}
			r := s.rand()
			if err := s.moveTo(ctx, t, pt{uniform(r, 0.3, 0.7) * float64(w), uniform(r, 0.3, 0.7) * float64(h)}, 200); err != nil {
				return 0, 0, err
			}
		}
	default:
		if !ps.Stay {
			if err := s.moveTo(ctx, t, pt{ps.X, ps.Y}, 200); err != nil {
				return 0, 0, err
			}
		}
		if ps.DX != 0 || ps.DY != 0 {
			dx, dy = ps.DX, ps.DY
		} else {
			ps.None = true // at the end already
		}
	}
	if ps.None {
		// A short swipe that finds the end.
		short := uniform(s.rand(), 60, 140)
		dx, dy = math.Copysign(math.Min(math.Abs(dx), short), dx), math.Copysign(math.Min(math.Abs(dy), short), dy)
	}
	return dx, dy, s.wheel(ctx, t, dx, dy)
}
