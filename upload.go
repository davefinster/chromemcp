package main

// The upload link: a way onto a session for bytes that are not already in
// the agent's hands. file_put carries a file inside the tool call, which
// means base64 through the model's context — about 1.4 characters per
// byte, and every one of them paid for twice. Anything bigger than a note
// wants a different road, so file_upload_url hands out a short-lived URL
// that takes the file over ordinary HTTP: a page to drop it on when it is
// on someone's machine, or a plain PUT for curl and anything else that
// speaks HTTP.
//
// The token is the whole authentication, exactly as it is for the live
// view (view.go) and for the same reason: whoever is sending the file has
// a browser or a shell, not the bearer token the rest of the server
// requires. So tokens are unguessable, short-lived, bound to one session,
// and kept apart from the view tokens — a link that lets someone watch a
// browser is not a link that lets them put files in it.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

type uploadToken struct {
	session string
	name    string // the only name this link may write, when one was pinned
	expires time.Time
}

type uploadTokens struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]uploadToken
}

func newUploadTokens(ttl time.Duration) *uploadTokens {
	if ttl <= 0 {
		// A zero TTL would mean every link is dead the moment it is
		// handed out, which looks like the feature being broken rather
		// than switched off.
		ttl = time.Hour
	}
	return &uploadTokens{ttl: ttl, m: map[string]uploadToken{}}
}

// issue mints a link for a session, optionally pinned to one file name.
func (u *uploadTokens) issue(session, name string) (string, time.Time) {
	var b [24]byte
	rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	exp := time.Now().Add(u.ttl)
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, t := range u.m {
		if time.Now().After(t.expires) {
			delete(u.m, k)
		}
	}
	u.m[tok] = uploadToken{session: session, name: name, expires: exp}
	return tok, exp
}

func (u *uploadTokens) lookup(tok string) (uploadToken, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.m[tok]
	if !ok || time.Now().After(t.expires) {
		return uploadToken{}, false
	}
	return t, true
}

// revokeSession is called when a session is deleted. Parking one does not
// revoke anything: a parked session still takes files, which is half the
// point of being able to send them ahead.
func (u *uploadTokens) revokeSession(session string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, t := range u.m {
		if t.session == session {
			delete(u.m, k)
		}
	}
}

type uploadHandler struct{ mgr *manager }

// ServeHTTP handles /upload/<token>[/<name>]:
//
//	GET  the drop page, for a person with the file on their machine
//	PUT  the body as one file       — curl -T report.pdf .../<token>/report.pdf
//	POST multipart/form-data        — the drop page, and curl -F
//
// A file lands with the name given in the path (or the pinned one), and
// replaces a file of that name the session already holds: a transfer sent
// twice should end with the file there once, not fail the second time.
func (h *uploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upload/")
	tok, name, slash := strings.Cut(rest, "/")
	if tok == "" {
		http.NotFound(w, r)
		return
	}
	t, ok := h.mgr.uploads.lookup(tok)
	if !ok {
		http.Error(w, "this upload link has expired; ask for a new one (file_upload_url)", http.StatusForbidden)
		return
	}
	s, err := h.mgr.get(t.session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !slash {
			// So the page's relative URLs land back under this token.
			http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
			return
		}
		h.page(w, s, t)
	case http.MethodPut, http.MethodPost:
		h.receive(w, r, s, t, name)
	default:
		w.Header().Set("Allow", "GET, PUT, POST")
		http.Error(w, "the upload link takes GET (the page), PUT (one file) or POST (a form)", http.StatusMethodNotAllowed)
	}
}

// receive writes whatever the request carries onto the session.
func (h *uploadHandler) receive(w http.ResponseWriter, r *http.Request, s *session, t uploadToken, name string) {
	s.touch()
	var landed []sessionFile

	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method == http.MethodPost && strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		mr, err := r.MultipartReader()
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, err)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				h.fail(w, r, http.StatusBadRequest, err)
				return
			}
			n := part.FileName()
			if n == "" {
				part.Close()
				continue // a plain form field, not a file
			}
			f, err := h.store(s, t, n, part)
			part.Close()
			if err != nil {
				h.fail(w, r, http.StatusBadRequest, err)
				return
			}
			landed = append(landed, *f)
		}
		if len(landed) == 0 {
			h.fail(w, r, http.StatusBadRequest, fmt.Errorf("the form carried no file"))
			return
		}
	} else {
		if name == "" {
			name = t.name
		}
		if name == "" {
			h.fail(w, r, http.StatusBadRequest, fmt.Errorf(
				"name the file in the path — PUT %s<name>, with the extension the site expects", r.URL.Path))
			return
		}
		f, err := h.store(s, t, name, r.Body)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, err)
			return
		}
		landed = append(landed, *f)
	}
	h.done(w, r, s, landed)
}

// store writes one file, holding a pinned link to the name it was pinned to.
func (h *uploadHandler) store(s *session, t uploadToken, name string, body io.Reader) (*sessionFile, error) {
	name = sanitizeUploadName(name)
	if t.name != "" && name != t.name {
		return nil, fmt.Errorf("this link is for %q; it cannot write %q", t.name, name)
	}
	f, err := s.putFileFrom(name, body, putOverwrite)
	if err != nil {
		return nil, err
	}
	logf("session %s: %s uploaded over the link (%s)", s.meta.ID, f.Name, humanBytes(f.Size))
	return f, nil
}

// sanitizeUploadName keeps only the last element of what a browser sent —
// some send a whole path — and leaves the rest to checkFileName, whose
// error tells the sender what a name may look like.
func sanitizeUploadName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func (h *uploadHandler) wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (h *uploadHandler) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	if h.wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	http.Error(w, err.Error(), code)
}

