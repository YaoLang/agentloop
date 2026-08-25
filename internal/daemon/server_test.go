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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/auth"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/sandbox"
	"github.com/YaoLang/agentloop/internal/tools"
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

func TestAdminSecretsSetListNoValuesNonAdminForbidden(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	const secret = "gho_xxx_must_not_list"

	rec := do(t, s, http.MethodPut, "/v1/admin/tenants/acme/secrets", testAdmin, map[string]string{
		"name":  "github",
		"value": secret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put secret: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("PUT response leaked secret value")
	}

	rec = do(t, s, http.MethodGet, "/v1/admin/tenants/acme/secrets", testAdmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list secrets: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("list leaked secret value")
	}
	var listed struct {
		Names []string `json:"names"`
	}
	decode(t, rec, &listed)
	if len(listed.Names) != 1 || listed.Names[0] != "github" {
		t.Fatalf("names=%v", listed.Names)
	}

	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec = do(t, s, http.MethodPut, "/v1/admin/tenants/acme/secrets", key, map[string]string{
		"name":  "github",
		"value": "nope",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodGet, "/v1/admin/tenants/acme/secrets", key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodDelete, "/v1/admin/tenants/acme/secrets/github", key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin DELETE: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, http.MethodDelete, "/v1/admin/tenants/acme/secrets/github", testAdmin, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodGet, "/v1/admin/tenants/acme/secrets", testAdmin, nil)
	decode(t, rec, &listed)
	if len(listed.Names) != 0 {
		t.Fatalf("after delete names=%v", listed.Names)
	}
}

func TestTenantASecretNotVisibleToTenantBRuntime(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "alpha", "Alpha")
	createTenant(t, s, "beta", "Beta")
	const secret = "gho_xxx_alpha_only"
	rec := do(t, s, http.MethodPut, "/v1/admin/tenants/alpha/secrets", testAdmin, map[string]string{
		"name":  "github",
		"value": secret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	got, ok := s.secrets.Get("alpha", "github")
	if !ok || got != secret {
		t.Fatalf("alpha Get: ok=%v", ok)
	}
	if _, ok := s.secrets.Get("beta", "github"); ok {
		t.Fatal("beta must not see alpha secret via Get")
	}

	reg := tools.NewRegistry()
	reg.Register(tools.HasSecretTool())

	ctxA := tools.WithRuntime(context.Background(), tools.Runtime{
		TenantID: "alpha",
		Secret: func(name string) (string, bool) {
			return s.secrets.Get("alpha", name)
		},
	})
	out, err := reg.Call(ctxA, "has_secret", `{"name":"github"}`)
	if err != nil || out != "present" {
		t.Fatalf("alpha has_secret: out=%q err=%v", out, err)
	}
	if strings.Contains(out, secret) {
		t.Fatal("value leaked")
	}

	ctxB := tools.WithRuntime(context.Background(), tools.Runtime{
		TenantID: "beta",
		Secret: func(name string) (string, bool) {
			return s.secrets.Get("beta", name)
		},
	})
	out, err = reg.Call(ctxB, "has_secret", `{"name":"github"}`)
	if err != nil || out != "absent" {
		t.Fatalf("beta has_secret: out=%q err=%v", out, err)
	}
}

func TestExtraToolsOnDaemonConfigAdvertisedAndCallable(t *testing.T) {
	sawPing := false
	s := testServer(t, func(c *Config) {
		c.ExtraTools = func(opt tools.Options) []*tools.Tool {
			return []*tools.Tool{{
				Name:        "ping",
				Description: "return pong",
				Handler: func(ctx context.Context, argsJSON string) (string, error) {
					return "pong", nil
				},
			}}
		}
		c.NewModel = func(name string) (model.Model, error) {
			return &extraToolModel{sawPing: &sawPing}, nil
		}
	})
	createTenant(t, s, "acme", "Acme")
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "ping extra", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	got := waitRun(t, s, key, created.ID)
	if got.Status != statusCompleted {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
	if !sawPing {
		t.Fatal("ping was not advertised to the model")
	}
	if got.Final != "pong-seen" {
		t.Fatalf("final=%q (extra tool not callable?)", got.Final)
	}
}

type extraToolModel struct {
	sawPing *bool
	step    int
}

func (m *extraToolModel) Name() string { return "mock" }

func (m *extraToolModel) Complete(_ context.Context, req model.CompleteRequest) (model.CompleteResponse, error) {
	for _, spec := range req.Tools {
		if spec.Name == "ping" && m.sawPing != nil {
			*m.sawPing = true
		}
	}
	m.step++
	if m.step == 1 {
		return model.CompleteResponse{
			Message: model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					ID:        "call_ping",
					Name:      "ping",
					Arguments: "{}",
				}},
			},
		}, nil
	}
	return model.CompleteResponse{
		Message: model.Message{Role: "assistant", Content: "pong-seen"},
	}, nil
}

func TestSecretsFileMode0600(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	if err := s.secrets.Put("acme", "github", "gho_mode"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.cfg.DataDir, "tenants", "acme", "secrets.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("github")) {
		t.Fatal("name missing on disk")
	}
}

func TestWhoamiOnDaemonRunSeesPrincipal(t *testing.T) {
	s := testServer(t, func(c *Config) {
		c.NewModel = func(name string) (model.Model, error) {
			return model.NewScripted([]model.Step{
				{Tool: "whoami", Args: map[string]any{}},
				{Content: "ok"},
			}), nil
		}
	})
	createTenant(t, s, "acme", "Acme")
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "who am i", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	got := waitRun(t, s, key, created.ID)
	if got.Status != statusCompleted {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
	// session is on disk under the tenant workspace
	sessPath := filepath.Join(s.cfg.DataDir, "tenants", "acme", "workspace", ".agentloop", "session.json")
	raw, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`tenant_id`)) || !bytes.Contains(raw, []byte(`acme`)) {
		t.Fatalf("whoami observation missing tenant: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"name": "whoami"`)) && !bytes.Contains(raw, []byte(`"name":"whoami"`)) {
		t.Fatalf("whoami tool trace missing: %s", raw)
	}
	if bytes.Contains(raw, []byte("gho_")) {
		t.Fatal("secret-like value in session")
	}
}

func TestHTTPCatalogPathAndMissingFile(t *testing.T) {
	s := testServer(t)
	createTenant(t, s, "acme", "Acme")
	path := s.store.HTTPCatalogPath("acme")
	wantSuffix := filepath.Join("tenants", "acme", "http.json")
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("path=%s want suffix %s", path, wantSuffix)
	}
	if strings.Contains(path, "workspace") {
		t.Fatalf("catalog must not live under workspace: %s", path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing catalog should not exist: %v", err)
	}
	key := mintKey(t, s, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, s, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "hello", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run without catalog: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	got := waitRun(t, s, key, created.ID)
	if got.Status != statusCompleted && got.Status != statusFailed {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
}

func TestHTTPCatalogAdvertisedWhenPresent(t *testing.T) {
	saw := false
	srv := testServer(t, func(c *Config) {
		c.NewModel = func(name string) (model.Model, error) {
			return &httpCatalogProbe{saw: &saw}, nil
		}
	})
	createTenant(t, srv, "acme", "Acme")
	cat := map[string]any{
		"base_url":    "https://api.example.com",
		"allow_hosts": []string{"api.example.com"},
		"endpoints": []map[string]string{
			{"id": "list_orders", "method": "GET", "path": "/v1/orders", "description": "List orders"},
		},
	}
	raw, err := json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.store.HTTPCatalogPath("acme"), raw, 0o640); err != nil {
		t.Fatal(err)
	}
	key := mintKey(t, srv, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, srv, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "list", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	got := waitRun(t, srv, key, created.ID)
	if got.Status != statusCompleted {
		t.Fatalf("status=%s err=%s", got.Status, got.Error)
	}
	if !saw {
		t.Fatal("http_call was not advertised to the model")
	}
}

func TestInvalidHTTPCatalogDoesNotRegister(t *testing.T) {
	saw := false
	srv := testServer(t, func(c *Config) {
		c.NewModel = func(name string) (model.Model, error) {
			return &httpCatalogProbe{saw: &saw}, nil
		}
	})
	createTenant(t, srv, "acme", "Acme")
	if err := os.WriteFile(srv.store.HTTPCatalogPath("acme"), []byte(`{not-json`), 0o640); err != nil {
		t.Fatal(err)
	}
	key := mintKey(t, srv, "acme", []string{auth.ScopeRunsWrite})
	rec := do(t, srv, http.MethodPost, "/v1/runs", key, map[string]string{"goal": "x", "model": "mock"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	got := waitRun(t, srv, key, created.ID)
	if got.Status != statusCompleted && got.Status != statusFailed {
		t.Fatalf("invalid catalog must not break the run: %s %s", got.Status, got.Error)
	}
	if saw {
		t.Fatal("http_call registered from invalid catalog")
	}
}

type httpCatalogProbe struct {
	saw *bool
}

func (m *httpCatalogProbe) Name() string { return "mock" }

func (m *httpCatalogProbe) Complete(_ context.Context, req model.CompleteRequest) (model.CompleteResponse, error) {
	for _, spec := range req.Tools {
		if spec.Name == "http_call" && m.saw != nil {
			*m.saw = true
		}
	}
	return model.CompleteResponse{
		Message: model.Message{Role: "assistant", Content: "ok"},
	}, nil
}
