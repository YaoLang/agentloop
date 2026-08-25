package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimingSafeEqual(t *testing.T) {
	if !TimingSafeEqual("abc", "abc") {
		t.Fatal("equal strings")
	}
	if TimingSafeEqual("abc", "abd") {
		t.Fatal("unequal strings")
	}
	if TimingSafeEqual("abc", "ab") {
		t.Fatal("unequal lengths")
	}
}

func TestChainSkipThenSuccess(t *testing.T) {
	admin := NewAdmin("adminkey")
	store, err := NewFileKeyStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := Chain{admin, NewAPIKey(store), NewJWT("jwt-secret")}

	p, err := chain.Authenticate(bearerReq("adminkey"))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != AdminTenant || !p.HasScope(ScopeAdmin) || !p.HasScope(ScopeRunsWrite) {
		t.Fatalf("admin principal: %+v", p)
	}
}

func TestChainMissingIsUnauthorized(t *testing.T) {
	chain := Chain{NewAdmin("adminkey"), NewJWT("secret")}
	_, err := chain.Authenticate(plainReq())
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestAPIKeyHappyAndWrong(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileKeyStore(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret, rec, err := store.Mint("acme", []string{ScopeRunsWrite})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "alk_") {
		t.Fatalf("secret prefix missing")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("raw secret written to disk")
	}
	sum := sha256.Sum256([]byte(secret))
	if !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatal("hash not stored")
	}

	a := NewAPIKey(store)
	p, err := a.Authenticate(bearerReq(secret))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "acme" || p.Subject != rec.ID || !p.HasScope(ScopeRunsWrite) {
		t.Fatalf("principal=%+v rec=%+v", p, rec)
	}

	_, err = a.Authenticate(bearerReq("alk_" + strings.Repeat("0", 64)))
	if err != ErrUnauthorized {
		t.Fatalf("wrong key err=%v", err)
	}

	_, err = a.Authenticate(plainReq())
	if err != ErrSkip {
		t.Fatalf("missing creds should skip, err=%v", err)
	}
}

func TestJWTHappyAndBadSig(t *testing.T) {
	const secret = "jwt-secret-for-tests"
	tok, err := SignJWT(secret, Principal{
		TenantID: "acme",
		Subject:  "user-1",
		Scopes:   []string{ScopeRunsWrite},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	j := NewJWT(secret)
	p, err := j.Authenticate(bearerReq(tok))
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != "acme" || p.Subject != "user-1" || !p.HasScope(ScopeRunsWrite) {
		t.Fatalf("principal=%+v", p)
	}

	_, err = NewJWT("other-secret").Authenticate(bearerReq(tok))
	if err != ErrUnauthorized {
		t.Fatalf("bad sig err=%v", err)
	}

	_, err = j.Authenticate(bearerReq("not-a-jwt"))
	if err != ErrSkip {
		t.Fatalf("non-jwt should skip, err=%v", err)
	}
}

func TestJWTRejectsAlgNone(t *testing.T) {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]string{"sub": "x", "tid": "acme"})
	tok := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "."
	_, err := NewJWT("secret").Authenticate(bearerReq(tok))
	if err != ErrUnauthorized {
		t.Fatalf("alg=none err=%v", err)
	}
}

func TestJWTExpired(t *testing.T) {
	tok, err := SignJWT("secret", Principal{TenantID: "acme", Subject: "u", Scopes: []string{ScopeRunsWrite}}, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewJWT("secret").Authenticate(bearerReq(tok))
	if err != ErrUnauthorized {
		t.Fatalf("expired err=%v", err)
	}
}

func TestAdminSkipOnMismatch(t *testing.T) {
	a := NewAdmin("real-admin")
	_, err := a.Authenticate(bearerReq("nope"))
	if err != ErrSkip {
		t.Fatalf("mismatch should skip, err=%v", err)
	}
}

func TestSafeID(t *testing.T) {
	for _, id := range []string{"acme", "a", "t_1", "T-2", "_admin"} {
		if !SafeID(id) {
			t.Fatalf("want safe %q", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../x", "a/b", "a\\b", "has space", strings.Repeat("x", 65)} {
		if SafeID(id) {
			t.Fatalf("want unsafe %q", id)
		}
	}
}

func bearerReq(tok string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func plainReq() *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	return r
}
