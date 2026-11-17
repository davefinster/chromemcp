package main

// The in-page side of the browsing tools: a script evaluated in the page's
// main world that (a) enumerates the interactive and structural elements
// with a short ref each — "e12" — the agent can click and type by, (b)
// resolves a target (ref, CSS selector, or visible text) to a point to
// click, and (c) draws or removes the ref labels a screenshot can carry.
// Refs are held on the window, not written into the DOM, so pages are not
// changed by being looked at.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// snapshotJS installs window.__cmcp and returns it. Idempotent per page.
const snapshotJS = `(() => {
if (window.__cmcp && window.__cmcp.v === 6) return window.__cmcp;
const SEL = 'a[href], button, input, select, textarea, summary, details, label, [role], [contenteditable=""], [contenteditable="true"], [contenteditable="plaintext-only"], [tabindex], [onclick], h1, h2, h3, h4, h5, h6, img[alt], [aria-label]';
const SKIP_ROLES = new Set(['presentation', 'none', 'generic', 'document', 'group', 'list', 'listitem', 'table', 'row', 'cell', 'gridcell', 'columnheader', 'rowheader', 'rowgroup', 'region', 'main', 'navigation', 'banner', 'contentinfo', 'complementary', 'form', 'search', 'article', 'section', 'paragraph', 'img', 'figure', 'separator', 'toolbar', 'status', 'log', 'timer', 'marquee', 'tooltip', 'progressbar', 'application', 'feed', 'note', 'definition', 'term', 'directory', 'math', 'code', 'emphasis', 'strong', 'subscript', 'superscript', 'time', 'insertion', 'deletion', 'blockquote', 'caption', 'meter', 'tablist', 'menubar', 'menu', 'listbox', 'radiogroup', 'tree', 'treegrid', 'grid', 'dialog', 'alertdialog', 'alert', 'tabpanel', 'scrollbar']);

function* walk(root) {
  const all = root.querySelectorAll('*');
  for (const el of all) {
    yield el;
    if (el.shadowRoot) yield* walk(el.shadowRoot);
  }
}
function frameOffset(win) {
  let x = 0, y = 0, w = win;
  try {
    while (w && w !== w.parent && w.frameElement) {
      const fe = w.frameElement, r = fe.getBoundingClientRect();
      x += r.left + fe.clientLeft; y += r.top + fe.clientTop; w = w.parent;
    }
  } catch (e) {}
  return {x, y};
}
// The composed parent: across a shadow root to its host, not out of a frame.
function parentOf(n) {
  if (n.parentElement) return n.parentElement;
  const r = n.getRootNode && n.getRootNode();
  return r && r.host ? r.host : null;
}
// What is under a point of the top viewport, looking into same-origin
// frames and open shadow roots, as the browser's own hit test does.
function deepHit(x, y) {
  let el = document.elementFromPoint(x, y), ox = 0, oy = 0;
  for (let g = 0; el && g < 32; g++) {
    if (el.shadowRoot) {
      const inner = el.shadowRoot.elementFromPoint(x - ox, y - oy);
      if (inner && inner !== el) { el = inner; continue; }
    }
    if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
      let d = null;
      try { d = el.contentDocument; } catch (e) {}
      if (!d) break;
      const r = el.getBoundingClientRect();
      ox += r.left + el.clientLeft; oy += r.top + el.clientTop;
      const inner = d.elementFromPoint(x - ox, y - oy);
      if (!inner) break;
      el = inner; continue;
    }
    break;
  }
  return el;
}
// Whether n is el or inside it, across shadow roots and frames.
function within(n, el) {
  for (let g = 0; n && g < 4096; g++) {
    if (n === el) return true;
    let p = parentOf(n);
    if (!p) { try { p = n.ownerDocument.defaultView.frameElement; } catch (e) { p = null; } }
    n = p;
  }
  return false;
}
function visible(el) {
  const st = el.ownerDocument.defaultView.getComputedStyle(el);
  if (!st || st.display === 'none' || st.visibility === 'hidden' || st.visibility === 'collapse' || parseFloat(st.opacity) === 0) return false;
  const r = el.getBoundingClientRect();
  if (r.width <= 0 && r.height <= 0) return false;
  if (el.getAttribute('aria-hidden') === 'true') return false;
  return true;
}
function proxyOf(el) {
  // A visually hidden native control fronted by its styled label: the
  // label is what is seen and what is clicked.
  if ((el.tagName === 'INPUT' || el.tagName === 'SELECT') && el.labels && el.labels.length) {
    const l = el.labels[0];
    if (visible(l)) return l;
  }
  return null;
}
function clean(s, max) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  if (max && s.length > max) s = s.slice(0, max - 1) + '…';
  return s;
}
function labelOf(el) {
  const doc = el.ownerDocument;
  const al = el.getAttribute('aria-label'); if (al) return clean(al, 120);
  const lb = el.getAttribute('aria-labelledby');
  if (lb) { const t = lb.split(/\s+/).map(id => { const n = doc.getElementById(id); return n ? n.innerText || n.textContent : ''; }).join(' '); if (clean(t)) return clean(t, 120); }
  if (el.labels && el.labels.length) { const t = clean(Array.from(el.labels).map(l => l.innerText).join(' '), 120); if (t) return t; }
  const ph = el.getAttribute('placeholder'); if (ph) return clean(ph, 120);
  if (el.tagName === 'INPUT' && (el.type === 'submit' || el.type === 'button' || el.type === 'reset') && el.value) return clean(el.value, 120);
  if (el.tagName === 'IMG') return clean(el.getAttribute('alt'), 120);
  const img = el.querySelector && el.querySelector('img[alt]');
  let t = clean(el.innerText !== undefined ? el.innerText : el.textContent, 120);
  if (!t && img) t = clean(img.getAttribute('alt'), 120);
  if (!t) t = clean(el.getAttribute('title'), 120);
  if (!t && el.tagName === 'INPUT') t = clean(el.getAttribute('name'), 60);
  return t;
}
function roleOf(el) {
  const tag = el.tagName.toLowerCase();
  const r = el.getAttribute('role');
  if (r) return r.split(/\s+/)[0];
  if (tag === 'a') return el.hasAttribute('href') ? 'link' : '';
  if (tag === 'button' || tag === 'summary') return 'button';
  if (tag === 'select') return 'combobox';
  if (tag === 'textarea') return 'textbox';
  if (tag === 'input') {
    const t = (el.type || 'text').toLowerCase();
    if (t === 'checkbox') return 'checkbox';
    if (t === 'radio') return 'radio';
    if (t === 'submit' || t === 'button' || t === 'reset' || t === 'image') return 'button';
    if (t === 'file') return 'file';
    if (t === 'range') return 'slider';
    if (t === 'hidden') return '';
    return 'textbox';
  }
  if (el.isContentEditable) return 'textbox';
  if (/^h[1-6]$/.test(tag)) return 'heading';
  if (tag === 'img') return 'img';
  if (tag === 'label') return 'label';
  if (tag === 'details') return '';
  if (el.hasAttribute('onclick') || (el.hasAttribute('tabindex') && el.tabIndex >= 0)) return 'clickable';
  if (el.hasAttribute('aria-label')) return 'clickable';
  return '';
}
function snapshot(opts) {
  opts = opts || {};
  const max = opts.max || 250;
  const refs = [];
  const out = [];
  const vw = innerWidth, vh = innerHeight;
  const docs = [{doc: document, off: {x: 0, y: 0}}];
  // Same-origin frames come along, offset to top-level coordinates.
  for (const f of document.querySelectorAll('iframe, frame')) {
    try { if (f.contentDocument && visible(f)) { const r = f.getBoundingClientRect(); docs.push({doc: f.contentDocument, off: {x: r.left, y: r.top}}); } } catch (e) {}
  }
  for (const {doc, off} of docs) {
    for (const el of walk(doc)) {
      if (!el.matches || !el.matches(SEL)) continue;
      let role = roleOf(el);
      if (!role || SKIP_ROLES.has(role)) continue;
      if (role === 'label' && el.control) continue; // the control carries the label already
      let via = null;
      if (!visible(el)) { via = proxyOf(el); if (!via) continue; }
      const r = (via || el).getBoundingClientRect();
      const box = [Math.round(r.left + off.x), Math.round(r.top + off.y), Math.round(r.width), Math.round(r.height)];
      const inView = box[0] + box[2] > 0 && box[1] + box[3] > 0 && box[0] < vw && box[1] < vh;
      if (opts.viewportOnly && !inView) continue;
      const item = {ref: 'e' + (refs.length + 1), role, tag: el.tagName.toLowerCase(), name: labelOf(el), box, inView};
      if (role === 'heading') item.level = parseInt(el.tagName[1]) || parseInt(el.getAttribute('aria-level')) || 0;
      if (el.tagName === 'A' && el.href) item.href = el.href;
      if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
        item.type = (el.type || 'text').toLowerCase();
        if (item.type === 'password') item.value = el.value ? '••••' : '';
        else if (item.type === 'checkbox' || item.type === 'radio') item.checked = !!el.checked;
        else item.value = clean(el.value, 80);
      }
      if (el.tagName === 'SELECT') {
        item.value = clean(el.selectedOptions && el.selectedOptions[0] ? el.selectedOptions[0].text : el.value, 80);
        item.options = Array.from(el.options).slice(0, 30).map(o => clean(o.text, 40));
        if (el.options.length > 30) item.options.push('… ' + (el.options.length - 30) + ' more');
      }
      if (el.isContentEditable && el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA') item.value = clean(el.innerText, 80);
      const ac = el.getAttribute('aria-checked'); if (ac !== null) item.checked = ac === 'true';
      const ae = el.getAttribute('aria-expanded'); if (ae !== null) item.expanded = ae === 'true';
      const as = el.getAttribute('aria-selected'); if (as !== null) item.selected = as === 'true';
      if (el.disabled || el.getAttribute('aria-disabled') === 'true') item.disabled = true;
      if (el.tagName === 'INPUT' && item.type === 'hidden') continue;
      refs.push(el);
      out.push(item);
      if (out.length >= max) break;
    }
    if (out.length >= max) break;
  }
  window.__cmcpRefs = refs;
  const de = document.documentElement;
  return {
    url: location.href, title: document.title,
    viewport: {w: vw, h: vh}, scroll: {x: Math.round(scrollX), y: Math.round(scrollY), pageW: de.scrollWidth, pageH: de.scrollHeight},
    truncated: out.length >= max, elements: out,
  };
}
function findByText(text, exact) {
  const want = clean(text).toLowerCase();
  if (!want) return [];
  const hits = [];
  for (const el of walk(document)) {
    if (!visible(el)) continue;
    const own = clean(el.innerText !== undefined ? el.innerText : el.textContent).toLowerCase();
    if (!own) continue;
    const match = exact ? own === want : own.includes(want);
    if (!match) continue;
    // Prefer the smallest element containing the text: skip if a child also matches.
    let childMatch = false;
    for (const c of el.children) { const ct = clean(c.innerText !== undefined ? c.innerText : c.textContent).toLowerCase(); if (ct && (exact ? ct === want : ct.includes(want))) { childMatch = true; break; } }
    if (childMatch) continue;
    hits.push(el);
  }
  // Interactive ancestors win: clicking the <a> rather than its <span>.
  return hits.map(el => el.closest('a[href], button, [role="button"], [role="link"], [role="menuitem"], [role="tab"], [role="option"], label, summary, input, select, textarea') || el);
}
function targetOf(t) {
  let el = null, how = '';
  if (t.ref) {
    const n = parseInt(String(t.ref).replace(/^e/, ''));
    const refs = window.__cmcpRefs || [];
    el = refs[n - 1];
    if (!el) return {error: 'unknown ref ' + t.ref + ' — take a new browser_snapshot; refs are renumbered on every snapshot and cleared by navigation'};
    if (!el.isConnected) return {error: 'ref ' + t.ref + ' is no longer in the page — take a new browser_snapshot'};
    how = t.ref;
  } else if (t.selector) {
    try { el = document.querySelector(t.selector); } catch (e) { return {error: 'bad selector: ' + e.message}; }
    if (!el) return {error: 'no element matches selector ' + JSON.stringify(t.selector)};
    how = 'selector ' + JSON.stringify(t.selector);
  } else if (t.text) {
    let hits = findByText(t.text, true);
    if (!hits.length) hits = findByText(t.text, false);
    if (!hits.length) return {error: 'no visible element with text ' + JSON.stringify(t.text)};
    el = hits[0];
    how = 'text ' + JSON.stringify(t.text) + (hits.length > 1 ? ' (first of ' + hits.length + ' matches)' : '');
  } else {
    return {error: 'give one of ref, selector or text'};
  }
  // What is seen and pointed at: the element, or the label fronting it.
  const seen = visible(el) ? el : (proxyOf(el) || el);
  return {el, seen, how};
}
function describeEl(el, how, r, off) {
  return {how, tag: el.tagName.toLowerCase(), role: roleOf(el), name: labelOf(el), box: [Math.round(r.left + off.x), Math.round(r.top + off.y), Math.round(r.width), Math.round(r.height)]};
}
// resolve jumps the target into view and gives its middle: the fallback
// for when a hand cannot get it into view (locate), and the screenshot's.
function resolve(t) {
  const f = targetOf(t);
  if (f.error) return f;
  const {el, seen, how} = f;
  try { seen.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'}); } catch (e) {}
  const r = seen.getBoundingClientRect();
  const off = frameOffset(el.ownerDocument.defaultView);
  if (r.width === 0 && r.height === 0) return {error: 'element ' + how + ' has no size (hidden?)'};
  const x = r.left + off.x + r.width / 2, y = r.top + off.y + r.height / 2;
  if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) return {error: 'element ' + how + ' is outside the viewport after scrolling'};
  return Object.assign(describeEl(el, how, r, off), {x, y});
}
// pointOn picks the point of a target in view that a person would click:
// random, biased to the middle, off the edges, on one of its boxes (a link
// broken over lines has one per line), and somewhere the target, not
// something over it, is what is hit. rnd is randomness from the server.
function pointOn(t, rnd) {
  const f = targetOf(t);
  if (f.error) return f;
  const {el, seen, how} = f;
  let i = 0;
  const R = () => rnd[(i++) % rnd.length];
  const off = frameOffset(seen.ownerDocument.defaultView);
  const whole = seen.getBoundingClientRect();
  const out = describeEl(el, how, whole, off);
  const boxes = [];
  for (const r of seen.getClientRects()) {
    const b = {l: Math.max(r.left + off.x, 0), t: Math.max(r.top + off.y, 0), r: Math.min(r.right + off.x, innerWidth), b: Math.min(r.bottom + off.y, innerHeight)};
    if (b.r - b.l >= 1 && b.b - b.t >= 1) boxes.push(b);
  }
  if (!boxes.length) return {error: 'element ' + how + (whole.width || whole.height ? ' is not in the viewport' : ' has no size (hidden?)')};
  const area = b => (b.r - b.l) * (b.b - b.t);
  const total = boxes.reduce((a, b) => a + area(b), 0);
  const pick = () => { let a = R() * total; for (const b of boxes) { a -= area(b); if (a <= 0) return b; } return boxes[boxes.length - 1]; };
  const gauss = () => Math.sqrt(-2 * Math.log(Math.max(R(), 1e-9))) * Math.cos(2 * Math.PI * R());
  const inside = (v, lo, hi, pad) => Math.min(Math.max(v, lo + pad), hi - pad);
  for (let k = 0; k < 16; k++) {
    const b = pick(), w = b.r - b.l, h = b.b - b.t;
    const x = inside((b.l + b.r) / 2 + gauss() * w / 6, b.l, b.r, Math.min(4, w * 0.2));
    const y = inside((b.t + b.b) / 2 + gauss() * h / 6, b.t, b.b, Math.min(4, h * 0.2));
    if (within(deepHit(x, y), seen)) return Object.assign(out, {x, y});
  }
  for (const b of boxes) {
    const x = (b.l + b.r) / 2, y = (b.t + b.b) / 2;
    if (within(deepHit(x, y), seen)) return Object.assign(out, {x, y});
  }
  // Something covers it everywhere we looked: its middle, as before.
  const b = boxes.reduce((a, c) => area(c) > area(a) ? c : a);
  return Object.assign(out, {x: (b.l + b.r) / 2, y: (b.t + b.b) / 2, covered: true});
}
// hitsAt is whether a point still lands on the target: pages move.
function hitsAt(t, x, y) {
  const f = targetOf(t);
  return !f.error && within(deepHit(x, y), f.seen);
}
// ---- scrolling the way a hand does ----
function docScroller(win) { const d = win.document; return d.scrollingElement || d.documentElement; }
function scrollable(el, axis) {
  const st = el.ownerDocument.defaultView.getComputedStyle(el);
  if (!/(auto|scroll|overlay)/.test(axis === 'y' ? st.overflowY : st.overflowX)) return false;
  return axis === 'y' ? el.scrollHeight > el.clientHeight + 1 : el.scrollWidth > el.clientWidth + 1;
}
// Whether a wheel over e would move e itself, being able to go that way.
function absorbs(e, dx, dy, isDoc) {
  const y = isDoc ? e.scrollHeight > e.clientHeight + 1 : scrollable(e, 'y');
  const x = isDoc ? e.scrollWidth > e.clientWidth + 1 : scrollable(e, 'x');
  if (dy && y && (dy > 0 ? e.scrollTop < e.scrollHeight - e.clientHeight - 1 : e.scrollTop > 0)) return true;
  if (dx && x && (dx > 0 ? e.scrollLeft < e.scrollWidth - e.clientWidth - 1 : e.scrollLeft > 0)) return true;
  return false;
}
function fixedish(el) {
  for (let n = el; n && n.nodeType === 1; n = parentOf(n)) {
    if (n.ownerDocument.defaultView.getComputedStyle(n).position === 'fixed') return true;
  }
  return false;
}
// The scrollers an element is seen through, innermost first: its scrolling
// ancestors and each document's own, out through the frames.
function chainOf(el) {
  const out = [];
  let n = el, win = el.ownerDocument.defaultView;
  for (let g = 0; g < 16; g++) {
    const ds = docScroller(win);
    for (let p = parentOf(n); p; p = parentOf(p)) {
      if (p !== ds && (scrollable(p, 'y') || scrollable(p, 'x'))) out.push({el: p, win, doc: false});
    }
    out.push({el: ds, win, doc: true});
    let fe = null;
    try { fe = win.frameElement; } catch (e) {}
    if (!fe) break;
    n = fe; win = fe.ownerDocument.defaultView;
  }
  return out;
}
// What a scroller shows, in top-viewport coordinates.
function viewBox(s) {
  const off = frameOffset(s.win);
  if (s.doc) {
    const de = s.win.document.documentElement;
    return {l: off.x, t: off.y, r: off.x + Math.min(de.clientWidth || Infinity, s.win.innerWidth), b: off.y + Math.min(de.clientHeight || Infinity, s.win.innerHeight)};
  }
  const r = s.el.getBoundingClientRect();
  const l = off.x + r.left + s.el.clientLeft, t = off.y + r.top + s.el.clientTop;
  return {l, t, r: l + s.el.clientWidth, b: t + s.el.clientHeight};
}
function clip(a, b) { return {l: Math.max(a.l, b.l), t: Math.max(a.t, b.t), r: Math.min(a.r, b.r), b: Math.min(a.b, b.b)}; }
// Whether a span a0..a1 is in a view v0..v1 well enough: all of it, off the
// very edge, or at least half the view when it is bigger than the view.
function axisOK(a0, a1, v0, v1) {
  const vs = v1 - v0, as = a1 - a0, m = Math.min(8, vs * 0.05);
  if (as <= vs - 2 * m) return a0 >= v0 + m && a1 <= v1 - m;
  return Math.min(a1, v1) - Math.max(a0, v0) >= Math.min(as, vs) * 0.5;
}
// How far to scroll to bring a span to fraction frac of the view (a span
// too big for it, to its start).
function need(a0, a1, v0, v1, frac) {
  if (axisOK(a0, a1, v0, v1)) return 0;
  const vs = v1 - v0;
  if (a1 - a0 <= vs * 0.8) return (a0 + a1) / 2 - (v0 + frac * vs);
  return a0 - (v0 + 0.1 * vs);
}
// As far as a scroller can go, of what is asked.
function roomIn(s, dx, dy) {
  const e = s.el;
  const canY = s.doc || scrollable(e, 'y'), canX = s.doc || scrollable(e, 'x');
  const maxY = e.scrollHeight - e.clientHeight, maxX = e.scrollWidth - e.clientWidth;
  dy = !canY ? 0 : dy > 0 ? Math.min(dy, maxY - e.scrollTop) : Math.max(dy, -e.scrollTop);
  dx = !canX ? 0 : dx > 0 ? Math.min(dx, maxX - e.scrollLeft) : Math.max(dx, -e.scrollLeft);
  return {dx: Math.abs(dx) < 1 ? 0 : dx, dy: Math.abs(dy) < 1 ? 0 : dy};
}
// Whether a wheel at a point would scroll s: what is under it is inside s,
// and nothing between them would take the wheel first.
function wheelReaches(x, y, s, dx, dy) {
  let n = deepHit(x, y);
  if (!n || /^(IFRAME|FRAME|EMBED|OBJECT)$/.test(n.tagName)) return false;
  for (let g = 0; n && g < 4096; g++) {
    if (!s.doc && n === s.el) return true;
    const doc = n.ownerDocument;
    if (n === doc.documentElement) {
      const win = doc.defaultView;
      if (s.doc && s.win === win) return true;
      if (absorbs(docScroller(win), dx, dy, true)) return false;
      try { n = win.frameElement; } catch (e) { n = null; }
      continue;
    }
    if (n !== docScroller(doc.defaultView) && absorbs(n, dx, dy, false)) return false;
    n = parentOf(n);
  }
  return false;
}
// Where to put the pointer to wheel s: where it is, if that works, as a
// person does not move the mouse to scroll; else the nearest of a few
// places in the middle of what s shows that work.
function wheelPoint(s, eff, dx, dy, cur, R) {
  if (cur && cur.x >= eff.l + 2 && cur.x <= eff.r - 2 && cur.y >= eff.t + 2 && cur.y <= eff.b - 2 && wheelReaches(cur.x, cur.y, s, dx, dy)) return {x: cur.x, y: cur.y, stay: true};
  const w = eff.r - eff.l, h = eff.b - eff.t;
  let best = null, bestD = Infinity;
  for (let k = 0, found = 0; k < 24 && found < 6; k++) {
    const x = eff.l + w * (0.2 + 0.6 * R()), y = eff.t + h * (0.2 + 0.6 * R());
    if (!wheelReaches(x, y, s, dx, dy)) continue;
    found++;
    const d = cur ? Math.hypot(x - cur.x, y - cur.y) : 0;
    if (d < bestD) { best = {x, y}; bestD = d; }
  }
  return best || {x: eff.l + w / 2, y: eff.t + h / 2};
}
// locate says whether a target is in view, and if not, the next scroll a
// person would make toward it: which scroller (the innermost that does not
// show it; that scroller's own, first, if it is out of sight itself), by
// how much, to bring it to fractions fr of the view, and where to wheel.
function locate(t, cur, fr, rnd) {
  const f = targetOf(t);
  if (f.error) return f;
  const {seen, how} = f;
  let i = 0;
  const R = () => rnd[(i++) % rnd.length];
  const r0 = seen.getBoundingClientRect();
  if (r0.width === 0 && r0.height === 0) return {error: 'element ' + how + ' has no size (hidden?)'};
  const off = frameOffset(seen.ownerDocument.defaultView);
  let want = {l: r0.left + off.x, t: r0.top + off.y, r: r0.right + off.x, b: r0.bottom + off.y};
  const box = [Math.round(want.l), Math.round(want.t), Math.round(r0.width), Math.round(r0.height)];
  if (fixedish(seen)) return {inView: true, box};
  const chain = chainOf(seen), views = chain.map(viewBox);
  const top = {l: 0, t: 0, r: innerWidth, b: innerHeight};
  for (let k = 0; k < chain.length; k++) {
    const vb = views[k];
    if (axisOK(want.l, want.r, vb.l, vb.r) && axisOK(want.t, want.b, vb.t, vb.b)) continue;
    let eff = clip(vb, top);
    for (let j = k + 1; j < chain.length; j++) eff = clip(eff, views[j]);
    if (eff.r - eff.l >= 24 && eff.b - eff.t >= 24) {
      const room = roomIn(chain[k], need(want.l, want.r, vb.l, vb.r, fr[0]), need(want.t, want.b, vb.t, vb.b, fr[1]));
      if (room.dx || room.dy) {
        const at = wheelPoint(chain[k], eff, room.dx, room.dy, cur, R);
        return {box, dx: Math.round(room.dx), dy: Math.round(room.dy), x: at.x, y: at.y, stay: !!at.stay};
      }
      continue; // it goes no further that way: as good as this one gets
    }
    want = vb; // this scroller is out of sight itself: show it first
  }
  return {inView: true, box};
}
// pageScroll is where to wheel to scroll the page by dx, dy, and how far it
// can go: the document if it can go that way, else the biggest scroller in
// view that can (an app whose body is fixed and whose main pane scrolls).
function pageScroll(cur, dx, dy, rnd) {
  let i = 0;
  const R = () => rnd[(i++) % rnd.length];
  const top = {l: 0, t: 0, r: innerWidth, b: innerHeight};
  const area = b => Math.max(0, b.r - b.l) * Math.max(0, b.b - b.t);
  let s = {el: docScroller(window), win: window, doc: true};
  if (!absorbs(s.el, dx, dy, true)) {
    let best = null, bestA = 0;
    for (const el of walk(document)) {
      if (el.scrollHeight <= el.clientHeight + 1 && el.scrollWidth <= el.clientWidth + 1) continue;
      if (!absorbs(el, dx, dy, false)) continue;
      const c = {el, win: window, doc: false}, a = area(clip(viewBox(c), top));
      if (a > bestA) { best = c; bestA = a; }
    }
    if (!best) return {none: true};
    s = best;
  }
  const eff = clip(viewBox(s), top);
  const room = roomIn(s, dx, dy);
  const at = wheelPoint(s, eff, room.dx || dx, room.dy || dy, cur, R);
  return {x: at.x, y: at.y, stay: !!at.stay, dx: Math.round(room.dx), dy: Math.round(room.dy)};
}
function elementOf(t) {
  if (t.ref) { const n = parseInt(String(t.ref).replace(/^e/, '')); return (window.__cmcpRefs || [])[n - 1] || null; }
  if (t.selector) { try { return document.querySelector(t.selector); } catch (e) { return null; } }
  if (t.text) { const h = findByText(t.text, true); if (h.length) return h[0]; const h2 = findByText(t.text, false); return h2[0] || null; }
  return null;
}
function clearField(t) {
  const el = elementOf(t);
  if (!el) return false;
  if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
    const proto = el.tagName === 'INPUT' ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
    setter.call(el, '');
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
    return true;
  }
  if (el.isContentEditable) {
    el.focus();
    const sel = el.ownerDocument.getSelection(); const range = el.ownerDocument.createRange();
    range.selectNodeContents(el); sel.removeAllRanges(); sel.addRange(range);
    el.ownerDocument.execCommand('delete');
    return true;
  }
  return false;
}
function selectOption(t, value) {
  const el = elementOf(t);
  if (!el) return {error: 'no such element'};
  if (el.tagName !== 'SELECT') return {error: 'element is a <' + el.tagName.toLowerCase() + '>, not a <select>; click it and use browser_click on its options instead'};
  const want = clean(value).toLowerCase();
  let opt = Array.from(el.options).find(o => o.value === value) || Array.from(el.options).find(o => clean(o.text).toLowerCase() === want) || Array.from(el.options).find(o => clean(o.text).toLowerCase().includes(want));
  if (!opt) return {error: 'no option ' + JSON.stringify(value) + '; options: ' + Array.from(el.options).map(o => clean(o.text, 40)).join(' | ')};
  el.value = opt.value;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {selected: clean(opt.text, 80), value: opt.value};
}
function fileInputsIn(doc) {
  const out = [];
  for (const el of walk(doc)) if (el.tagName === 'INPUT' && (el.type || '').toLowerCase() === 'file') out.push(el);
  return out;
}
function allFileInputs() {
  const out = fileInputsIn(document);
  for (const f of document.querySelectorAll('iframe, frame')) {
    try { if (f.contentDocument) out.push(...fileInputsIn(f.contentDocument)); } catch (e) {}
  }
  return out;
}
function describeInput(el) {
  let where = 'input';
  if (el.id) where += '#' + el.id;
  else if (el.name) where += '[name=' + JSON.stringify(el.name) + ']';
  return {tag: 'input', name: el.name || el.id || labelOf(el) || '', selector: where,
          accept: el.getAttribute('accept') || '', multiple: !!el.multiple,
          hidden: !visible(el), disabled: !!el.disabled};
}
// fileTarget finds what a file should be given to, and leaves the element
// on window.__cmcpFileEl for the CDP side to set the files on — the input
// itself where there is one (hidden or not: a picker is never really
// clicked), and 'chooser' where the page builds its input only once the
// button is clicked, which the caller then does with the dialog
// intercepted.
function fileTarget(t) {
  window.__cmcpFileEl = null;
  let el = null;
  if (t && (t.ref || t.selector || t.text)) {
    el = elementOf(t);
    if (!el) return {error: 'no element matches that ref, selector or text — take a new browser_snapshot'};
  }
  if (el) {
    let input = null;
    if (el.tagName === 'INPUT' && (el.type || '').toLowerCase() === 'file') input = el;
    else if (el.tagName === 'LABEL' && el.control && el.control.tagName === 'INPUT' && (el.control.type || '').toLowerCase() === 'file') input = el.control;
    else if (el.querySelector) input = el.querySelector('input[type=file]');
    if (input) { window.__cmcpFileEl = input; return Object.assign({mode: 'input'}, describeInput(input)); }
    if (!visible(el)) return {error: 'that element is not a file input and cannot be clicked (it is hidden)'};
    return {mode: 'chooser'};
  }
  const all = allFileInputs();
  if (all.length === 0) return {error: 'this page has no <input type=file>; if it uploads through a button or a drop zone, ' +
    'give that button as ref/selector/text and its file chooser will be caught'};
  if (all.length > 1) return {error: 'this page has ' + all.length + ' file inputs — say which one, by selector or by ref ' +
    '(browser_snapshot lists them with role "file"): ' +
    all.map(describeInput).map(d => d.selector + (d.name ? ' ' + JSON.stringify(d.name) : '') + (d.hidden ? ' (hidden)' : '')).join(', ')};
  window.__cmcpFileEl = all[0];
  return Object.assign({mode: 'input'}, describeInput(all[0]));
}
// fileInputState reads back what the input ended up holding: the site's own
// view of the upload, and the only proof the files landed.
function fileInputState() {
  const el = window.__cmcpFileEl;
  if (!el || !el.files) return null;
  return Array.from(el.files).map(f => f.name + ' (' + f.size + ' bytes' + (f.type ? ', ' + f.type : '') + ')');
}
function label(on) {
  const old = document.getElementById('__cmcp_labels');
  if (old) old.remove();
  if (!on) return 0;
  const refs = window.__cmcpRefs || [];
  const layer = document.createElement('div');
  layer.id = '__cmcp_labels';
  layer.style.cssText = 'position:fixed;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none;font:11px/1 system-ui,sans-serif;';
  let n = 0;
  refs.forEach((el, i) => {
    if (!el.isConnected) return;
    const r = ((visible(el) ? null : proxyOf(el)) || el).getBoundingClientRect();
    const off = frameOffset(el.ownerDocument.defaultView);
    const x = r.left + off.x, y = r.top + off.y;
    if (r.width === 0 || x + r.width < 0 || y + r.height < 0 || x > innerWidth || y > innerHeight) return;
    const box = document.createElement('div');
    box.style.cssText = 'position:absolute;box-sizing:border-box;border:1.5px solid rgba(220,38,38,.9);left:' + x + 'px;top:' + y + 'px;width:' + r.width + 'px;height:' + r.height + 'px;';
    const tag = document.createElement('div');
    tag.textContent = 'e' + (i + 1);
    tag.style.cssText = 'position:absolute;left:' + Math.max(0, x) + 'px;top:' + Math.max(0, y - 13) + 'px;background:rgba(220,38,38,.92);color:#fff;padding:1px 3px;border-radius:2px;font-weight:600;';
    layer.appendChild(box); layer.appendChild(tag); n++;
  });
  document.documentElement.appendChild(layer);
  return n;
}
function readText(sel, max) {
  let root = document.body;
  if (sel) { try { root = document.querySelector(sel); } catch (e) { return {error: 'bad selector: ' + e.message}; } if (!root) return {error: 'no element matches selector ' + JSON.stringify(sel)}; }
  let text = (root.innerText !== undefined ? root.innerText : root.textContent) || '';
  text = text.replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
  const total = text.length;
  if (max && text.length > max) text = text.slice(0, max);
  return {url: location.href, title: document.title, text, total, truncated: total > text.length};
}
window.__cmcp = {v: 6, snapshot, resolve, pointOn, hitsAt, locate, pageScroll, clearField, selectOption, label, readText, fileTarget, fileInputState};
return window.__cmcp;
})()`

