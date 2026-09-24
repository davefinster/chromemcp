package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAS is a minimal RFC 8414 authorization server on a loopback listener:
// metadata, a JWKS with one RSA key, and a helper minting signed tokens.
type fakeAS struct {
	*httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	as := &fakeAS{key: key, kid: "test-key"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                as.URL,
			"authorization_endpoint":                as.URL + "/oauth2/authorize",
			"token_endpoint":                        as.URL + "/oauth2/token",
			"jwks_uri":                              as.URL + "/oauth2/jwks",
			"registration_endpoint":                 as.URL + "/oauth2/register",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("/oauth2/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pub := &key.PublicKey
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": as.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)
	return as
}

// mint signs a JWT with the fake AS key. Overrides replace default claims;
// a nil value deletes the claim.
func (as *fakeAS) mint(t *testing.T, overrides map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"iss":   as.URL,
		"sub":   "user_123",
		"aud":   "https://chromemcp.example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"email": "me@example.com",
	}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	return as.sign(t, map[string]any{"alg": "RS256", "kid": as.kid, "typ": "JWT"}, claims)
}

func (as *fakeAS) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, as.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newTestHandler(t *testing.T, as *fakeAS, allowedEmail string) http.Handler {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mcp ok"))
	})
	h, err := newOAuthHandler(context.Background(), &oauthConfig{
		Issuer:       as.URL,
		PublicURL:    "https://chromemcp.example.com",
		AllowedEmail: allowedEmail,
	}, inner)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestOAuthHandlerTokens(t *testing.T) {
	as := newFakeAS(t)
	h := newTestHandler(t, as, "me@example.com")

	other := newFakeAS(t) // trusted by nobody: its key is not in as's JWKS

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"valid", as.mint(t, nil), http.StatusOK},
		{"valid audience array", as.mint(t, map[string]any{"aud": []string{"x", "https://chromemcp.example.com"}}), http.StatusOK},
		{"valid email case-insensitive", as.mint(t, map[string]any{"email": "ME@EXAMPLE.COM"}), http.StatusOK},
		{"missing token", "", http.StatusUnauthorized},
		{"garbage", "not-a-jwt", http.StatusUnauthorized},
		{"expired", as.mint(t, map[string]any{"exp": time.Now().Add(-2 * time.Hour).Unix()}), http.StatusUnauthorized},
		{"no expiry", as.mint(t, map[string]any{"exp": nil}), http.StatusUnauthorized},
		{"wrong issuer", as.mint(t, map[string]any{"iss": "https://evil.example.com"}), http.StatusUnauthorized},
		{"wrong audience", as.mint(t, map[string]any{"aud": "https://other.example.com"}), http.StatusUnauthorized},
		{"no audience", as.mint(t, map[string]any{"aud": nil}), http.StatusUnauthorized},
		{"wrong email", as.mint(t, map[string]any{"email": "attacker@example.com"}), http.StatusUnauthorized},
		{"no email", as.mint(t, map[string]any{"email": nil}), http.StatusUnauthorized},
		{"not yet valid", as.mint(t, map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}), http.StatusUnauthorized},
		{"foreign signature", other.mint(t, map[string]any{"iss": as.URL}), http.StatusUnauthorized},
		{"alg none", as.sign(t, map[string]any{"alg": "none", "kid": as.kid}, map[string]any{"iss": as.URL}), http.StatusUnauthorized},
		// The JWKS was fetched at handler construction, so the unknown-kid
		// refetch is rate-limited away and the key stays unknown.
		{"unknown kid", as.sign(t, map[string]any{"alg": "RS256", "kid": "rotated"}, map[string]any{
			"iss": as.URL, "aud": "https://chromemcp.example.com",
			"exp": time.Now().Add(time.Hour).Unix(), "email": "me@example.com",
		}), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := get(t, h, "/", tt.token)
			if w.Code != tt.want {
				b, _ := httputil.DumpResponse(w.Result(), true)
				t.Errorf("got %d, want %d\n%s", w.Code, tt.want, b)
			}
		})
	}
}

func TestOAuthHandlerNoEmailPin(t *testing.T) {
	as := newFakeAS(t)
	h := newTestHandler(t, as, "") // no -allowed-email: any authenticated user passes
	if w := get(t, h, "/", as.mint(t, map[string]any{"email": nil})); w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestOAuthChallengeAdvertisesMetadata(t *testing.T) {
	as := newFakeAS(t)
	h := newTestHandler(t, as, "me@example.com")

	w := get(t, h, "/", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	challenge := w.Header().Get("WWW-Authenticate")
	wantMeta := "https://chromemcp.example.com/.well-known/oauth-protected-resource"
	if !strings.Contains(challenge, "resource_metadata") || !strings.Contains(challenge, wantMeta) {
		t.Errorf("WWW-Authenticate %q does not point at %s", challenge, wantMeta)
	}
}

func TestOAuthWellKnown(t *testing.T) {
	as := newFakeAS(t)
	h := newTestHandler(t, as, "me@example.com")

	// Protected-resource metadata (RFC 9728) is served without a token.
	req := httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PRM: got %d, want 200", w.Code)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &prm); err != nil {
		t.Fatal(err)
	}
	if prm.Resource != "https://chromemcp.example.com" {
		t.Errorf("resource = %q", prm.Resource)
	}
	if len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != as.URL {
		t.Errorf("authorization_servers = %v, want [%s]", prm.AuthorizationServers, as.URL)
	}

	// The compatibility shim re-serves the issuer's own metadata.
	req = httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("AS shim: got %d, want 200", w.Code)
	}
	var asm struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &asm); err != nil {
		t.Fatal(err)
	}
	if asm.Issuer != as.URL || asm.JWKSURI == "" {
		t.Errorf("shim issuer = %q jwks_uri = %q", asm.Issuer, asm.JWKSURI)
	}
}

// TestPublicHostBehindLoopbackProxy reproduces the deployed topology: the
// listener is loopback (TLS proxy dials 127.0.0.1) while the Host header is
// the public name. The SDK's DNS-rebinding heuristic 403s that shape unless
// newHTTPHandler switches it off for the OAuth mode — an authenticated
// initialize must reach the MCP handler.
func TestPublicHostBehindLoopbackProxy(t *testing.T) {
	as := newFakeAS(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "chromemcp-test", Version: "0"}, nil)
	h, err := newHTTPHandler(server, &oauthConfig{
		Issuer:       as.URL,
		PublicURL:    "https://chromemcp.example.com",
		AllowedEmail: "me@example.com",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h) // real loopback listener, unlike NewRecorder
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"debug","version":"0"}}}`
	req, err := http.NewRequest("POST", ts.URL+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "chromemcp.example.com" // public Host over the loopback conn
	req.Header.Set("Authorization", "Bearer "+as.mint(t, nil))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize with public Host: got %d, want 200\n%s", resp.StatusCode, b)
	}
}

func TestOAuthHandlerBadIssuer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := newOAuthHandler(context.Background(), &oauthConfig{
		Issuer:    srv.URL,
		PublicURL: "https://chromemcp.example.com",
	}, http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected an error for an issuer with no metadata")
	}
}
