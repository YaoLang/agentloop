package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// JWT authenticates HS256 bearer tokens signed with AGENTLOOP_JWT_SECRET.
// Claims: sub (subject), tid (tenant), scp (scopes, array or space-separated).
type JWT struct {
	Secret string
}

// NewJWT returns a JWT authenticator. An empty secret always skips.
func NewJWT(secret string) *JWT { return &JWT{Secret: secret} }

func (j *JWT) Name() string { return "jwt" }

func (j *JWT) Authenticate(r *http.Request) (Principal, error) {
	if j == nil || j.Secret == "" {
		return Principal{}, ErrSkip
	}
	tok, ok := BearerToken(r)
	if !ok {
		return Principal{}, ErrSkip
	}
	if strings.HasPrefix(tok, apiKeyPrefix) {
		return Principal{}, ErrSkip
	}
	if strings.Count(tok, ".") != 2 {
		return Principal{}, ErrSkip
	}
	p, err := parseJWT(j.Secret, tok)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	return p, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	Sub string          `json:"sub"`
	Tid string          `json:"tid"`
	Scp json.RawMessage `json:"scp"`
	Exp int64           `json:"exp,omitempty"`
	Iat int64           `json:"iat,omitempty"`
}

// SignJWT mints an HS256 token for p. ttl of 0 omits exp.
func SignJWT(secret string, p Principal, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("auth: empty jwt secret")
	}
	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	claims := jwtPayload{
		Sub: p.Subject,
		Tid: p.TenantID,
		Iat: time.Now().UTC().Unix(),
	}
	if ttl != 0 {
		claims.Exp = time.Now().UTC().Add(ttl).Unix()
	}
	scp, err := json.Marshal(p.Scopes)
	if err != nil {
		return "", err
	}
	claims.Scp = scp
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signing))
	sig := enc.EncodeToString(mac.Sum(nil))
	return signing + "." + sig, nil
}

func parseJWT(secret, token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, ErrUnauthorized
	}
	enc := base64.RawURLEncoding
	hb, err := enc.DecodeString(parts[0])
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return Principal{}, ErrUnauthorized
	}
	if !strings.EqualFold(hdr.Alg, "HS256") {
		return Principal{}, ErrUnauthorized
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := enc.DecodeString(parts[2])
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	if !hmac.Equal(want, got) {
		return Principal{}, ErrUnauthorized
	}
	pb, err := enc.DecodeString(parts[1])
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	var pl jwtPayload
	if err := json.Unmarshal(pb, &pl); err != nil {
		return Principal{}, ErrUnauthorized
	}
	if pl.Tid == "" || !SafeID(pl.Tid) {
		return Principal{}, ErrUnauthorized
	}
	if pl.Exp > 0 && time.Now().UTC().Unix() > pl.Exp {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		TenantID: pl.Tid,
		Subject:  pl.Sub,
		Scopes:   parseScopes(pl.Scp),
	}, nil
}

func parseScopes(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		return strings.Fields(s)
	}
	return nil
}
