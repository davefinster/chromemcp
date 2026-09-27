package main

// Mutual TLS between this server and its nodes (node.go, remote.go). Both
// ends present a certificate from the same private CA and check the other's
// against it; the node also checks that the client's certificate names one
// of the callers it admits, since everything that CA ever signed would
// otherwise do. Certificates from a CA like step-ca live for hours and are
// renewed in place on disk, so they are read again whenever the files change
// rather than once at startup: a long-running node or server keeps working
// across renewals without a restart.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

// certFiles is a certificate and key pair, reloaded when either file's
// modification time changes.
type certFiles struct {
	cert, key string

	mu      sync.Mutex
	loaded  *tls.Certificate
	stamp   [2]time.Time
	checked time.Time
}

func newCertFiles(cert, key string) (*certFiles, error) {
	if cert == "" || key == "" {
		return nil, errors.New("a certificate and its key are both needed")
	}
	c := &certFiles{cert: cert, key: key}
	if _, err := c.get(); err != nil {
		return nil, err
	}
	return c, nil
}

// get is the current pair. The files are looked at most once a second: a
// handshake costs two stats, not a parse.
func (c *certFiles) get() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded != nil && time.Since(c.checked) < time.Second {
		return c.loaded, nil
	}
	c.checked = time.Now()
	var stamp [2]time.Time
	for i, f := range []string{c.cert, c.key} {
		fi, err := os.Stat(f)
		if err != nil {
			if c.loaded != nil {
				return c.loaded, nil // mid-renewal; keep the last good pair
			}
			return nil, err
		}
		stamp[i] = fi.ModTime()
	}
	if c.loaded != nil && stamp == c.stamp {
		return c.loaded, nil
	}
	pair, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		if c.loaded != nil {
			// A renewal writes the two files one after the other; the pair
			// is briefly mismatched. The next look will find them agreeing.
			return c.loaded, nil
		}
		return nil, fmt.Errorf("%s / %s: %w", c.cert, c.key, err)
	}
	c.loaded, c.stamp = &pair, stamp
	return c.loaded, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: no PEM certificates", path)
	}
	return pool, nil
}

// serverTLS is a node's configuration: its own certificate, and client
// certificates required and verified against the CA.
func serverTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cf, err := newCertFiles(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cf.get() },
		// HTTP/1.1 only: the DevTools websocket is relayed as an HTTP/1.1
		// upgrade, which has no HTTP/2 equivalent in net/http.
		NextProtos: []string{"http/1.1"},
	}, nil
}

// clientTLS is this server's side: its client certificate, and the node's
// verified against the CA.
func clientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cf, err := newCertFiles(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		RootCAs:              pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cf.get() },
		NextProtos:           []string{"http/1.1"},
	}, nil
}

// certNames is every name a certificate goes by: its common name, DNS
// names, email addresses and URIs.
func certNames(c *x509.Certificate) []string {
	names := []string{c.Subject.CommonName}
	names = append(names, c.DNSNames...)
	names = append(names, c.EmailAddresses...)
	for _, u := range c.URIs {
		names = append(names, u.String())
	}
	return names
}

// peerAllowed reports whether the verified client certificate names one of
// the allowed callers. An empty list admits any certificate the CA signed.
func peerAllowed(state *tls.ConnectionState, allowed []string) (string, bool) {
	if state == nil || len(state.PeerCertificates) == 0 {
		return "", false
	}
	leaf := state.PeerCertificates[0]
	if len(allowed) == 0 {
		return leaf.Subject.CommonName, true
	}
	for _, n := range certNames(leaf) {
		if n != "" && slices.Contains(allowed, n) {
			return n, true
		}
	}
	return leaf.Subject.CommonName, false
}
