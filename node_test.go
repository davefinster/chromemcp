package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority for the mutual-TLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

func newTestCA(t *testing.T, dir, name string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &testCA{cert: cert, key: key, dir: dir}
	writePEM(t, filepath.Join(dir, name+".pem"), "CERTIFICATE", der)
	return ca
}

func (ca *testCA) file(name string) string { return filepath.Join(ca.dir, name) }

// issue writes name.crt / name.key: a leaf for the CN, which is also its
// DNS name, valid for 127.0.0.1 as well so a test server can present it.
func (ca *testCA) issue(t *testing.T, name string, serial int64) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = ca.file(name+".crt"), ca.file(name+".key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", kb)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testNode serves a node over mutual TLS on loopback and returns it with a
// client that holds the admitted certificate.
func testNode(t *testing.T, cfg *nodeConfig) (*nodeServer, *nodeClient, *testCA) {
	t.Helper()
	dir := t.TempDir()
	ca := newTestCA(t, dir, "ca")
	srvCert, srvKey := ca.issue(t, "node.test", 2)
	cliCert, cliKey := ca.issue(t, "chromemcp", 3)
	ns, err := newNodeServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stls, err := serverTLS(srvCert, srvKey, ca.file("ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// Served as the node serves itself: httptest's StartTLS would add its own
	// certificate, which a client that sends no SNI (an IP address) is given
	// in place of the one GetCertificate would have chosen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: ns.handler(), TLSConfig: stls}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	t.Cleanup(ns.shutdown)
	ctls, err := clientTLS(cliCert, cliKey, ca.file("ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := newNodeClient("n1", "https://"+ln.Addr().String(), ctls)
	if err != nil {
		t.Fatal(err)
	}
	return ns, n, ca
}

func TestNodeMutualTLS(t *testing.T) {
	_, n, ca := testNode(t, &nodeConfig{SessionsDir: t.TempDir(), Chrome: "/nonexistent", PortLow: 9300, PortHigh: 9309,
		MaxRunning: 2, AllowClients: []string{"chromemcp"}})
	ctx := context.Background()

	info, err := n.getInfo(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.OS != hostOS || info.MaxRunning != 2 || info.Version != version || len(info.Running) != 0 {
		t.Errorf("info: %+v", info)
	}

	// A certificate from the same CA that is not on the node's list.
	intCert, intKey := ca.issue(t, "intruder", 4)
	itls, _ := clientTLS(intCert, intKey, ca.file("ca.pem"))
	intruder, _ := newNodeClient("n1", n.base.String(), itls)
	if _, err := intruder.getInfo(ctx, 0); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Errorf("an unlisted certificate: %v", err)
	}
	// One from another CA does not get through the handshake at all.
	other := newTestCA(t, t.TempDir(), "other")
	oCert, oKey := other.issue(t, "chromemcp", 5)
	otls, _ := clientTLS(oCert, oKey, ca.file("ca.pem"))
	stranger, _ := newNodeClient("n1", n.base.String(), otls)
	if _, err := stranger.getInfo(ctx, 0); err == nil {
		t.Error("a certificate from another CA was accepted")
	}
	// Nor does a client with no certificate.
	pool, _ := loadCAPool(ca.file("ca.pem"))
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if resp, err := bare.Get(n.url("/v1/info")); err == nil {
		resp.Body.Close()
		t.Error("a client without a certificate was served")
	}

	// What a server may ask a node to launch is bounded.
	for _, tc := range []struct {
		req  nodeLaunchRequest
		want string
	}{
		{nodeLaunchRequest{Headless: false, Width: 800, Height: 600}, "headless"},
		{nodeLaunchRequest{Headless: true, Width: 800, Height: 600, Flags: []string{"--renderer-cmd-prefix=/bin/sh"}}, "not one a node passes"},
		{nodeLaunchRequest{Headless: true, Width: 800, Height: 600, Flags: []string{"--remote-debugging-address=0.0.0.0"}}, "not one a node passes"},
		{nodeLaunchRequest{Headless: true, Width: 800, Height: 600, Env: []string{"DYLD_INSERT_LIBRARIES=/tmp/x.dylib"}}, "not one a node passes"},
		{nodeLaunchRequest{Headless: true, Width: 50, Height: 600}, "window"},
	} {
		if _, err := n.launch(ctx, "s-00000001", &tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("launch %+v: %v, want %q", tc.req, err, tc.want)
		}
	}
	if err := n.do(ctx, http.MethodGet, "/v1/sessions/nope/wait", nil, nil); err == nil || !strings.Contains(err.Error(), "session id") {
		t.Errorf("a malformed session id: %v", err)
	}
	// A session with no Chrome is simply not alive, and has no endpoint.
	if alive, err := n.wait(ctx, "s-00000001", time.Second); err != nil || alive {
		t.Errorf("wait on nothing: %v %v", alive, err)
	}
	if err := n.do(ctx, http.MethodGet, "/v1/sessions/s-00000001/cdp/json/version", nil, nil); err == nil || !strings.Contains(err.Error(), "no running Chrome") {
		t.Errorf("cdp with no Chrome: %v", err)
	}

	// Files: stored under the session, names held to the same rule as put
	// files, and deleted.
	p, err := n.putFile(ctx, "s-00000001", "notes.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "hello" || filepath.Base(filepath.Dir(p)) != "files" {
		t.Errorf("put file at %s: %q %v", p, b, err)
	}
	if _, err := n.putFile(ctx, "s-00000001", "../escape", strings.NewReader("x")); err == nil {
		t.Error("a name with a separator was accepted")
	}
	if err := n.deleteFile(ctx, "s-00000001", "notes.txt"); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("deleted file still there: %v", err)
	}
}

// A renewed certificate is served without a restart.
func TestCertReload(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, dir, "ca")
	certFile, keyFile := ca.issue(t, "node.test", 10)
	cf, err := newCertFiles(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := cf.get()
	serial := func(c *tls.Certificate) int64 {
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		return leaf.SerialNumber.Int64()
	}
	ca.issue(t, "node.test", 11) // same files, new certificate
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(certFile, later, later)
	os.Chtimes(keyFile, later, later)
	time.Sleep(1100 * time.Millisecond)
	second, _ := cf.get()
	if serial(first) != 10 || serial(second) != 11 {
		t.Errorf("serials %d then %d, want 10 then 11", serial(first), serial(second))
	}
	// Half a renewal (a key that does not match) keeps the last good pair.
	os.WriteFile(keyFile, []byte("garbage"), 0o600)
	os.Chtimes(keyFile, later.Add(time.Second), later.Add(time.Second))
	time.Sleep(1100 * time.Millisecond)
	if c, err := cf.get(); err != nil || serial(c) != 11 {
		t.Errorf("mid-renewal: %v", err)
	}
}

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string][2]int{"9300-9309": {9300, 9309}, "9310": {9310, 9310}} {
		lo, hi, err := parsePortRange(in)
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("%s: %d-%d %v", in, lo, hi, err)
		}
	}
	for _, bad := range []string{"80-90", "9309-9300", "x", "9300-70000"} {
		if _, _, err := parsePortRange(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if platformVersion("27.0") != "27.0.0" || platformVersion("14.8.9") != "14.8.9" || platformVersion("27") != "27.0.0" {
		t.Error("platformVersion")
	}
	if _, _, err := parseNodeFlag("mac=https://x:9310"); err != nil {
		t.Error(err)
	}
	if _, _, err := parseNodeFlag("https://x:9310"); err == nil {
		t.Error("a node without a name was accepted")
	}
}

// The device list offers a real machine's profile only where it can run.
func TestNodeDevices(t *testing.T) {
	cfg := &managerConfig{SessionsDir: filepath.Join(t.TempDir(), "s"), IdentitiesDir: filepath.Join(t.TempDir(), "i"), Chrome: "/x"}
	mgr, err := newManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	has := func(names []string, n string) bool {
		return strings.Contains(","+strings.Join(names, ",")+",", ","+n+",")
	}
	if hostOS != "darwin" && has(mgr.deviceNames(), "mac") {
		t.Errorf("mac offered with no node: %v", mgr.deviceNames())
	}
	if !has(deviceNames(), "mac") || !has(mgr.deviceNames(), "windows") {
		t.Errorf("registry %v, offered %v", deviceNames(), mgr.deviceNames())
	}
	if hostOS != "darwin" {
		mac, _ := lookupDevice("mac")
		if _, err := mgr.placeDevice(context.Background(), mac); err == nil || !strings.Contains(err.Error(), "has none") {
			t.Errorf("placing mac with no node: %v", err)
		}
	}
	cfg.Nodes = []*nodeClient{{name: "n1"}}
	if !has(mgr.deviceNames(), "mac") {
		t.Errorf("mac not offered with a node: %v", mgr.deviceNames())
	}
	// The mac profile leaves the real GPU's strings alone, and puts the menu
	// bar at the top of the screen.
	mac, _ := lookupDevice("mac")
	js := mac.initScript(chromeVersion{Full: "154.0.8037.57", Major: "154"})
	if !strings.Contains(js, `"vendor":""`) || !strings.Contains(js, `"menubar":25`) || !strings.Contains(js, "(P.vendor || P.renderer)") {
		t.Errorf("mac init script: %s", js)
	}
	if ua := mac.userAgentOverride(chromeVersion{Full: "154.0.8037.57", Major: "154"}); ua.Platform != "MacIntel" || ua.UserAgentMetadata.Platform != "macOS" ||
		strings.Contains(ua.UserAgent, "Headless") || !strings.Contains(ua.UserAgent, "Macintosh") {
		t.Errorf("mac UA: %+v", ua)
	}
}

// TestNodeSessionIntegration runs a session's Chrome on a node -- in this
// process, over mutual TLS on loopback, with this machine's Chrome -- and
// drives it through the manager as any session is driven.
func TestNodeSessionIntegration(t *testing.T) {
	exe := defaultChrome()
	if exe == "" || os.Getenv("CHROMEMCP_TEST_NO_CHROME") != "" {
		t.Skip("no Chrome on PATH")
	}
	if os.Getenv("CHROMEMCP_NO_SANDBOX") == "1" {
		t.Skip("a node always runs Chrome sandboxed")
	}
	// One DevTools port, found free: the node's range.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	nodeDir := t.TempDir()
	ns, n, _ := testNode(t, &nodeConfig{SessionsDir: nodeDir, Chrome: exe, PortLow: port, PortHigh: port, MaxRunning: 1})

	// A real-machine profile for this OS, emulating Windows on top, so the
	// device emulation is exercised through the relay too.
	win, _ := lookupDevice("windows")
	testDev := *win
	testDev.Name, testDev.OS, testDev.FontsPlan, testDev.FontFamilies = "testnode", hostOS, nil, nil
	deviceProfiles["testnode"] = &testDev
	t.Cleanup(func() { delete(deviceProfiles, "testnode") })

	mgr, err := newManager(&managerConfig{
		SessionsDir: filepath.Join(t.TempDir(), "sessions"), IdentitiesDir: filepath.Join(t.TempDir(), "identities"),
		Chrome: exe, ViewportW: 900, ViewportH: 600, MaxRunning: 2, Nodes: []*nodeClient{n},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.shutdown)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/report.txt":
			w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
			w.Write([]byte("downloaded bytes"))
		default:
			w.Write([]byte(`<!doctype html><title>Node Page</title>
<input type="file" id="f" onchange="document.getElementById('out').textContent = this.files[0].name + '@' + this.files[0].size">
<a id="dl" href="/report.txt">report</a><p id="out"></p>
<script>document.cookie = 'nc=1; max-age=3600; path=/';</script>`))
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Refused before anything launches: what a node cannot do.
	if _, err := mgr.start(ctx, startOptions{Device: "testnode", Mode: modeHeadful}); err == nil || !strings.Contains(err.Error(), "headless only") {
		t.Errorf("headful on a node: %v", err)
	}

	s, err := mgr.start(ctx, startOptions{Device: "testnode", Label: "node"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := s.meta.ID
	if s.meta.Node != "n1" {
		t.Fatalf("session placed on %q", s.meta.Node)
	}
	rc, ok := s.chrome.(*remoteChrome)
	if !ok {
		t.Fatalf("session's Chrome is a %T", s.chrome)
	}
	if !strings.HasPrefix(rc.wsURL(), fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/", rc.debugPort())) {
		t.Errorf("ws URL %s does not go through the relay", rc.wsURL())
	}
	// What Chrome advertises is the relay too, so chrome-devtools-mcp's
	// browserUrl lookup lands back here.
	if ws := devtoolsBrowserURL(rc.debugPort()); !strings.HasPrefix(ws, fmt.Sprintf("ws://127.0.0.1:%d/", rc.debugPort())) {
		t.Errorf("advertised through the relay: %q", ws)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, sid, "profile")); err != nil {
		t.Errorf("no profile on the node: %v", err)
	}
	if _, err := os.Stat(s.profileDir()); err == nil {
		t.Error("a node session has a profile on the server")
	}

	if _, err := s.putFile("rows.csv", []byte("a,b\n1,2\n"), putCreate); err != nil {
		t.Fatal(err)
	}
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		if err := navigate(ctx, tb, srv.URL, 20*time.Second); err != nil {
			return err
		}
		var platform string
		if err := evalJSON(ctx, tb, "navigator.platform", &platform); err != nil {
			return err
		}
		if platform != "Win32" {
			t.Errorf("navigator.platform %q: the device profile did not reach the node's Chrome", platform)
		}
		// A put file is copied to the node and given to the page from there.
		paths, err := s.filePaths([]string{"rows.csv"})
		if err != nil {
			return err
		}
		if len(paths) != 1 || !strings.HasPrefix(paths[0], nodeDir) {
			t.Errorf("upload path %v is not the node's", paths)
		}
		if _, err := findFileTarget(ctx, tb, targetSpec{Selector: "#f"}); err != nil {
			return err
		}
		if err := setFilesOnElement(ctx, tb, paths); err != nil {
			return err
		}
		var out string
		evalJSON(ctx, tb, "document.getElementById('out').textContent", &out)
		if out != "rows.csv@8" {
			t.Errorf("the page got %q", out)
		}
		// A download lands on the node and is listed through it.
		res, err := resolveTarget(ctx, tb, targetSpec{Selector: "#dl"})
		if err != nil {
			return err
		}
		return clickAt(ctx, tb, res.X, res.Y, "left", 1, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []sessionFile
	for i := 0; i < 50; i++ {
		got, _ = s.listDownloads()
		if len(got) == 1 && !got[0].Partial {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(got) != 1 || got[0].Name != "report.txt" || got[0].Size != 16 || !strings.HasPrefix(got[0].path, nodeDir) {
		t.Fatalf("downloads: %+v", got)
	}
	if err := s.deleteFile("report.txt"); err != nil {
		t.Error(err)
	}
	if got, _ := s.listDownloads(); len(got) != 0 {
		t.Errorf("after deleting the download: %+v", got)
	}

	// Park and resume: the cookie comes back, carried by the server.
	s.mu.Lock()
	s.park()
	s.mu.Unlock()
	if len(ns.sessions) != 1 || ns.running(sid) != nil {
		t.Error("the node's Chrome is still running after a park")
	}
	err = mgr.withTab(ctx, sid, "", func(ctx context.Context, s *session, tb *tab) error {
		var cookie string
		if err := evalJSON(ctx, tb, "document.cookie", &cookie); err != nil {
			return err
		}
		if !strings.Contains(cookie, "nc=1") {
			t.Errorf("cookie after resume: %q", cookie)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A Chrome that dies on the node is noticed here.
	ns.mu.Lock()
	pid := ns.sessions[sid].proc.PID()
	ns.mu.Unlock()
	syscall.Kill(-pid, syscall.SIGKILL)
	deadline := time.Now().Add(15 * time.Second)
	for s.isRunningQuick() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if s.isRunningQuick() {
		t.Error("the node's Chrome was killed and the session still says running")
	}
	// ... and the next use starts it again.
	if _, err := mgr.running(ctx, sid); err != nil {
		t.Fatalf("relaunch after a crash: %v", err)
	}

	if err := mgr.remove(sid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, sid)); !os.IsNotExist(err) {
		t.Errorf("session directory left on the node: %v", err)
	}
}
