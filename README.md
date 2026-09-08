# chromemcp

A [Model Context Protocol](https://modelcontextprotocol.io) server that gives
an agent Chrome browsers of its own: it launches Chrome instances — headless,
or headful on a private VNC display — drives them, and hands back what the
page looks like after every action. It is OAuth-protected the way the sibling
dmf.zone MCP servers (`m2mcp`, `barrys`, `trackmcp`, `hivemcp`) are, and can
pass each session through to Google's
[chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp)
for the low-level work it does not reimplement.

One Go binary, four parts:

| part | what |
|---|---|
| MCP server | Streamable HTTP (or stdio), OAuth-protected: AuthKit issuer, resource identifier, single-account email pin. 30 tools. |
| sessions | One Chrome per session on its own profile directory, like the first launch on a clean workstation. Ephemeral: parked when idle (Chrome closed, profile kept, resumable), deleted when old. |
| identities | Named snapshots of a profile the owner has logged into — the persistent part. A session started *as* an identity begins already signed in. |
| live view | Headful sessions render on an Xvnc display; a token link serves noVNC over the server's own websocket bridge, so a person can watch, take over, and sign in where no agent can (passkeys, 2FA). |

## Tools

**Sessions**

| tool | purpose |
|---|---|
| `session_start` | a new Chrome on a fresh profile, or on a copy of an identity's; `mode` headless (default) or headful; `device` profile (`default` or `windows`), `timezone`, `locale`; `viewport`, `label`, `url` |
| `session_list` | running and parked sessions (what can be resumed), plus the saved identities |
| `session_resume` / `session_stop` | relaunch a parked session with its cookies, storage and tabs; park a running one |
| `session_delete` | close and erase a session |
| `session_view` | a live-view link for a headful session, for a person to open |

**Identities**

| tool | purpose |
|---|---|
| `identity_list` | the saved identities |
| `identity_save` | snapshot a session's profile and cookie jar under a name (Chrome is closed cleanly around the copy and started again) |
| `identity_delete` | remove one |

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
                               ├─ /healthz
                               └─ sessions/
                                   ├─ s-…/profile/   Chrome --user-data-dir, --headless=new
                                   ├─ s-…/cookies.json
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
- **Dialogs are accepted** (alert/confirm/prompt/beforeunload) so a page never
  blocks on one nobody can see; the message is reported with the next result.

## Device profiles

A session's Chrome is this server's Chrome: Linux, a container's screen, a
software GPU, and in headless mode a user agent that says `HeadlessChrome`.
For testing how a site behaves for a particular kind of user, `session_start`
takes a `device` profile that changes what the browser says about the
machine it runs on — never the browser itself, since a site can compare the
two and Chrome's version, features and TLS fingerprint stay its own.

| profile | what sites see |
|---|---|
| `default` | this Chrome as it is |
| `windows` | a Windows 11 PC running the same Chrome version: `Mozilla/5.0 (Windows NT 10.0; Win64; x64) … Chrome/N.0.0.0 …`; `Sec-CH-UA-Platform: "Windows"` with the high-entropy hints of a 64-bit x86 desktop on 24H2 (`platformVersion` 19.0.0); `navigator.platform` `Win32`; `navigator.userAgentData` to match; a 1920×1080 display at 100 % scale; an NVIDIA GeForce RTX 3060 under ANGLE/D3D11 in `WEBGL_debug_renderer_info`; `navigator.webdriver` false |

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
  Chrome's own. Only hints already present are rewritten, never added, so a
  request sends exactly what Chrome chose to, with the platform corrected;
  every request is continued whatever happens, and Chrome drops the
  interception by itself if the client goes away, so a page never hangs on it.
- A script evaluated in every new document (and in every worker before its
  own script) handles what the protocol cannot, or cannot reliably:
  `navigator.platform` (Blink keeps the protocol's override in per-page
  settings that every attached session — chromedp's included — restores after
  a process swap, the sessions with nothing set included, so it does not
  survive the first navigation; and workers never get it), `navigator.webdriver`
  (true in any Chrome with remote debugging on; the flag that turns it off
  puts an "unsupported command-line flag" bar over headful windows),
  the WebGL vendor and renderer strings, `uaFullVersion` (which Chrome blanks
  when the user agent comes from the command line), and `window.outerWidth` /
  `outerHeight` when they read 0 — which a freshly opened tab does for a moment
  and a headless window always does, a "no window" bot signal — reported then
  as the inner size plus a frame. The patched functions report `[native code]`
  to `Function.prototype.toString`.
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
  OS and browser from another. The profile runs Chrome with a fontconfig
  configuration of its own (`FONTCONFIG_FILE`; `fonts.go`) that presents the
  image's fonts under Windows names — Segoe UI is Selawik, Microsoft's own
  OFL-licensed metric-compatible stand-in; Calibri and Cambria are Carlito
  and Caladea; Arial, Times New Roman and Courier New are Liberation; Verdana,
  Tahoma and the rest of the common ones are DejaVu; Impact and the condensed
  faces are Liberation Sans Narrow; the East Asian families are Noto CJK — and
  hides the image's own family names from explicit requests, so `"DejaVu Sans"`
  in a stylesheet falls through the way it does on Windows. Nothing is renamed;
  only which names find which fonts changes. Chrome accepts a substitute only
  when the substituted request's first family is the font found, so the file
  is generated against what `fc-list` says is installed, and a deployment with
  fewer fonts simply has fewer Windows families.
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

What a profile does not change: `hardwareConcurrency` and `deviceMemory`
(the container's own numbers, plausible for a PC and consistent across
page and workers, which an override would not be), the glyph shapes and
antialiasing behind the font names (Selawik is not Segoe UI, and Linux
does not render like DirectWrite), canvas and audio rendering, the media
devices and speech voices, and the WebGL capabilities behind the renderer
string (SwiftShader's limits, not a GeForce's). A determined fingerprinting
script can tell; a site deciding what to show a Windows Chrome user cannot.

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
  -public-url https://chromemcp.dmf.zone \
  -allowed-email me@dmf.zone \
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
device profiles draw on (Liberation, DejaVu, Carlito, Caladea, Noto, and
Selawik from its GitHub release, checksum pinned); runs as uid 1000 with
identities on `/data/identities` and sessions under `/tmp`:

```bash
docker run -d --name chromemcp -p 127.0.0.1:8787:8787 -v chromemcp:/data --shm-size=1g \
  ghcr.io/davefinster/chromemcp serve -http :8787 \
    -oauth-issuer https://<env>.authkit.app -public-url https://chromemcp.dmf.zone \
    -allowed-email me@dmf.zone
```

The image sets `CHROMEMCP_NO_SANDBOX=1`: Chrome's sandbox needs user
namespaces that container runtimes usually withhold. Drop it on a runtime
that grants them. `--disable-dev-shm-usage` is always on, so a small
`/dev/shm` does not crash tabs, but a real one is kinder.

A homelab deployment follows the `services/hivemcp` pattern in `infralife`:
the pod on the global-infrastructure tailnet behind the edge at
`chromemcp.dmf.zone`, `/healthz` as the backend check, a claim mounted at
`/data` for the identities (the one thing worth keeping), an emptyDir for
`/tmp`, and a memory request that allows for `-max-running` Chromes at a
few hundred MB each. The edge must pass websockets for `/view/`.

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
generic measures narrower than `system-ui`; and the default profile's brand
list against the algorithm. They skip themselves where there is no Chrome, as
in the image's build stage; `CHROMEMCP_TEST_NO_CHROME=1` skips them anywhere.
