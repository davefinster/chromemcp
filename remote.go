package main

// Sessions whose Chrome runs on a node (node.go). The server keeps the
// session -- its metadata, cookie jar, put files, device profile and every
// tool -- and the node runs the Chrome. Nothing that drives a session knows
// the difference, because the node's DevTools endpoint is brought back to
// a loopback port here: a relay per running session forwards whatever
// arrives on it, websockets included, to the node over mutual TLS, carrying
// its own Host header along so that the URLs Chrome advertises point at the
// relay too. chromedp, the device emulator, the cookie export and
// chrome-devtools-mcp all connect to 127.0.0.1 as they do for a local
// Chrome.
//
// What does differ is the filesystem. Chrome opens the files a page is given
// by path, on its own machine, so a put file is copied to the node as the
// page is about to be given it (filePaths), and the downloads are the
// node's, listed and deleted through it.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var regexpNodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// nodeClient is this server's connection to one node.
type nodeClient struct {
	name      string
	base      *url.URL
	transport *http.Transport

	mu     sync.Mutex
	info   *nodeInfo
	infoAt time.Time
}

// parseNodeFlag reads NAME=URL.
func parseNodeFlag(v string) (name, rawURL string, err error) {
	name, rawURL, ok := strings.Cut(v, "=")
	if !ok || !regexpNodeName.MatchString(name) {
		return "", "", fmt.Errorf("node %q: want NAME=https://host:port, NAME a short lower-case word", v)
	}
	return name, rawURL, nil
}

func newNodeClient(name, rawURL string, tlsCfg *tls.Config) (*nodeClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("node %s: %q is not an https:// URL", name, rawURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &nodeClient{
		name: name,
		base: u,
		transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

func (n *nodeClient) url(path string) string { return n.base.String() + path }

// do sends one API request and decodes the JSON answer into out (when not
// nil). A node's errors come back as {"error": "..."}.
func (n *nodeClient) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, n.url(path), body)
	if err != nil {
		return err
	}
	if body != nil && method != http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: n.transport}).Do(req)
	if err != nil {
		return fmt.Errorf("node %s: %w", n.name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("node %s: %w", n.name, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("node %s: %s", n.name, e.Error)
		}
		return fmt.Errorf("node %s: %s %s: %s", n.name, method, path, resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("node %s: %s: %w", n.name, path, err)
		}
	}
	return nil
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// getInfo is the node's /v1/info, from a cache no older than maxAge.
func (n *nodeClient) getInfo(ctx context.Context, maxAge time.Duration) (*nodeInfo, error) {
	n.mu.Lock()
	if n.info != nil && time.Since(n.infoAt) < maxAge {
		info := n.info
		n.mu.Unlock()
		return info, nil
	}
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var info nodeInfo
	if err := n.do(ctx, http.MethodGet, "/v1/info", nil, &info); err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.info, n.infoAt = &info, time.Now()
	n.mu.Unlock()
	return &info, nil
}

// maxRunning is how many sessions the node will run at once, as it last
// said; 1 if it cannot be asked.
func (n *nodeClient) maxRunning() int {
	info, err := n.getInfo(context.Background(), 10*time.Minute)
	if err != nil || info.MaxRunning < 1 {
		return 1
	}
	return info.MaxRunning
}

func sessionPath(id string) string { return "/v1/sessions/" + url.PathEscape(id) }

// launch starts the session's Chrome on the node and brings its DevTools
// endpoint back here.
func (n *nodeClient) launch(ctx context.Context, id string, req *nodeLaunchRequest) (*remoteChrome, error) {
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var resp nodeLaunchResponse
	if err := n.do(lctx, http.MethodPost, sessionPath(id)+"/launch", jsonBody(req), &resp); err != nil {
		return nil, err
	}
	relay, err := startRelay(n, id)
	if err != nil {
		n.stopSession(context.Background(), id, 2*time.Second)
		return nil, err
	}
	wctx, stopWatch := context.WithCancel(context.Background())
	c := &remoteChrome{
		node: n, id: id, relay: relay, browserPath: resp.BrowserPath, downloads: resp.Downloads,
		done: make(chan struct{}), stopWatch: stopWatch,
	}
	go c.watch(wctx)
	return c, nil
}

func (n *nodeClient) stopSession(ctx context.Context, id string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, grace+30*time.Second)
	defer cancel()
	return n.do(ctx, http.MethodPost, fmt.Sprintf("%s/stop?grace_ms=%d", sessionPath(id), grace.Milliseconds()), nil, nil)
}

func (n *nodeClient) remove(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return n.do(ctx, http.MethodDelete, sessionPath(id), nil, nil)
}

// wait blocks on the node until the session's Chrome exits or timeout
// passes, and reports whether it is still alive.
func (n *nodeClient) wait(ctx context.Context, id string, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	var out struct {
		Alive bool `json:"alive"`
	}
	err := n.do(ctx, http.MethodGet, fmt.Sprintf("%s/wait?timeout=%s", sessionPath(id), timeout), nil, &out)
	return out.Alive, err
}