// snapElement is one entry of a snapshot, as the page reports it.
type snapElement struct {
	Ref      string   `json:"ref"`
	Role     string   `json:"role"`
	Tag      string   `json:"tag"`
	Name     string   `json:"name"`
	Box      [4]int   `json:"box"`
	InView   bool     `json:"inView"`
	Level    int      `json:"level,omitempty"`
	Href     string   `json:"href,omitempty"`
	Type     string   `json:"type,omitempty"`
	Value    *string  `json:"value,omitempty"`
	Checked  *bool    `json:"checked,omitempty"`
	Expanded *bool    `json:"expanded,omitempty"`
	Selected *bool    `json:"selected,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type snapResult struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Viewport struct {
		W, H int
	} `json:"viewport"`
	Scroll struct {
		X, Y, PageW, PageH int
	} `json:"scroll"`
	Truncated bool          `json:"truncated"`
	Elements  []snapElement `json:"elements"`
}

// render writes the snapshot as the compact text the agent reads: one line
// per element, viewport elements first when asked.
func (r *snapResult) render(viewportFirst bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %q\nviewport %dx%d, scrolled to (%d,%d) of %dx%d page; %d elements",
		r.URL, r.Title, r.Viewport.W, r.Viewport.H, r.Scroll.X, r.Scroll.Y, r.Scroll.PageW, r.Scroll.PageH, len(r.Elements))
	if r.Truncated {
		sb.WriteString(" (truncated: raise max, or use viewport_only)")
	}
	sb.WriteString("\n\n")
	els := r.Elements
	if viewportFirst {
		els = append([]snapElement(nil), els...)
		sort.SliceStable(els, func(i, j int) bool { return els[i].InView && !els[j].InView })
	}
	offscreen, files := false, false
	for _, e := range els {
		if viewportFirst && !e.InView && !offscreen {
			sb.WriteString("\n-- below/above the viewport (scroll to reach) --\n")
			offscreen = true
		}
		sb.WriteString(e.line())
		sb.WriteByte('\n')
		if e.Role == "file" {
			files = true
		}
	}
	if files {
		sb.WriteString("\nthis page takes a file: file_put puts one on the session, browser_upload gives it to the input.\n")
	}
	return sb.String()
}

func (e *snapElement) line() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s", e.Ref, e.Role)
	if e.Role == "heading" && e.Level > 0 {
		fmt.Fprintf(&sb, " h%d", e.Level)
	}
	if e.Type != "" && e.Type != "text" && e.Role != "checkbox" && e.Role != "radio" && e.Role != "button" && e.Role != "file" {
		fmt.Fprintf(&sb, " (%s)", e.Type)
	}
	if e.Name != "" {
		fmt.Fprintf(&sb, " %q", e.Name)
	}
	if e.Value != nil && *e.Value != "" {
		fmt.Fprintf(&sb, " value=%q", *e.Value)
	}
	if e.Checked != nil {
		if *e.Checked {
			sb.WriteString(" checked")
		} else {
			sb.WriteString(" unchecked")
		}
	}
	if e.Expanded != nil {
		if *e.Expanded {
			sb.WriteString(" expanded")
		} else {
			sb.WriteString(" collapsed")
		}
	}
	if e.Selected != nil && *e.Selected {
		sb.WriteString(" selected")
	}
	if e.Disabled {
		sb.WriteString(" disabled")
	}
	if e.Href != "" {
		fmt.Fprintf(&sb, " -> %s", shortURL(e.Href, 80))
	}
	if len(e.Options) > 0 {
		fmt.Fprintf(&sb, " options=[%s]", strings.Join(e.Options, " | "))
	}
	if !e.InView {
		sb.WriteString(" (offscreen)")
	}
	fmt.Fprintf(&sb, " @%d,%d %dx%d", e.Box[0], e.Box[1], e.Box[2], e.Box[3])
	return sb.String()
}

func shortURL(u string, max int) string {
	if len(u) <= max {
		return u
	}
	return u[:max-1] + "…"
}

// jsCall builds an expression invoking one of the __cmcp functions with
// JSON-encoded arguments, installing the script first if needed.
func jsCall(fn string, args ...any) string {
	parts := make([]string, len(args))
	for i, a := range args {
		b, _ := json.Marshal(a)
		parts[i] = string(b)
	}
	return snapshotJS + "." + fn + "(" + strings.Join(parts, ", ") + ")"
}
