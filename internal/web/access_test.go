package web

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testTeam = "example.cloudflareaccess.com"
	testAUD  = "0123456789abcdef"
)

var testNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signRS256(t *testing.T, k *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func goodClaims() map[string]any {
	return map[string]any{
		"iss":   "https://" + testTeam,
		"aud":   []string{testAUD},
		"exp":   testNow.Add(time.Hour).Unix(),
		"nbf":   testNow.Add(-time.Minute).Unix(),
		"iat":   testNow.Add(-time.Minute).Unix(),
		"email": "Me@Example.com",
		"type":  "app",
	}
}

func writeJWKS(t *testing.T, path string, keys map[string]*rsa.PublicKey) {
	t.Helper()
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	for kid, k := range keys {
		set.Keys = append(set.Keys, map[string]string{
			"kid": kid, "kty": "RSA", "alg": "RS256",
			"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes()),
		})
	}
	b, _ := json.Marshal(set)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testAccess(t *testing.T, k *rsa.PrivateKey) (*Access, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "certs.json")
	writeJWKS(t, path, map[string]*rsa.PublicKey{"k1": &k.PublicKey})
	return &Access{
		TeamDomain:    testTeam,
		Audience:      testAUD,
		AllowedEmails: []string{"me@example.com"},
		Keys:          &FileKeys{Path: path},
		Now:           func() time.Time { return testNow },
	}, path
}

func TestAccessVerify(t *testing.T) {
	k := testKey(t)
	other := testKey(t)
	a, _ := testAccess(t, k)

	email, err := a.Verify(signRS256(t, k, "k1", goodClaims()))
	if err != nil || email != "me@example.com" {
		t.Fatalf("valid token: email=%q err=%v", email, err)
	}

	with := func(key string, v any) map[string]any {
		c := goodClaims()
		if v == nil {
			delete(c, key)
		} else {
			c[key] = v
		}
		return c
	}
	hmacToken := func() string {
		h, _ := json.Marshal(map[string]string{"alg": "HS256", "kid": "k1"})
		c, _ := json.Marshal(goodClaims())
		in := b64(h) + "." + b64(c)
		m := hmac.New(sha256.New, k.PublicKey.N.Bytes()) // classic alg-confusion attempt
		m.Write([]byte(in))
		return in + "." + b64(m.Sum(nil))
	}
	noneToken := func() string {
		h, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k1"})
		c, _ := json.Marshal(goodClaims())
		return b64(h) + "." + b64(c) + "."
	}
	tampered := func() string {
		tok := signRS256(t, k, "k1", goodClaims())
		parts := strings.Split(tok, ".")
		c := goodClaims()
		c["email"] = "me@example.com "
		cb, _ := json.Marshal(c)
		return parts[0] + "." + b64(cb) + "." + parts[2]
	}

	for name, tok := range map[string]string{
		"empty":              "",
		"garbage":            "not.a.jwt",
		"two parts":          "a.b",
		"oversized":          strings.Repeat("a", maxTokenLen+1),
		"wrong signing key":  signRS256(t, other, "k1", goodClaims()),
		"unknown kid":        signRS256(t, k, "k2", goodClaims()),
		"alg none":           noneToken(),
		"alg HS256":          hmacToken(),
		"tampered claims":    tampered(),
		"wrong issuer":       signRS256(t, k, "k1", with("iss", "https://evil.cloudflareaccess.com")),
		"wrong audience":     signRS256(t, k, "k1", with("aud", []string{"someone-else"})),
		"expired":            signRS256(t, k, "k1", with("exp", testNow.Add(-2*time.Minute).Unix())),
		"no exp":             signRS256(t, k, "k1", with("exp", nil)),
		"not yet valid":      signRS256(t, k, "k1", with("nbf", testNow.Add(5*time.Minute).Unix())),
		"issued in future":   signRS256(t, k, "k1", with("iat", testNow.Add(5*time.Minute).Unix())),
		"no email (service)": signRS256(t, k, "k1", with("email", nil)),
		"other user":         signRS256(t, k, "k1", with("email", "someone@example.com")),
	} {
		if email, err := a.Verify(tok); err == nil {
			t.Errorf("%s: accepted (email %q)", name, email)
		}
	}

	// aud may also be a plain string; small clock skew is tolerated.
	if _, err := a.Verify(signRS256(t, k, "k1", with("aud", testAUD))); err != nil {
		t.Errorf("string aud: %v", err)
	}
	if _, err := a.Verify(signRS256(t, k, "k1", with("exp", testNow.Add(-30*time.Second).Unix()))); err != nil {
		t.Errorf("exp within leeway: %v", err)
	}
}

func TestFileKeysPicksUpRotation(t *testing.T) {
	k1, k2 := testKey(t), testKey(t)
	a, path := testAccess(t, k1)
	if _, err := a.Verify(signRS256(t, k1, "k1", goodClaims())); err != nil {
		t.Fatal(err)
	}
	// Cloudflare rotates to k2; the host timer rewrites the file.
	writeJWKS(t, path, map[string]*rsa.PublicKey{"k1": &k1.PublicKey, "k2": &k2.PublicKey})
	if _, err := a.Verify(signRS256(t, k2, "k2", goodClaims())); err != nil {
		t.Fatalf("new key not picked up: %v", err)
	}
}

func TestParseJWKSRejectsWeakKeys(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "certs.json")
	writeJWKS(t, path, map[string]*rsa.PublicKey{"weak": &weak.PublicKey})
	b, _ := os.ReadFile(path)
	if _, err := parseJWKS(b); err == nil {
		t.Fatal("accepted a 1024-bit key")
	}
	if _, err := parseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Fatal("accepted an empty key set")
	}
}