func (h *uploadHandler) done(w http.ResponseWriter, r *http.Request, s *session, landed []sessionFile) {
	if h.wantsJSON(r) {
		out := make([]map[string]any, len(landed))
		for i, f := range landed {
			out[i] = map[string]any{"name": f.Name, "size": f.Size, "type": f.MIME}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": s.meta.ID, "files": out})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, f := range landed {
		fmt.Fprintf(w, "%s on session %s (%s)\n", f.Name, s.meta.ID, humanBytes(f.Size))
	}
}

// page is the drop page: the thing to send someone when the file is on
// their machine and nowhere else. Self-contained, because it is served
// from a token URL that nothing else may be loaded from.
func (h *uploadHandler) page(w http.ResponseWriter, s *session, t uploadToken) {
	var b [16]byte
	rand.Read(b[:])
	nonce := hex.EncodeToString(b[:])
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+
		"'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pinned := "any file"
	if t.name != "" {
		pinned = html.EscapeString(t.name) + " only"
	}
	fmt.Fprintf(w, dropPage, nonce, nonce,
		html.EscapeString(s.meta.ID), pinned, humanBytes(maxFileBytes),
		t.expires.Format("15:04 on 2 Jan"), html.EscapeString(t.name))
}

// dropPage takes: script nonce, style nonce, session id, what it accepts,
// the per-file limit, when the link dies, and the pinned name (or "").
const dropPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Send a file to this browser</title>
<style nonce="%[1]s">
:root { color-scheme: light dark; --bg:#fbfbfa; --fg:#1a1a18; --dim:#6b6b66; --line:#dcdcd6; --accent:#3b6ea5; --ok:#2f7d4f; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#17171a; --fg:#e9e9e4; --dim:#9a9a94; --line:#33333a; --accent:#7aa9d8; }
}
* { box-sizing:border-box }
body { margin:0; padding:32px 16px; background:var(--bg); color:var(--fg);
  font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif; }
main { max-width:560px; margin:0 auto }
h1 { font-size:19px; margin:0 0 4px; font-weight:600 }
p.sub { margin:0 0 24px; color:var(--dim); font-size:13.5px }
code { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12.5px }
#drop { border:1.5px dashed var(--line); border-radius:12px; padding:40px 24px; text-align:center;
  transition:border-color .15s, background .15s; cursor:pointer }
#drop.over { border-color:var(--accent); background:color-mix(in srgb, var(--accent) 8%%, transparent) }
#drop strong { display:block; font-weight:550; margin-bottom:4px }
#drop span { color:var(--dim); font-size:13px }
input[type=file] { display:none }
ul { list-style:none; padding:0; margin:20px 0 0 }
li { display:flex; align-items:center; gap:10px; padding:9px 0; border-top:1px solid var(--line); font-size:13.5px }
li .nm { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
li .st { color:var(--dim); font-size:12.5px; font-variant-numeric:tabular-nums }
li.done .st { color:var(--ok) }
li.err .st { color:#c1492f }
.bar { height:3px; background:var(--line); border-radius:2px; overflow:hidden; margin-top:6px }
.bar i { display:block; height:100%%; width:0; background:var(--accent); transition:width .1s linear }
footer { margin-top:28px; color:var(--dim); font-size:12.5px; line-height:1.7 }
</style></head>
<body><main>
<h1>Send a file to this browser</h1>
<p class="sub">Session <code>%[3]s</code> · accepts %[4]s, up to %[5]s each</p>

<div id="drop" tabindex="0" role="button" aria-label="Choose files to send">
  <strong>Drop files here</strong>
  <span>or click to choose</span>
</div>
<input type="file" id="pick" multiple>
<ul id="list"></ul>

<footer>
Files land on the session and stay until it is deleted. The agent can then hand them to a
page's file picker.<br>This link stops working at %[6]s.
</footer>
</main>
<script nonce="%[2]s">
const pinned = %[7]q;
const drop = document.getElementById('drop'), pick = document.getElementById('pick'), list = document.getElementById('list');
drop.onclick = () => pick.click();
drop.onkeydown = e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick.click(); } };
for (const ev of ['dragenter', 'dragover']) drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.add('over'); });
for (const ev of ['dragleave', 'drop']) drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.remove('over'); });
drop.addEventListener('drop', e => send(e.dataTransfer.files));
pick.onchange = () => { send(pick.files); pick.value = ''; };

function send(files) { for (const f of files) one(f); }

function one(file) {
  const li = document.createElement('li');
  const nm = document.createElement('div'); nm.className = 'nm';
  const name = document.createElement('div'); name.textContent = file.name;
  const bar = document.createElement('div'); bar.className = 'bar';
  const fill = document.createElement('i'); bar.appendChild(fill);
  nm.append(name, bar);
  const st = document.createElement('div'); st.className = 'st'; st.textContent = '0%%';
  li.append(nm, st); list.prepend(li);

  const x = new XMLHttpRequest();
  x.open('PUT', './' + encodeURIComponent(pinned || file.name));
  x.setRequestHeader('Accept', 'application/json');
  x.upload.onprogress = e => {
    if (!e.lengthComputable) return;
    const pct = Math.round(e.loaded / e.total * 100);
    fill.style.width = pct + '%%'; st.textContent = pct + '%%';
  };
  x.onload = () => {
    let body = {};
    try { body = JSON.parse(x.responseText); } catch (_) {}
    if (x.status === 200 && body.ok) {
      li.className = 'done'; st.textContent = 'sent'; fill.style.width = '100%%';
    } else {
      li.className = 'err'; st.textContent = body.error || ('failed (' + x.status + ')'); bar.remove();
    }
  };
  x.onerror = () => { li.className = 'err'; st.textContent = 'failed'; bar.remove(); };
  x.send(file);
}
</script></body></html>
`
