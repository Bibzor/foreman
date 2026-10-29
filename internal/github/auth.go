package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Secret holds a credential. It never prints itself: fmt, errors and slog
// all see "[redacted]". Reveal is the only way to read it.
type Secret struct{ v string }

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the credential.
func (s Secret) Reveal() string { return s.v }

func (Secret) String() string { return "[redacted]" }

// GoString keeps %#v from printing the field.
func (Secret) GoString() string { return "[redacted]" }

// MarshalText keeps encoders from printing the field.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// LoadKey reads a PEM-encoded RSA private key (PKCS#1 or PKCS#8).
func LoadKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return ParseKey(b)
}

// ParseKey parses a PEM-encoded RSA private key. Errors never include the
// key material.
func ParseKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("private key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key: not an RSA key in PKCS#1 or PKCS#8 form")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key: not an RSA key")
	}
	return rk, nil
}

// appJWT returns a JWT authenticating as the App, valid for 9 minutes
// (GitHub accepts at most 10). iat is backdated for clock drift.
func appJWT(key *rsa.PrivateKey, appID int64, now time.Time) (Secret, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{now.Add(-60 * time.Second).Unix(), now.Add(9 * time.Minute).Unix(), strconv.FormatInt(appID, 10)})
	if err != nil {
		return Secret{}, err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return Secret{}, fmt.Errorf("sign JWT: %w", err)
	}
	return NewSecret(signing + "." + base64.RawURLEncoding.EncodeToString(sig)), nil
}
