// Package auth is the pluggable authentication surface for agentloopd.
// Authenticators skip (ErrSkip) when their credential type is absent;
// the first non-skip success on a Chain wins.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrSkip means this authenticator's credentials are not present on the
// request; the chain should try the next plugin.
var ErrSkip = errors.New("auth: skip")

// ErrUnauthorized means credentials were presented but were invalid,
// or no plugin accepted the request.
var ErrUnauthorized = errors.New("auth: unauthorized")

// Principal is an authenticated caller. TenantID isolates data; Scopes
// gate routes (e.g. runs:write, admin).
type Principal struct {
	TenantID string
	Subject  string
	Scopes   []string
}

// HasScope reports whether p includes the named scope.
func (p Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Authenticator is one auth plugin (API key, JWT, admin key, …).
type Authenticator interface {
	Name() string
	Authenticate(r *http.Request) (Principal, error)
}

// Chain is a list of authenticators. The first non-skip success wins.
// A non-skip error fails the request immediately (invalid creds).
type Chain []Authenticator

func (c Chain) Authenticate(r *http.Request) (Principal, error) {
	for _, a := range c {
		if a == nil {
			continue
		}
		p, err := a.Authenticate(r)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, ErrSkip) {
			continue
		}
		return Principal{}, err
	}
	return Principal{}, ErrUnauthorized
}

type ctxKey struct{}

// WithPrincipal stores p on ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// BearerToken extracts the raw token from Authorization: Bearer.
// It never logs the value; callers must not either.
func BearerToken(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return "", false
	}
	typ, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(typ, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false
	}
	return tok, true
}

// TimingSafeEqual compares a and b in constant time via SHA-256 so
// unequal lengths do not leak.
func TimingSafeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// SafeID reports whether id is a single path component safe to join
// under the data directory (no slashes, no "..").
func SafeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