func (n *nodeClient) listDownloads(ctx context.Context, id string) ([]sessionFile, error) {
	var files []nodeFile
	if err := n.do(ctx, http.MethodGet, sessionPath(id)+"/downloads", nil, &files); err != nil {
		return nil, err
	}
	out := make([]sessionFile, 0, len(files))
	for _, f := range files {
		out = append(out, sessionFile{Name: f.Name, Size: f.Size, Modified: f.Modified, MIME: fileMIME(f.Name),
			Downloaded: true, Partial: f.Partial, path: f.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (n *nodeClient) deleteDownload(ctx context.Context, id, name string) error {
	return n.do(ctx, http.MethodDelete, sessionPath(id)+"/downloads/"+url.PathEscape(name), nil, nil)
}

func (n *nodeClient) deleteFile(ctx context.Context, id, name string) error {
	return n.do(ctx, http.MethodDelete, sessionPath(id)+"/files/"+url.PathEscape(name), nil, nil)
}

// putFile copies a file to the node and returns its path there.
func (n *nodeClient) putFile(ctx context.Context, id, name string, body io.Reader) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	if err := n.do(ctx, http.MethodPut, sessionPath(id)+"/files/"+url.PathEscape(name), body, &out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// ---- the relay ----

// cdpRelay is a loopback HTTP server that forwards everything to one
// session's DevTools endpoint on its node.
type cdpRelay struct {
	ln   net.Listener
	srv  *http.Server
	port int
}

func startRelay(n *nodeClient, id string) (*cdpRelay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	prefix := n.base.Path + sessionPath(id) + "/cdp"
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = n.base.Scheme
			pr.Out.URL.Host = n.base.Host
			pr.Out.URL.Path = prefix + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			// The relay's own address: Chrome advertises its endpoints under
			// whatever Host it is asked with (node.go's cdp).
			pr.Out.Host = pr.In.Host
		},
		Transport: n.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("node %s: %v", n.name, err), http.StatusBadGateway)
		},
	}
	r := &cdpRelay{ln: ln, port: ln.Addr().(*net.TCPAddr).Port,
		srv: &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}}
	go r.srv.Serve(ln)
	return r, nil
}

func (r *cdpRelay) close() { r.srv.Close() }

// ---- a Chrome on a node ----

// remoteChrome is a session's Chrome running on a node: the chromeHandle
// of a remote session.
type remoteChrome struct {
	node        *nodeClient
	id          string
	relay       *cdpRelay
	browserPath string
	downloads   string

	done      chan struct{}
	once      sync.Once
	stopWatch context.CancelFunc
}

func (c *remoteChrome) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *remoteChrome) wsURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d%s", c.relay.port, c.browserPath)
}
func (c *remoteChrome) debugPort() int       { return c.relay.port }
func (c *remoteChrome) downloadsDir() string { return c.downloads }

func (c *remoteChrome) stop(grace time.Duration) {
	if c.Alive() {
		if err := c.node.stopSession(context.Background(), c.id, grace); err != nil {
			logf("session %s: stopping its Chrome on node %s: %v", c.id, c.node.name, err)
		}
	}
	c.finish()
}

func (c *remoteChrome) finish() {
	c.once.Do(func() {
		c.stopWatch()
		c.relay.close()
		close(c.done)
	})
}

// watch follows the remote Chrome's life with the node's wait, so a crash
// there is noticed here as it happens -- as the exit of a local Chrome is.
// A node that stops answering is taken to have lost it after a minute or so
// of trying; the next launch then starts it over from its profile.
func (c *remoteChrome) watch(ctx context.Context) {
	failures := 0
	for {
		alive, err := c.node.wait(ctx, c.id, 50*time.Second)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			if failures >= 5 {
				logf("session %s: node %s unreachable (%v); taking its Chrome as gone", c.id, c.node.name, err)
				c.finish()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(failures*failures) * 2 * time.Second):
			}
			continue
		}
		failures = 0
		if !alive {
			logf("session %s: its Chrome on node %s exited", c.id, c.node.name)
			c.finish()
			return
		}
	}
}

// ---- placing sessions ----

// node is the configured node of that name, or nil.
func (m *manager) node(name string) *nodeClient {
	for _, n := range m.cfg.Nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

// placeDevice decides where a new session with this device profile runs:
// nil for here, or the node that has the operating system the profile is
// a real instance of.
func (m *manager) placeDevice(ctx context.Context, dev *deviceProfile) (*nodeClient, error) {
	if dev.OS == "" {
		return nil, nil
	}
	var errs []string
	for _, n := range m.cfg.Nodes {
		info, err := n.getInfo(ctx, 30*time.Second)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if info.OS == dev.OS {
			return n, nil
		}
	}
	if dev.OS == hostOS {
		return nil, nil
	}
	msg := fmt.Sprintf("device %s runs on a node with %s, and this server has none", dev.Name, dev.OS)
	if len(errs) > 0 {
		msg = fmt.Sprintf("device %s runs on a node with %s, and none answered: %s", dev.Name, dev.OS, strings.Join(errs, "; "))
	}
	return nil, errors.New(msg)
}

// remote is the node the session runs on, or nil for a local one.
func (s *session) remote() (*nodeClient, error) {
	if s.meta.Node == "" {
		return nil, nil
	}
	n := s.mgr.node(s.meta.Node)
	if n == nil {
		return nil, fmt.Errorf("session %s runs on node %q, which this server is not configured with", s.meta.ID, s.meta.Node)
	}
	return n, nil
}

// platformVersion is an OS version as Chrome reports it in
// Sec-CH-UA-Platform-Version: three components, "27.0" as "27.0.0".
func platformVersion(v string) string {
	parts := strings.Split(strings.TrimSpace(v), ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	return strings.Join(parts[:3], ".")
}

// pushFile copies one of the session's put files to its node, for a page
// to be given it, and returns its path there.
func (s *session) pushFile(n *nodeClient, f sessionFile) (string, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return n.putFile(ctx, s.meta.ID, f.Name, fh)
}
