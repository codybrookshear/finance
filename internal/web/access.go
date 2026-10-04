package web

// Cloudflare Access JWT verification, standard library only.
//
// Access signs a JWT for every request it lets through and sends it in the
// Cf-Access-Jwt-Assertion header. cloudflared on the droplet already checks
// it (originRequest.access in deploy/cloudflared/config.yml); the app checks
// it again, so nothing that reaches the port some other way gets in.

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	accessHeader   = "Cf-Access-Jwt-Assertion"
	maxTokenLen    = 8 << 10
	clockLeeway    = time.Minute
	keysRecheckMin = 30 * time.Second
)

// Access holds what a valid token must match.
type Access struct {
	TeamDomain    string   // <team>.cloudflareaccess.com
	Audience      string   // the Access application's AUD tag
	AllowedEmails []string // compared case-insensitively
	Keys          KeySource
	Now           func() time.Time // nil = time.Now
}

// KeySource returns the RSA public key Access signed with, by key ID.
type KeySource interface {
	Key(kid string) (*rsa.PublicKey, error)
}

// Verify checks a token's signature and claims and returns the user's email.
// Errors describe the failure for logs; they never include the token.
func (a *Access) Verify(token string) (string, error) {
	if token == "" {
		return "", errors.New("no Access token")
	}
	if len(token) > maxTokenLen {
		return "", errors.New("Access token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed Access token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return "", fmt.Errorf("token header: %w", err)
	}
	if hdr.Alg != "RS256" { // never "none", never HMAC
		return "", fmt.Errorf("unexpected token algorithm %q", hdr.Alg)
	}
	key, err := a.Keys.Key(hdr.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("token signature is not base64url")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("bad token signature")
	}

	var c struct {
		Iss   string   `json:"iss"`
		Aud   audience `json:"aud"`
		Exp   int64    `json:"exp"`
		Nbf   int64    `json:"nbf"`
		Iat   int64    `json:"iat"`
		Email string   `json:"email"`
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return "", fmt.Errorf("token claims: %w", err)
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	switch {
	case c.Iss != "https://"+a.TeamDomain:
		return "", fmt.Errorf("token issuer %q is not this team", c.Iss)
	case !slices.Contains(c.Aud, a.Audience):
		return "", errors.New("token is for a different Access application")
	case c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockLeeway)):
		return "", errors.New("token expired")
	case c.Nbf != 0 && now.Add(clockLeeway).Before(time.Unix(c.Nbf, 0)):
		return "", errors.New("token not valid yet")
	case c.Iat != 0 && now.Add(clockLeeway).Before(time.Unix(c.Iat, 0)):
		return "", errors.New("token issued in the future")
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" { // e.g. a service token: not for the browser UI
		return "", errors.New("token has no user email")
	}
	if !slices.ContainsFunc(a.AllowedEmails, func(e string) bool { return strings.EqualFold(e, email) }) {
		return "", fmt.Errorf("%s is not an allowed user", email)
	}
	return email, nil
}

// audience accepts the aud claim as either a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor a list of strings")
	}
	*a = many
	return nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("not base64url")
	}
	return json.Unmarshal(b, v)
}

// FileKeys reads Access's signing keys from a JSON file: the response of
// https://<team>.cloudflareaccess.com/cdn-cgi/access/certs, which a timer on
// the host keeps fresh (the web container has no internet access). The file
// is re-read when it changes, checked at most every 30s, or immediately when
// a token names a key ID we don't have (Cloudflare rotates keys).
type FileKeys struct {
	Path string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	modTime time.Time
	size    int64
	checked time.Time
}

func (f *FileKeys) Key(kid string) (*rsa.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(false); err != nil && f.keys == nil {
		return nil, err
	}
	if k, ok := f.keys[kid]; ok {
		return k, nil
	}
	if err := f.load(true); err != nil && f.keys == nil {
		return nil, err
	}
	if k, ok := f.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown Access signing key %q", kid)
}

// Check loads the file now, so a missing or bad file fails at startup.
func (f *FileKeys) Check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load(true)
}

func (f *FileKeys) load(force bool) error {
	if !force && f.keys != nil && time.Since(f.checked) < keysRecheckMin {
		return nil
	}
	f.checked = time.Now()
	st, err := os.Stat(f.Path)
	if err != nil {
		return fmt.Errorf("Access keys file: %w", err)
	}
	if f.keys != nil && st.ModTime().Equal(f.modTime) && st.Size() == f.size {
		return nil
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return fmt.Errorf("Access keys file: %w", err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, 1<<20))
	if err != nil {
		return fmt.Errorf("Access keys file: %w", err)
	}
	keys, err := parseJWKS(b)
	if err != nil {
		return fmt.Errorf("Access keys file: %w", err)
	}
	f.keys, f.modTime, f.size = keys, st.ModTime(), st.Size()
	return nil
}

// parseJWKS extracts the RSA keys (at least 2048-bit) from a JWK set.
func parseJWKS(b []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, errors.New("not a JWK set")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			return nil, fmt.Errorf("key %q is malformed", k.Kid)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 || pub.E < 3 || pub.E%2 == 0 {
			return nil, fmt.Errorf("key %q is too weak or invalid", k.Kid)
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no RSA keys")
	}
	return keys, nil
}
