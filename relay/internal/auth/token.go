// Package auth issues and verifies the relay's own tokens and runs the
// optional Google and GitHub sign-in. Anonymous use needs none of this.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/idestis/knock/protocol"
)

const (
	KindAccess  = "access"
	KindRefresh = "refresh"

	AccessTTL  = time.Hour
	RefreshTTL = 90 * 24 * time.Hour // sliding: every refresh issues a new one
)

var ErrInvalidToken = errors.New("invalid token")

// Claims is the token payload. Tokens are stateless and HMAC-signed, so a
// relay restart (or another replica with the same secret) keeps users signed in.
type Claims struct {
	Kind     string `json:"knd"`
	Subject  string `json:"sub"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Provider string `json:"prv"`
	Avatar   string `json:"avt,omitempty"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	Nonce    string `json:"jti"`
}

func (c Claims) Identity() protocol.Identity {
	return protocol.Identity{ID: c.Subject, Name: c.Name, Email: c.Email, Provider: c.Provider, Verified: true, Avatar: c.Avatar}
}

type Signer struct{ key []byte }

func NewSigner(secret []byte) *Signer { return &Signer{key: secret} }

func (s *Signer) Issue(kind string, id protocol.Identity, ttl time.Duration) (string, time.Time) {
	now := time.Now()
	exp := now.Add(ttl)
	c := Claims{Kind: kind, Subject: id.ID, Name: id.Name, Email: id.Email, Provider: id.Provider, Avatar: id.Avatar,
		IssuedAt: now.Unix(), Expires: exp.Unix(), Nonce: RandomString(9)}
	body, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(body)
	return p + "." + s.sign(p), exp
}

func (s *Signer) Verify(token, kind string) (Claims, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(p))) {
		return Claims{}, ErrInvalidToken
	}
	body, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if c.Kind != kind || time.Now().Unix() >= c.Expires {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

func (s *Signer) sign(p string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("knock.v1." + p))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RandomString returns n random bytes, base64url encoded.
func RandomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
