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
if (window.__cmcp && window.__cmcp.v === 5) return window.__cmcp;
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
      const r = w.frameElement.getBoundingClientRect();
      x += r.left; y += r.top; w = w.parent;
    }
  } catch (e) {}
  return {x, y};
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
function resolve(t) {
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
  const via = visible(el) ? null : proxyOf(el);
  try { (via || el).scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'}); } catch (e) {}
  const r = (via || el).getBoundingClientRect();
  const off = frameOffset(el.ownerDocument.defaultView);
  if (r.width === 0 && r.height === 0) return {error: 'element ' + how + ' has no size (hidden?)'};
  const x = r.left + off.x + r.width / 2, y = r.top + off.y + r.height / 2;
  if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) return {error: 'element ' + how + ' is outside the viewport after scrolling'};
  return {x, y, how, tag: el.tagName.toLowerCase(), role: roleOf(el), name: labelOf(el), box: [Math.round(r.left + off.x), Math.round(r.top + off.y), Math.round(r.width), Math.round(r.height)]};
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
window.__cmcp = {v: 5, snapshot, resolve, clearField, selectOption, label, readText, fileTarget, fileInputState};
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
