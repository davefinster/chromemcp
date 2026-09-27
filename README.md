# chromemcp

A [Model Context Protocol](https://modelcontextprotocol.io) server that gives
an agent Chrome browsers of its own: it launches Chrome instances — headless,
or headful on a private VNC display — drives them, and hands back what the
page looks like after every action. It sits behind an OAuth authorization
server of your choosing (built for WorkOS AuthKit, but any RFC 8414 issuer
works), and can pass each session through to Google's
[chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp)
for the low-level work it does not reimplement.

One Go binary, five parts:

| part | what |
|---|---|
| MCP server | Streamable HTTP (or stdio), OAuth-protected: AuthKit issuer, resource identifier, single-account email pin. 34 tools. |
| sessions | One Chrome per session on its own profile directory, like the first launch on a clean workstation, with whatever files the agent has put on it for a page's file picker. Ephemeral: parked when idle (Chrome closed, profile kept, resumable), deleted when old. |
| identities | Named snapshots of a profile the owner has logged into — the persistent part. A session started *as* an identity begins already signed in. |
| live view | Headful sessions render on an Xvnc display; a token link serves noVNC over the server's own websocket bridge, so a person can watch, take over, and sign in where no agent can (passkeys, 2FA). A session with no display — a node's, or a headless one — gets a screencast view instead: frames from DevTools, mouse and keys sent back. |
| nodes | `chromemcp node` on another machine — a Mac — runs sessions' Chrome for the server over mutual TLS, so `device=mac` is a real Mac's Chrome with its own GPU and fonts ([Nodes](#nodes)). |

## Tools

**Sessions**

| tool | purpose |
|---|---|
| `session_start` | a new Chrome on a fresh profile, or on a copy of an identity's; `mode` headless (default) or headful; `device` profile (`windows`, the default, or `linux`), `timezone`, `locale`; `viewport`, `label`, `url` |
| `session_list` | running and parked sessions (what can be resumed), plus the saved identities |
| `session_resume` / `session_stop` | relaunch a parked session with its cookies, storage and tabs; park a running one |
| `session_delete` | close and erase a session |
| `session_view` | a live-view link for a person to open: noVNC for a headful session here, a screencast for any other |

**Identities**

| tool | purpose |
|---|---|
| `identity_list` | the saved identities |
| `identity_save` | snapshot a session's profile and cookie jar under a name (Chrome is closed cleanly around the copy and started again) |
| `identity_delete` | remove one |

**Session files** — bytes a page can be given through its file picker,
deleted with the session

| tool | purpose |
|---|---|
| `file_put` | write a file onto a session: `content` (base64, or a `data:` URL) or `text`, under the `name` the site should see; `overwrite` replaces one, `append` adds to the end of one |
| `file_upload_url` | a short-lived link that takes files over plain HTTP — a page to drop them on, or `curl -T`; `name` pins it to one file |
| `file_list` | what a session holds, put or downloaded, with sizes and the type each name implies |
| `file_delete` | remove one — after the upload has gone through: a page given a file holds a reference to it, not a copy |

Up to 20 MB a file and 100 MB (or 50 files) a session. There are three ways
in, and they cost very different amounts:

- **the browser downloads it** — free. Chrome saves what a session downloads
  into the session, where `file_list` shows it and `browser_upload` hands it
  to the next site. Fetching a file from one page and giving it to another
  puts none of it through the agent's context.
- **`file_upload_url`** — free, and the answer when the file is on a
  person's machine: send them the link, they drop the file on the page it
  serves. `curl -T report.pdf <link>report.pdf` does the same from a shell.
- **`file_put`** — base64 inside the tool call, about 1.4 characters of
  context per byte, paid on the way in and again on every turn that
  transcript survives. Right for something small, wrong for a photo.
  `append` sends one in chunks when there is no other road.

**Browser** (every tool takes `session_id`, optionally `tab`; most return a
screenshot, `screenshot=false` turns it off)

| tool | purpose |
|---|---|
| `browser_navigate`, `browser_history` | go to a URL; back / forward / reload |
| `browser_snapshot` | the page's interactive elements with a ref each (`e12`), role, label, value, position; `labels=true` draws them on a screenshot |
| `browser_screenshot` | viewport, full page, or one element; jpeg/png, quality, downscale, ref labels |
| `browser_click`, `browser_hover` | by ref, visible text, CSS selector, or viewport coordinates; real mouse events |
| `browser_type`, `browser_press`, `browser_select` | type into a field (optionally clear, submit); press keys with modifiers; choose a `<select>` option |
| `browser_scroll` | wheel-scroll the page, or bring an element into view |
| `browser_upload` | give the page files from the session (`file_list` shows them, put or downloaded) as the file dialog would — the input itself, a hidden one behind its label, or the button whose file chooser is caught |
| `browser_wait` | a delay, or until an element / text appears (or disappears), or the URL changes |
| `browser_read` | the page's rendered text — cheap, no screenshot |
| `browser_evaluate` | JavaScript in the page, result as JSON |
| `browser_fingerprint` | what the page's scripts see of the device — user agent and client hints, platform, languages, time zone, screen, GPU strings, fonts, and the same from a worker — for checking what a device profile presents |
| `browser_console` | console messages and exceptions the tab has logged |
| `browser_tabs`, `browser_tab_new`, `browser_tab_select`, `browser_tab_close` | tabs, aliased `t1`, `t2`, … |

**DevTools passthrough**

| tool | purpose |
|---|---|
| `devtools_tools` | the live tool list and schemas of chrome-devtools-mcp, attached to this session's Chrome |
| `devtools_call` | call one of them; page-scoped tools go to the session's current tab unless a `pageId` is given |

The passthrough is a pair rather than one registered tool per DevTools tool,
so this server's tool list stays stable across chrome-devtools-mcp releases
and an agent reads the schemas when it needs them. Each session gets its own
`chrome-devtools-mcp` process on first use, connected over the session's
DevTools port, and it dies with the session.

## How it works

```
claude.ai ──HTTPS──▶ edge ──▶ chromemcp :8787
                               ├─ /             MCP + OAuth
                               ├─ /view/<tok>/  noVNC + websocket ⇄ Xvnc   (headful sessions)
                               ├─ /upload/<tok>/ drop page + PUT/POST → sessions/s-…/files/
                               ├─ /healthz
                               └─ sessions/
                                   ├─ s-…/profile/   Chrome --user-data-dir, --headless=new
                                   ├─ s-…/cookies.json
                                   ├─ s-…/files/     what was put there, for a page's file picker
                                   ├─ s-…/downloads/ what Chrome downloaded, offered the same way
                                   └─ s-…/            Xvnc :N ◀── Chrome (headful) ◀── chrome-devtools-mcp
                                  identities/<name>/{profile/, cookies.json, identity.json}
```

- **Chrome is this server's child**, not chromedp's: the server owns the
  profile directory, the display, and the DevTools port (read from Chrome's
  `DevToolsActivePort` file) that the passthrough attaches to. It connects
  over CDP with [chromedp](https://github.com/chromedp/chromedp) and drives
  one tab context per page target.
- **The snapshot is a script** evaluated in the page (shadow roots and
  same-origin frames included) that lists visible interactive and structural
  elements and keeps the element references on `window`, never in the DOM.
  Refs are renumbered by every snapshot and cleared by navigation; clicking
  by text finds the innermost visible match and promotes it to its link or
  button. Visually hidden native controls fronted by a styled label (the
  usual custom checkbox) are reported and clicked through the label.
- **Headless tabs that are not in front are hidden** to Chrome and stop
  rendering — a screenshot of one waits for a frame that never comes. Every
  operation brings its tab to the front first.
- **Cookies are carried by the server as well as by Chrome.** Chrome
  commits cookies to SQLite on a 30-second timer and flushes them on a
  graceful `Browser.close` (sent over a websocket of the server's own, since
  chromedp refuses to send it to a browser it did not launch), but not when
  it has to be signalled instead, and then a login made in the last half
  minute before a park would be lost. So every park also exports the jar
  over CDP to `cookies.json`, and every launch imports it back before the
  first page loads, replacing the on-disk store's contents. The remembered
  tabs are reopened after that. Local storage and IndexedDB are written
  promptly by Chrome and travel with the profile copy.
- **Identities are profile copies** minus caches (`copyProfile` skips the
  cache directories, lock files and the port file) plus the cookie jar.
  Chrome runs with `--password-store=basic` so a profile's encrypted data
  decrypts wherever it is restored. Saving an identity parks the session,
  copies, and relaunches it; the copy is built beside the old identity and
  swapped in, so a failed save never leaves a half identity.
- **The live view's token is its whole authentication**: a browser opening a
  link cannot carry the bearer token the MCP endpoint requires. Tokens are
  48 hex characters, bound to one session, expire (`-view-ttl`, 30 minutes),
  and are revoked when the session is stopped or deleted. Xvnc listens on a
  unix socket only (a loopback port when the path would be too long for a
  socket), and the bridge dials it per websocket. A connected viewer keeps
  the session from being parked.
- **The upload link is the same idea for bytes.** `file_upload_url` mints a
  token of its own — separate from the view tokens, so a link that lets
  someone watch a browser is not a link that lets them put files in it —
  and `/upload/<tok>/` serves a self-contained drop page (inline style and
  script under a nonce, `default-src 'none'`) that `PUT`s each file with an
  `XMLHttpRequest`. `curl -T file <link>name` and `curl -F` reach the same
  handler, which streams straight to disk and enforces the size limits
  against the stream rather than after buffering it. A file replaces one of
  that name, because a transfer that has to be repeated should not have to
  be renamed. Links last `-upload-ttl` (an hour), survive parking — a
  parked session takes files perfectly well — and die with the session.
- **Uploads go through the file dialog, not around it.** The bytes land in
  `sessions/s-…/files/` — inside the session directory, so they are kept
  while it is parked and deleted with it, and `identity_save`, which copies
  only the profile and the cookie jar, never carries one into another
  session. `sessions/s-…/downloads/` is where Chrome has always put what a
  session downloads, and a listing merges the two: to the page being handed
  a file there is no difference, so a download is uploadable without
  anything having to read it. A name held on both sides resolves to the put
  file, and the listing says the download is there rather than letting it
  disappear. A download still arriving (Chrome's `.crdownload`) is listed
  under the name it will have and refused until it is finished. `browser_upload` then hands them over with
  `DOM.setFileInputFiles`, which fills the input exactly as a person
  choosing the file does, `input` and `change` firing over a real `FileList`
  — nothing a script in the page can construct. It finds the input behind
  the widget (the element itself, a `<label>`'s control, an input nested in
  a drop zone), and a hidden one is fine because no click is needed; where
  the page has no input until its button is clicked, the chooser is
  intercepted (`Page.setInterceptFileChooserDialog`, always turned off
  again so a person at the live view still gets their own dialog) and the
  input Chrome names in the event is filled instead. The answer reads back
  what the input ended up holding, and says so when the file falls outside
  the input's `accept` — which a site silently ignores, looking exactly
  like nothing having happened.
- **Dialogs are accepted** (alert/confirm/prompt/beforeunload) so a page never
  blocks on one nobody can see; the message is reported with the next result.

## Device profiles

A session's Chrome is this server's Chrome: Linux, a container's screen, a
software GPU, and in headless mode a user agent that says `HeadlessChrome`.
A `device` profile on `session_start` changes what the browser says about
the machine it runs on — never the browser itself, since a site can compare
the two and Chrome's version, features and TLS fingerprint stay its own.
The default is `windows`: what most of the web expects of an ordinary
visitor, and what its bot heuristics score as one; `linux` is this Chrome
as it is, for comparing how a site treats the two. A session records the
profile it was started with, so a parked one resumes as what it was (one
from before profiles were recorded is `linux`, which is what it was).

| profile | what sites see |
|---|---|
| `linux` | this Chrome as it is |
| `windows` (default) | a Windows 11 PC running the same Chrome version: `Mozilla/5.0 (Windows NT 10.0; Win64; x64) … Chrome/N.0.0.0 …`; `Sec-CH-UA-Platform: "Windows"` with the high-entropy hints of a 64-bit x86 desktop on 24H2 (`platformVersion` 19.0.0); `navigator.platform` `Win32`; `navigator.userAgentData` to match; a 1920×1080 display at 100 % scale; an NVIDIA GeForce RTX 3060 under ANGLE/D3D11 in `WEBGL_debug_renderer_info`; `navigator.webdriver` false |

Independently of the profile, `timezone` (an IANA zone) and `locale` (a
language tag) set where and in what language the browser runs — `TZ` and
`--accept-lang` plus `LANG`/`LANGUAGE` on the Chrome process, so `Date`,
`Intl`, `Accept-Language` and `navigator.languages` all agree, workers
included. (`Intl`'s default locale is Chrome's UI language for the tag, as
Chrome resolves it: `en-AU` becomes `en-GB`, the way it does on a real
machine.) The defaults are the server's: UTC and `en-US` in the container.

How a profile is applied (`emulate.go`, `device.go`):

- The user agent, `navigator.platform` and the client-hint metadata go on
  **every target** through a second, minimal CDP client on the browser
  websocket that auto-attaches — pages, out-of-process iframes, dedicated,
  shared and service workers — with `waitForDebuggerOnStart`, so Chrome holds
  each new target until the overrides are in place. The client-hint brand
  list (`"Chromium";v="152", "Not?A_Brand";v="24", …`) is generated with
  Chrome's own algorithm for the running version, so it is exactly what this
  Chrome sends unemulated; the integration test checks that against the live
  browser. The same client emulates the viewport in headless mode (it used to
  be done per tab on adoption) and the screen size in both modes.
- The user agent is also on the command line (`--user-agent`), because a
  popup's first navigation and a service worker's script fetch are requested
  by the browser before any DevTools session can reach the new target — the
  `waitForDebuggerOnStart` pause holds the renderer, not the browser-initiated
  document request. The flag fixes the user-agent *string* on those, but not
  the low-entropy client hints, so the same client also turns on **browser-level
  request interception** (`Fetch.enable` on the browser session, which sees
  those first requests) and rewrites `Sec-CH-UA-Platform` — and the brand and
  mobile hints — to the profile's values on any request that still carries
  Chrome's own. It also caps `Sec-CH-Device-Memory` / `Device-Memory` (the
  hint that carries device memory over the wire, which no override touches, so
  the container's host figure — 16 or 32 — leaks into it; a real browser caps
  the hint at 8, making anything larger an impossible value that anti-bots read
  as a VM, and Akamai on mouser.com answers a request bearing it with an HTTP/2
  stream reset the browser shows as `ERR_HTTP2_PROTOCOL_ERROR`). Only hints
  already present are rewritten, never added, so a request sends exactly what
  Chrome chose to, with the machine values corrected; every request is continued
  whatever happens, and Chrome drops the interception by itself if the client
  goes away, so a page never hangs on it.
- A script evaluated in every new document (and in every worker before its
  own script) handles what the protocol cannot, or cannot reliably:
  `navigator.platform` (Blink keeps the protocol's override in per-page
  settings that every attached session — chromedp's included — restores after
  a process swap, the sessions with nothing set included, so it does not
  survive the first navigation; and workers never get it), `navigator.webdriver`
  (true in any Chrome with remote debugging on; the flag that turns it off
  puts an "unsupported command-line flag" bar over headful windows),
  the WebGL vendor and renderer strings, `uaFullVersion` (which Chrome blanks
  when the user agent comes from the command line), `window.outerWidth` /
  `outerHeight` when they read 0 — which a freshly opened tab does for a moment
  and a headless window always does, a "no window" bot signal — reported then
  as the inner size plus a frame, and the machine numbers a container reports
  from its host rather than a consumer PC: `navigator.deviceMemory` (the
  container sees 16 or 32 GB, but a real browser caps this at 8, so anything
  higher is impossible and marks a VM) is pinned to 8 — matching the
  `Sec-CH-Device-Memory` header the request interception caps to the same value
  — `navigator.hardwareConcurrency` (a server's core count) to 8, and
  `screen.availHeight` reserves a 48px taskbar (emulated device metrics
  otherwise leave it equal to the screen height — no desktop chrome), page and
  workers alike. The patched functions report `[native code]` to
  `Function.prototype.toString`.
- Headful sessions run with `--enable-unsafe-swiftshader`, since without a
  GPU headful Chrome otherwise has no WebGL at all (headless falls back to
  SwiftShader by itself), and a browser with no WebGL is nothing like a PC.
- Without an input device Chrome answers `(pointer: none)` and
  `(hover: none)` — headless and on an Xvnc alike — and a site shows its
  touch layout; a profile sets Blink's pointer and hover types on the
  command line (`--blink-settings`) to a mouse's. Its headless window is
  also made a frame's worth bigger than the viewport, so `outerWidth` and
  `outerHeight` exceed the inner ones the way a real window's do.
- **Fonts** are the loudest thing after the user agent: a site lays text
  out in the first family of its stack that exists, and fingerprinting
  scripts measure a list of families to see which do — and, more subtly,
  measure the CSS generic families (`system-ui`, `fantasy`, …) to tell one
  OS and browser from another. The profile gives Chrome a font world of its
  own (`FONTCONFIG_FILE`; `fonts.go`): a directory of links to the image's
  fonts, and a configuration that is the whole of what Chrome can see — the
  system's own is deliberately not included. Each font is renamed, as
  fontconfig scans it, to the families it stands in for and to nothing else:
  Segoe UI is Selawik, Microsoft's own OFL-licensed metric-compatible
  stand-in; Calibri and Cambria are Carlito and Caladea; Arial, Times New
  Roman and Courier New are Liberation; Verdana, Tahoma and the rest of the
  common ones are DejaVu; Impact and the condensed faces are Liberation Sans
  Narrow; the East Asian families are Noto CJK. So the Windows families are
  present because a font really carries those names, and `"DejaVu Sans"` in a
  stylesheet falls through the way it does on Windows because nothing carries
  that name any more. It is generated against what `fc-list` says is
  installed, so a deployment with fewer fonts simply has fewer Windows
  families, and built once per sessions directory (~25ms, `fc-cache` included).

  It was done the other way about until Chrome 154 — the system fonts left in
  place, Windows families aliased onto them, and the image's names hidden by
  rewriting any request that led with one. That rested on Chrome refusing a
  match whose family was not the one asked for; 154 no longer refuses it, and
  every hidden name came back visible while the aliases went on working.
  Renaming the fonts needs no such cooperation.

  One tell survives and cannot be removed here: Skia keeps its own table of
  metric-compatible families and answers a request for either of a pair with
  the other, whatever fontconfig says. A profile that must have Arial
  therefore also answers to Liberation Sans, and likewise Times New
  Roman/Liberation Serif, Courier New/Liberation Mono, Calibri/Carlito and
  Cambria/Caladea. Only the unpaired names — DejaVu, Noto, Selawik, Ubuntu —
  can be made to disappear, and `TestDeviceIntegration` asserts exactly those.
- The **generic font families** — what `system-ui`, `serif`, `fantasy` and
  the rest resolve to — Blink picks from its own preferences, not fontconfig,
  so the profile also seeds the profile's `Default/Preferences` (merged, before
  Chrome opens it) with the family names Chrome ships on Windows: `standard`
  and `serif` Times New Roman, `sansserif` Arial, `fixed` Consolas, `cursive`
  Comic Sans MS, `fantasy` Impact — resolved through the same Windows aliases
  to stand-ins. Without this a script that measures `fantasy` (Impact, narrow
  on Windows) against `system-ui` (Segoe UI) finds them the wrong way round —
  the Linux fallbacks make `fantasy` the wider — and reads the box as Firefox
  on Linux, contradicting the Chrome user agent and scoring the session as
  "tampered". With it the metrics line up as Windows Chrome's do.

What a profile does not change is what a Linux container without a GPU
cannot fake convincingly: the glyph shapes and antialiasing behind the font
names (Selawik is not Segoe UI, and Linux does not render like DirectWrite),
canvas and audio rendering, the media devices and speech voices, and above
all the WebGL *rendering* — the renderer string says GeForce, but the pixels
and the capabilities behind it are SwiftShader's, which a script that hashes a
WebGL draw or reads its limits can still tell apart from real hardware. Nor
does it supply human behaviour: mouse movement, timing and interaction
history. Anti-bot services that weigh those — Cloudflare, and the
PerimeterX/HUMAN "press and hold" that Walmart runs — can still challenge a
session the fingerprint alone would pass, especially a "cold" one with no
interaction, and their verdict is probabilistic and reputation-weighted. The
profile makes the machine look like a consumer Windows PC; it does not make an
automated visit look like a person's.

`browser_fingerprint` reports the observable values from inside a session,
and `testdata/fingerprint.js` is the same script: pasted into the DevTools
console of a real Windows Chrome it prints the same object and copies it to
the clipboard, for a side-by-side comparison — the way to check a profile,
or to collect the values for a new one.

## Running

```bash
go build -o chromemcp .

# local: no auth, sessions under $TMPDIR, identities under ~/.local/share/chromemcp
chromemcp serve -http 127.0.0.1:8787

# stdio, for Claude Code / Claude Desktop
chromemcp serve

# the deployment
chromemcp serve -http :8787 \
  -oauth-issuer https://<env>.authkit.app \
  -public-url https://chromemcp.example.com \
  -allowed-email you@example.com \
  -identities-dir /data/identities
```

Every flag has an environment fallback (`chromemcp serve -h`). The ones that
matter:

| flag | env | default |
|---|---|---|
| `-chrome` | `CHROMEMCP_CHROME`, `CHROME_PATH` | first of `google-chrome-stable`, `google-chrome`, `chromium` on PATH |
| `-no-sandbox` | `CHROMEMCP_NO_SANDBOX` | off (on in the container) |
| `-xvnc` | `CHROMEMCP_XVNC` | `Xvnc` on PATH; empty disables headful sessions |
| `-novnc` | `CHROMEMCP_NOVNC` | `/usr/share/novnc` |
| `-devtools-mcp` | `CHROMEMCP_DEVTOOLS_MCP` | `chrome-devtools-mcp` on PATH, else `npx -y chrome-devtools-mcp@1.8.0`; empty disables the passthrough |
| `-sessions-dir` | `CHROMEMCP_SESSIONS_DIR` | `$TMPDIR/chromemcp-sessions` |
| `-identities-dir` | `CHROMEMCP_IDENTITIES_DIR` | `~/.local/share/chromemcp/identities` |
| `-viewport` | `CHROMEMCP_VIEWPORT` | `1280x800` |
| `-idle-park`, `-max-age`, `-max-running` | `CHROMEMCP_IDLE_PARK`, … | 30m, 24h, 6 |
| `-view-ttl`, `-view-url` | `CHROMEMCP_VIEW_TTL`, `CHROMEMCP_VIEW_URL` | 30m; `-public-url` |
| `-upload-ttl` | `CHROMEMCP_UPLOAD_TTL` | 1h (upload links; they share `-view-url`) |
| `-guidance`, `-guidance-file` | `CHROMEMCP_GUIDANCE`, `CHROMEMCP_GUIDANCE_FILE` | none ([Deployment guidance](#deployment-guidance)) |
| `-node NAME=URL` | `CHROMEMCP_NODES` (space-separated) | none; repeatable ([Nodes](#nodes)) |
| `-node-cert`, `-node-key`, `-node-ca` | `CHROMEMCP_NODE_CERT`, `…_KEY`, `…_CA` | the client certificate presented to nodes, and the CA theirs chain to |

`GET /healthz` is unauthenticated and reports the version, the session
counts, and whether headful sessions and the passthrough are available.

Built against WorkOS AuthKit, which needs three things set up on its side,
per server: Dynamic Client Registration enabled (claude.ai registers itself),
`-public-url` registered as a **Resource Indicator** (the `resource` claude.ai
sends at authorize time, and the `aud` the tokens must carry — an
unregistered one is refused at the authorization endpoint, which claude.ai
reports as `state: Field required` from its callback), and a JWT template of
`{"email": "{{user.email}}"}` so the `-allowed-email` pin has a claim to
match. Any RFC 8414 issuer that puts those claims in an RS256 access token
works just as well.

### Deployment guidance

What a deployment prefers — which device for what, which identity for which
accounts, headful or not — is the operator's to say, not this repository's.
`-guidance` (or `-guidance-file`) is that text. It goes **first** in the
server's instructions, ahead of the generic text, because a client may cut
instructions short (claude.ai does, at a couple of thousand characters), and
again at the end of `session_start`'s description, which is in front of an
agent when it picks a setup. Keep it short and concrete, for example:

```bash
chromemcp serve ... -guidance 'For anonymous browsing, session_start device="mac" (headless).
For tasks that need my own accounts, device="windows", identity="me", mode="headful".'
```

Without it the server says nothing of the kind, and agents take the
defaults.

### Logging in as yourself

An agent must never be handed a password, and Google's sign-in (passkeys,
2FA, device prompts) could not be scripted anyway. The flow is:

1. The agent starts a headful session and asks for `session_view`.
2. You open the link, sign in to Google (or whatever) in that Chrome, and
   close the tab. The link is good for 30 minutes; the session keeps whatever
   you logged into.
3. The agent (or you, through it) calls `identity_save` with a name, say
   `google-me`.
4. From then on `session_start identity=google-me` begins signed in. Refresh
   the identity the same way when the login expires (`overwrite=true`).

Sessions started from an identity work on a *copy*: nothing they do changes
the identity unless it is saved again.

## Container

`ghcr.io/davefinster/chromemcp`, built for amd64 and arm64 by
`.github/workflows/docker.yml`. Debian trixie with Google Chrome from
Google's own apt repository (which serves both architectures), TigerVNC's
Xvnc, noVNC, Node 20 and `chrome-devtools-mcp` 1.8.0, and the fonts the
device profiles draw on (Liberation, Liberation Sans Narrow — a separate
package from Liberation, and the condensed stand-in for Impact and Arial
Narrow — DejaVu, Carlito, Caladea, Noto, and Selawik from its GitHub release,
checksum pinned); runs as uid 1000 with identities on `/data/identities` and
sessions under `/tmp`:

```bash
docker run -d --name chromemcp -p 127.0.0.1:8787:8787 -v chromemcp:/data --shm-size=1g \
  ghcr.io/davefinster/chromemcp serve -http :8787 \
    -oauth-issuer https://<env>.authkit.app -public-url https://chromemcp.example.com \
    -allowed-email you@example.com
```

The image sets `CHROMEMCP_NO_SANDBOX=1`: Chrome's sandbox needs user
namespaces that container runtimes usually withhold. Drop it on a runtime
that grants them. `--disable-dev-shm-usage` is always on, so a small
`/dev/shm` does not crash tabs, but a real one is kinder.

On Kubernetes: the pod behind whatever reverse proxy terminates TLS for it,
`/healthz` as the backend check, a persistent claim mounted at `/data` for
the identities (the one thing worth keeping), an emptyDir for `/tmp`, and a
memory request that allows for `-max-running` Chromes at a few hundred MB
each. The proxy must pass websockets for `/view/`.

## Nodes

A server in a container runs Chrome on SwiftShader and stand-in fonts, and
the `windows` profile can only describe a GPU it does not have. A **node**
is another machine that runs sessions' Chrome for the server — built for a
Mac, where Chrome is simply a Mac's: its real GPU (Metal, through ANGLE,
headless included), its own fonts and hardware numbers. `session_start
device=mac` places the session on a node whose OS is macOS; nothing else
about the session changes.

```
server ── mutual TLS ──▶ tailscale serve --tcp 9310 ──▶ chromemcp node (127.0.0.1:9310, as a dedicated user)
  │  relay 127.0.0.1:N ◀── chromedp, emulator, cookies,          │  Chrome --headless=new, DevTools on 9300–9309
  │                         chrome-devtools-mcp                   └─ sessions/s-…/{profile,downloads,files}
  └─ sessions/s-…/{session.json, cookies.json, files/}
```

- **The server keeps the session; the node runs the Chrome.** Metadata, the
  cookie jar, put files, the device profile, parking and the reaper stay on
  the server. The node launches and stops Chrome for a session id, relays its
  DevTools endpoint, and holds the profile and whatever Chrome downloads.
- **The DevTools endpoint comes back to loopback.** Each running remote
  session gets a relay on `127.0.0.1` that forwards everything, websockets
  included, to the node, and sends its own address as the `Host` — Chrome
  builds the URLs it advertises from that header — so chromedp, the device
  emulator and chrome-devtools-mcp connect exactly as they do to a local
  Chrome. A put file is copied to the node when a page is about to be given
  it (`DOM.setFileInputFiles` takes a path on Chrome's machine); downloads
  are listed and deleted through the node. A watcher long-polls the node, so
  a Chrome that dies there is noticed here and relaunched on next use.
- **Mutual TLS only.** Both ends present a certificate from the same CA;
  the node admits only the client names given with `-allow-client`, since
  everything that CA signed would otherwise do. Certificates are read again
  whenever their files change, so a short-lived one renewed in place (step-ca
  and the like) needs no restart. The node binds loopback by default; the
  intended way in is `tailscale serve --tcp`, which forwards raw TCP and so
  keeps the TLS end to end.
- **What a server may ask for is bounded.** The node builds Chrome's command
  line itself and accepts only the user agent, Blink settings and language
  flags, and only `TZ`/`LANG`/`LANGUAGE` in the environment — nothing that
  runs a command as its account (`--renderer-cmd-prefix`), moves the profile,
  opens the DevTools port wider or changes how Chrome is loaded (`DYLD_*`).
- **On macOS** it launches Chrome with `--use-mock-keychain` (never the
  account's keychain, and no dialog nobody can answer), keeps the GPU on in
  headless mode, turns off Chrome's own DNS client by default (`-disable-features
  AsyncDns`, so names resolve through mDNSResponder rather than by this
  account's packets to the LAN resolver), and sets the session's language
  through Chrome's `AppleLanguages` preference just before each launch —
  Chrome on macOS ignores `--lang` and the environment for it. `TZ` works as
  on Linux.
- **Headful on a Mac is a real window nobody sees.** The node's account has
  no login session, yet a non-headless Chrome launched by it renders all the
  same — the Metal GPU, screenshots, a user agent with no `HeadlessChrome` in
  it natively — in a window WindowServer never shows. The node turns off the
  throttling Chrome applies to occluded windows, which that window would
  otherwise count as. A Linux node has no display and stays headless-only.
- **The live view of a node session is a screencast** (`castview.go`), since
  there is no framebuffer for VNC: `Page.startScreencast` frames to a small
  page under the same token as the noVNC view (`/view/<tok>/cast`), and the
  viewer's mouse, wheel, keys and paste go back as `Input.dispatch*`. It
  follows tabs as they open (a sign-in popup), lists them to switch between,
  and brings the one it shows to the front. It cannot reach what the page
  never sees: a native dialog, a passkey on the machine's own authenticator.
  Any session without a display gets one, headless sessions here included.
- **Identities cross over as profile archives.** `session_start identity=`
  on a node sends the identity's profile there (a gzipped tar, minus the
  caches `copyProfile` skips; unpacked only as plain files beneath the
  profile); `identity_save` parks the session, fetches the profile back and
  saves it with the cookie jar the server already holds. An identity records
  the device it was saved from, and a session on another device is told
  what does not come across: the saved passwords, encrypted per machine,
  and the site's sense of which device it has seen before.

Run it as a dedicated standard account that owns nothing else, from launchd —
not from an SSH session, which on macOS can hand its processes the Full Disk
Access that Remote Login holds:

```xml
<!-- /Library/LaunchDaemons/com.example.chromemcp-node.plist -->
<plist version="1.0"><dict>
  <key>Label</key><string>com.example.chromemcp-node</string>
  <key>UserName</key><string>chromenode</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/chromemcp</string><string>node</string>
    <string>-tls-cert</string><string>/usr/local/etc/chromemcp-node/node.crt</string>
    <string>-tls-key</string><string>/usr/local/etc/chromemcp-node/node.key</string>
    <string>-client-ca</string><string>/usr/local/etc/chromemcp-node/ca.crt</string>
    <string>-allow-client</string><string>chromemcp.example.com</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/Users/chromenode/Library/Logs/chromemcp-node.log</string>
</dict></plist>
```

and firewall that account to the public internet: a browser loads arbitrary
pages, and on the same machine as other services a page is local code. With
pf, `user` rules drop its TCP and UDP to loopback, the private, link-local,
CGNAT (tailnet) and multicast ranges, and the LAN by interface
(`(en0:network)`: a LAN's IPv6 prefix is usually a global one). Two things
to get right: a `pass out` for loopback 9300–9309 so it can reach its own
Chrome, and a `pass in … keep state` for loopback 9300–9310, because `user`
matches the *sending* socket and the replies from its own listeners would
otherwise meet the block. `chromemcp node -h` lists the rest: `-ports`,
`-max-running` (default 1), `-sessions-dir` (`~/Library/Caches/chromemcp-node`).

The binary comes from the repository's releases: every `v*` tag attaches
`chromemcp-darwin-arm64` (and darwin/amd64, linux/amd64, linux/arm64) with a
`SHA256SUMS` to pin against (`.github/workflows/release.yml`).

Then on the server:

```bash
chromemcp serve ... -node mac=https://mac.example.ts.net:9310 \
  -node-cert client.crt -node-key client.key -node-ca ca.crt
```

`mac` is offered in `session_start`'s device list whenever a node is
configured, and placed on the first node that reports macOS; a node's own
`-max-running` is its limit, and the server parks the least recently used of
that node's sessions to make room.

## Tests

`go test ./...` runs the OAuth stack against a fake authorization server,
the identity store and profile copy, the snapshot rendering, key and flag
parsing, the device profiles, the view handler, and the tool registry over
an in-memory transport. With a Chrome on PATH it also runs
`TestBrowserIntegration`: a real headless session against a local test page
— navigate, snapshot, type, submit, select, click through a label,
scroll-into-view, screenshots, park and resume with cookies and tabs,
identity save and seed, and the `-max-running` eviction — and
`TestDeviceIntegration`: the `windows` profile with a timezone and locale
against a server that records request headers and asks for the high-entropy
client hints, checked on the page, in a worker, in a service worker, in a
popup (where the platform hint on the popup's own first navigation is checked,
the request-interception path), after a park and resume, and (with an Xvnc on
PATH) headful, including the framed outer-window size and that the `fantasy`
generic measures narrower than `system-ui`; and the `linux` profile's brand
list against the algorithm; and `TestUploadIntegration` / `TestUploadTools`:
the three shapes a site asks for a file in — a plain input, a hidden one
behind its styled label, and a button that builds its input only when
clicked — checked by what the page's own `change` handler reports, the
second of the two over the MCP wire from `file_put` to `file_delete`; and
`TestDropPageInChrome`, which opens the upload link's own page in a session
and chooses a file through it, because that page's JavaScript only ever runs
in a browser; and `TestNodeSessionIntegration`, a node in the test process
over mutual TLS on loopback with this machine's Chrome, driven through the
manager: placement, the device profile through the relay, a put file copied
over and given to a page, a download listed and deleted through the node,
park and resume with the cookie carried back, a Chrome killed on the node
noticed and relaunched, and deletion reaching the node. `TestNodeMutualTLS`
and `TestCertReload` need no Chrome: an unlisted certificate, one from
another CA and none at all are refused, the launch request is bounded, and a
renewed certificate is served without a restart. They
skip themselves where there is no Chrome, as in the image's build stage;
`CHROMEMCP_TEST_NO_CHROME=1` skips them anywhere.

## Licence

[MIT](LICENSE).
