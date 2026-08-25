package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/auth"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/sandbox"
)

const (
	testAdmin = "test-admin-key"
	testJWT   = "test-jwt-secret-please-change"
)

func testServer(t *testing.T, opts ...func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		DataDir:       t.TempDir(),
		DefaultModel:  "mock",
		MaxConcurrent: 8,
		RunTimeout:    15 * time.Second,
		ToolTimeout:   time.Second,
		AdminKey:      testAdmin,
		JWTSecret:     testJWT,
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitIdle(s) })
	return s
}

func waitIdle(s *Server) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.quota.mu.Lock()
		n := 0
		for _, c := range s.quota.running {
			n += c
		}
		s.quota.mu.Unlock()
		if n == 0 {
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func do(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v body=%s", rec.Result().Status, err, rec.Body.String())
	}
}

func createTenant(t *testing.T, s *Server, id, name string) {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/v1/admin/tenants", testAdmin, map[string]string{"id": id, "name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tenant %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

func mintKey(t *testing.T, s *Server, tenant string, scopes []string) string {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/v1/admin/keys", testAdmin, map[string]any{
		"tenant_id": tenant,
		"scopes":    scopes,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint key: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Secret string `json:"secret"`
	}
	decode(t, rec, &out)
	if out.Secret == "" {
		t.Fatal("empty secret")
	}
	return out.Secret
}

func TestHealthzNoAuth(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/healthz", "", nil)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnauthorizedMissingAndWrongKey(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")

	rec := do(t, s, http.MethodPost, "/v1/runs", "", map[string]string{"goal": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, http.MethodPost, "/v1/runs", "totally-wrong", map[string]string{"goal": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, http.MethodPost, "/v1/runs", "alk_0000000000000000000000000000000000000000000000000000000000000000", map[string]string{"goal": "x"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong alk_ key: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJWTHappy(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	tok, err := auth.SignJWT(testJWT, auth.Principal{
		TenantID: "acme",
		Subject:  "user-1",
		Scopes:   []string{auth.ScopeRunsWrite},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, http.MethodPost, "/v1/runs", tok, map[string]string{"goal": "hello jwt", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("jwt run: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decode(t, rec, &out)
	if out.ID == "" {
		t.Fatal("missing run id")
	}
	waitRun(t, s, tok, out.ID)
}

func TestAPIKeyHappy(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "hello apikey", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apikey run: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	decode(t, rec, &out)
	got := waitRun(t, s, key, out.ID)
	if got.Status != statusCompleted && got.Status != statusFailed {
		t.Fatalf("status=%s", got.Status)
	}
}

func TestAdminMintsKeys(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	rec := do(t, s, http.MethodPost, "/v1/admin/keys", testAdmin, map[string]any{
		"tenant_id": "acme",
		"scopes":    []string{auth.ScopeRunsWrite},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decode(t, rec, &out)
	secret, _ := out["secret"].(string)
	if secret == "" {
		t.Fatal("secret not returned once")
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("secret persisted to keys.json")
	}
}

func TestNonAdminCannotMint(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, s, http.MethodPost, "/v1/admin/keys", key, map[string]any{
		"tenant_id": "acme",
		"scopes":    []string{auth.ScopeRunsWrite},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin mint: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodPost, "/v1/admin/tenants", key, map[string]string{"id": "other", "name": "x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin tenant: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIsolationTenants(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "alpha", "Alpha")
	createTenant(t, s, "beta", "Beta")
	keyA := mintKey(t, s, "alpha", []string{auth.ScopeRunsWrite})
	keyB := mintKey(t, s, "beta", []string{auth.ScopeRunsWrite})

	rec := do(t, s, http.MethodPost, "/v1/runs", keyA, map[string]string{"goal": "secret-from-alpha", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("alpha run: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	waitRun(t, s, keyA, created.ID)

	note := filepath.Join(s.cfg.DataDir, "tenants", "alpha", "workspace", "agent-notes.txt")
	raw, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("alpha workspace file missing: %v", err)
	}
	if !bytes.Contains(raw, []byte("secret-from-alpha")) {
		t.Fatalf("alpha file contents=%q", raw)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "tenants", "beta", "workspace", "agent-notes.txt")); err == nil {
		t.Fatal("beta workspace leaked alpha file")
	}

	betaWS := filepath.Join(s.cfg.DataDir, "tenants", "beta", "workspace")
	if _, err := sandbox.JailPath(betaWS, note); err == nil {
		t.Fatal("beta jail allowed alpha absolute path")
	}

	rec = do(t, s, http.MethodGet, "/v1/runs/"+created.ID, keyB, nil)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("beta fetch alpha run: %d %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("secret-from-alpha")) || bytes.Contains(rec.Body.Bytes(), raw) {
		t.Fatal("leaked alpha run contents to beta")
	}

	rec = do(t, s, http.MethodGet, "/v1/runs/"+created.ID+"/events", keyB, nil)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("beta events alpha run: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, http.MethodGet, "/v1/runs/"+created.ID, keyA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("alpha fetch own run: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConcurrencyQuota(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	s := testServer(t, func(c *Config) {
		c.MaxConcurrent = 1
		c.NewModel = func(name string) (model.Model, error) {
			return &gateModel{started: started, once: &once, release: release}, nil
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	createTenant(t, s, "acme", "Acme")
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})

	rec := do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "hold", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first run: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
	}

	rec = do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "should-429", "model": "mock"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body.String())
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec = do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "after", "model": "mock"})
		if rec.Code == http.StatusAccepted {
			return
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("after release: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("quota did not release, last=%d %s", rec.Code, rec.Body.String())
}

func TestAdminListTenants(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	rec := do(t, s, http.MethodGet, "/v1/admin/tenants", testAdmin, nil)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

type gateModel struct {
	started chan struct{}
	once    *sync.Once
	release chan struct{}
}

func (g *gateModel) Name() string { return "mock" }

func (g *gateModel) Complete(ctx context.Context, _ model.CompleteRequest) (model.CompleteResponse, error) {
	g.once.Do(func() {
		close(g.started)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	})
	return model.CompleteResponse{
		Message: model.Message{Role: "assistant", Content: "ok"},
	}, nil
}

func waitRun(t *testing.T, s *Server, token, id string) RunRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var rec *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		rec = do(t, s, http.MethodGet, "/v1/runs/"+id, token, nil)
		if rec.Code != 200 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var out RunRecord
		decode(t, rec, &out)
		if out.Status == statusCompleted || out.Status == statusFailed {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish: %s", id, rec.Body.String())
	return RunRecord{}
}
