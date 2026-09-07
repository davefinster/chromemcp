package main

// OAuth 2.1 resource-server support for the Streamable HTTP transport, per
// the MCP authorization spec: bearer tokens are validated against an external
// authorization server (any RFC 8414 issuer; built for AuthKit), and the
// RFC 9728 protected-resource metadata that clients use to find that server
// is published under /.well-known/. This is the same stack, verbatim, as the
// sibling m2mcp / barrys / trackmcp servers, so one AuthKit environment and
// one -allowed-email pin govern every MCP server at the edge.
//
// The verifier is deliberately strict: RS256 only, the key must come from the
// issuer's JWKS, the token must be addressed to this server (aud = the
// -public-url resource identifier), and -allowed-email pins it to a single
// account on top of whatever sign-in restrictions the issuer enforces.

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// oauthConfig configures bearer-token auth for `chromemcp serve -http`.
type oauthConfig struct {
	Issuer       string // authorization server issuer URL, e.g. https://xyz.authkit.app
	PublicURL    string // canonical public URL of this server: the OAuth resource identifier
	AllowedEmail string // accept only tokens whose email claim matches (optional)
}

const prmPath = "/.well-known/oauth-protected-resource"

// newOAuthHandler fetches the authorization server's metadata (failing fast
// on a bad issuer) and wraps inner so that every path except the two
// /.well-known/ documents requires a valid bearer token.
func newOAuthHandler(ctx context.Context, cfg *oauthConfig, inner http.Handler) (http.Handler, error) {
	issuer := strings.TrimSuffix(cfg.Issuer, "/")
	resource := strings.TrimSuffix(cfg.PublicURL, "/")

	hc := &http.Client{Timeout: 10 * time.Second}
	metaURL := issuer + "/.well-known/oauth-authorization-server"
	meta, err := oauthex.GetAuthServerMeta(ctx, metaURL, issuer, hc)
	if err != nil {
		return nil, fmt.Errorf("authorization server metadata: %w", err)
	}
	if meta == nil { // GetAuthServerMeta reports 4xx as (nil, nil)
		return nil, fmt.Errorf("no authorization server metadata at %s", metaURL)
	}
	if meta.JWKSURI == "" {
		return nil, fmt.Errorf("authorization server metadata at %s has no jwks_uri", metaURL)
	}

	keys := &jwksCache{uri: meta.JWKSURI, client: hc}
	if err := keys.refresh(ctx); err != nil {
		return nil, fmt.Errorf("fetching JWKS: %w", err)
	}

	require := auth.RequireBearerToken(
		newTokenVerifier(meta.Issuer, resource, cfg.AllowedEmail, keys),
		&auth.RequireBearerTokenOptions{
			ResourceMetadataURL: resource + prmPath,
			ClockSkew:           time.Minute,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(prmPath, auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{meta.Issuer},
		ScopesSupported:        meta.ScopesSupported,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Chrome browser sessions for agents",
	}))
	// Compatibility shim: pre-RFC 9728 clients look for the authorization
	// server's metadata on the resource host itself instead of following
	// the pointer above, so re-serve the issuer's document.
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	})
	mux.Handle("/", require(inner))
	return mux, nil
}

// newTokenVerifier accepts RS256 JWTs signed by a JWKS key, issued by issuer,
// addressed to resource, and — when allowedEmail is non-empty — carrying a
// matching email claim (AuthKit adds it via the environment's JWT template).
// The email pin keeps this server single-account even if the authorization
// server ever admits more users. Expiry is left to RequireBearerToken, which
// rejects missing or elapsed Expiration values.
func newTokenVerifier(issuer, resource, allowedEmail string, keys *jwksCache) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: not a JWT", auth.ErrInvalidToken)
		}
		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("%w: bad header encoding", auth.ErrInvalidToken)
		}
		var header struct {
			Alg string `json:"alg"`
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(headerJSON, &header); err != nil {
			return nil, fmt.Errorf("%w: bad header", auth.ErrInvalidToken)
		}
		if header.Alg != "RS256" { // the only alg the issuer signs with; never accept "none"
			return nil, fmt.Errorf("%w: unexpected alg %q", auth.ErrInvalidToken, header.Alg)
		}
		key := keys.keyFor(ctx, header.Kid)
		if key == nil {
			return nil, fmt.Errorf("%w: unknown signing key %q", auth.ErrInvalidToken, header.Kid)
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			return nil, fmt.Errorf("%w: bad signature encoding", auth.ErrInvalidToken)
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
			return nil, fmt.Errorf("%w: bad signature", auth.ErrInvalidToken)
		}

		claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%w: bad claims encoding", auth.ErrInvalidToken)
		}
		var claims struct {
			Iss   string   `json:"iss"`
			Sub   string   `json:"sub"`
			Aud   audience `json:"aud"`
			Exp   int64    `json:"exp"`
			Nbf   int64    `json:"nbf"`
			Email string   `json:"email"`
			Scope string   `json:"scope"`
		}
		if err := json.Unmarshal(claimsJSON, &claims); err != nil {
			return nil, fmt.Errorf("%w: bad claims", auth.ErrInvalidToken)
		}
		if claims.Iss != issuer {
			return nil, fmt.Errorf("%w: issuer %q not trusted", auth.ErrInvalidToken, claims.Iss)
		}
		if !claims.Aud.contains(resource) {
			return nil, fmt.Errorf("%w: token not addressed to %s", auth.ErrInvalidToken, resource)
		}
		if claims.Nbf != 0 && time.Now().Add(time.Minute).Before(time.Unix(claims.Nbf, 0)) {
			return nil, fmt.Errorf("%w: token not yet valid", auth.ErrInvalidToken)
		}
		if allowedEmail != "" && !strings.EqualFold(claims.Email, allowedEmail) {
			return nil, fmt.Errorf("%w: token is not for the allowed account", auth.ErrInvalidToken)
		}
		info := &auth.TokenInfo{
			Scopes:     strings.Fields(claims.Scope),
			Expiration: time.Unix(claims.Exp, 0),
			UserID:     claims.Sub,
		}
		if claims.Exp == 0 {
			info.Expiration = time.Time{} // let the middleware reject it as missing
		}
		if claims.Email != "" {
			info.Extra = map[string]any{"email": claims.Email}
		}
		return info, nil
	}
}

// audience is a JWT aud claim, which may be a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = audience{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return err
	}
	*a = audience(ss)
	return nil
}

func (a audience) contains(want string) bool {
	for _, s := range a {
		if strings.TrimSuffix(s, "/") == want {
			return true
		}
	}
	return false
}

// jwksCache holds the issuer's RSA signing keys. An unknown key ID triggers a
// refetch (key rotation) at most once a minute.
type jwksCache struct {
	uri    string
	client *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

func (c *jwksCache) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.uri, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", c.uri, resp.Status)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("decoding JWKS from %s: %w", c.uri, err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil || len(n) == 0 {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
	}
	c.mu.Lock()
	c.keys, c.lastFetch = keys, time.Now()
	c.mu.Unlock()
	return nil
}

func (c *jwksCache) keyFor(ctx context.Context, kid string) *rsa.PublicKey {
	c.mu.Lock()
	key, ok := c.keys[kid]
	stale := time.Since(c.lastFetch) > time.Minute
	c.mu.Unlock()
	if ok {
		return key
	}
	if !stale {
		return nil
	}
	if err := c.refresh(ctx); err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys[kid]
}
