// Package daemon is the multi-tenant HTTP process (agentloopd).
// Isolation is the existing process jail: each tenant gets its own
// workspace under --data; there is no Docker.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/YaoLang/agentloop/internal/agent"
	"github.com/YaoLang/agentloop/internal/auth"
	"github.com/YaoLang/agentloop/internal/memory"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/tools"
)

const defaultMaxConcurrent = 8

// Config is process-wide daemon settings.
type Config struct {
	DataDir       string
	DefaultModel  string
	MaxConcurrent int
	RunTimeout    time.Duration
	ToolTimeout   time.Duration
	AdminKey      string
	JWTSecret     string
	Auth          auth.Chain
	NewModel      func(name string) (model.Model, error)
	// ExtraTools, if set, is called when a run's registry is built.
	// Returned tools are registered after builtins (Options.Extra).
	ExtraTools func(opt tools.Options) []*tools.Tool
}

// Server is the HTTP API.
type Server struct {
	cfg     Config
	store   *Store
	keys    *auth.FileKeyStore
	secrets *SecretStore
	auth    auth.Chain
	mux     *http.ServeMux
	quota   *quota
	newMod  func(name string) (model.Model, error)
}

type quota struct {
	mu      sync.Mutex
	running map[string]int
	limit   int
}

func (q *quota) tryAcquire(tenant string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running[tenant] >= q.limit {
		return false
	}
	if q.running == nil {
		q.running = map[string]int{}
	}
	q.running[tenant]++
	return true
}

func (q *quota) release(tenant string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running[tenant] > 0 {
		q.running[tenant]--
	}
}

func (q *quota) runningCount(tenant string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running[tenant]
}

// New builds a Server. DataDir is created if missing.
func New(cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "mock"
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 2 * time.Minute
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 5 * time.Second
	}
	st, err := newStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ks, err := auth.NewFileKeyStore(st.keysPath())
	if err != nil {
		return nil, err
	}
	chain := cfg.Auth
	if chain == nil {
		chain = auth.Chain{
			auth.NewAdmin(cfg.AdminKey),
			auth.NewAPIKey(ks),
			auth.NewJWT(cfg.JWTSecret),
		}
	}
	newMod := cfg.NewModel
	if newMod == nil {
		newMod = defaultNewModel
	}
	sec, err := newSecretStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		store:   st,
		keys:    ks,
		secrets: sec,
		auth:    chain,
		mux:     http.NewServeMux(),
		quota:   &quota{running: map[string]int{}, limit: cfg.MaxConcurrent},
		newMod:  newMod,
	}
	s.routes()
	return s, nil
}

func defaultNewModel(name string) (model.Model, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mock", "":
		return model.NewMock(), nil
	case "openai":
		return model.FromEnv()
	default:
		return nil, fmt.Errorf("unknown model %q (want mock|openai)", name)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /v1/runs", s.withAuth(s.requireScope(auth.ScopeRunsWrite, s.handleCreateRun)))
	s.mux.HandleFunc("GET /v1/runs/{id}", s.withAuth(s.requireRunRead(s.handleGetRun)))
	s.mux.HandleFunc("GET /v1/runs/{id}/events", s.withAuth(s.requireRunRead(s.handleRunEvents)))
	s.mux.HandleFunc("POST /v1/admin/tenants", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handleCreateTenant)))
	s.mux.HandleFunc("GET /v1/admin/tenants", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handleListTenants)))
	s.mux.HandleFunc("POST /v1/admin/keys", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handleMintKey)))
	s.mux.HandleFunc("PUT /v1/admin/tenants/{id}/secrets", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handlePutSecret)))
	s.mux.HandleFunc("GET /v1/admin/tenants/{id}/secrets", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handleListSecrets)))
	s.mux.HandleFunc("DELETE /v1/admin/tenants/{id}/secrets/{name}", s.withAuth(s.requireScope(auth.ScopeAdmin, s.handleDeleteSecret)))
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

func (s *Server) requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || !p.HasScope(scope) {
			writeErr(w, http.StatusForbidden, "forbidden", "insufficient scope")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireRunRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || !(p.HasScope(auth.ScopeRunsWrite) || p.HasScope(auth.ScopeRunsRead)) {
			writeErr(w, http.StatusForbidden, "forbidden", "insufficient scope")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type createTenantReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	meta, err := s.store.createTenant(req.ID, req.Name)
	if err != nil {
		if errors.Is(err, errExists) {
			writeErr(w, http.StatusConflict, "conflict", "tenant already exists")
			return
		}
		if !auth.SafeID(req.ID) || req.ID == auth.AdminTenant {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "create tenant failed")
		return
	}
	writeJSON(w, http.StatusCreated, meta)
}

func (s *Server) handleListTenants(w http.ResponseWriter, _ *http.Request) {
	list, err := s.store.listTenants()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "list tenants failed")
		return
	}
	if list == nil {
		list = []TenantMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": list})
}

type mintKeyReq struct {
	TenantID string   `json:"tenant_id"`
	Scopes   []string `json:"scopes"`
}

func (s *Server) handleMintKey(w http.ResponseWriter, r *http.Request) {
	var req mintKeyReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if _, err := s.store.getTenant(req.TenantID); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	secret, rec, err := s.keys.Mint(req.TenantID, req.Scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        rec.ID,
		"secret":    secret,
		"tenant_id": rec.TenantID,
		"scopes":    rec.Scopes,
	})
}

type putSecretReq struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.getTenant(id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	var req putSecretReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.secrets.Put(id, req.Name, req.Value); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name})
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.getTenant(id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	names, err := s.secrets.ListNames(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if _, err := s.store.getTenant(id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	if err := s.secrets.Delete(id, name); err != nil {
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createRunReq struct {
	Goal  string `json:"goal"`
	Model string `json:"model"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req createRunReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Goal = strings.TrimSpace(req.Goal)
	if req.Goal == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "goal is required")
		return
	}
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = s.cfg.DefaultModel
	}
	switch strings.ToLower(modelName) {
	case "mock", "openai":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "model must be mock or openai")
		return
	}
	if _, err := s.store.getTenant(p.TenantID); err != nil && p.TenantID != auth.AdminTenant {
		writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	if !s.quota.tryAcquire(p.TenantID) {
		writeErr(w, http.StatusTooManyRequests, "too_many_requests", "tenant concurrency limit reached")
		return
	}
	rec, err := s.store.createRun(p.TenantID, req.Goal, strings.ToLower(modelName))
	if err != nil {
		s.quota.release(p.TenantID)
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "create run failed")
		return
	}
	if err := s.store.appendEvent(rec.TenantID, rec.ID, Event{Type: "status", Status: statusQueued}); err != nil {
		log.Printf("agentloopd: event write tenant=%s run=%s: %v", rec.TenantID, rec.ID, err)
	}
	go s.executeRun(rec, p)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": rec.ID, "status": rec.Status})
}

var httpCatalogLogged sync.Map // tenant id → logged invalid catalog

func (s *Server) attachHTTPCatalog(opt *tools.Options, tenantID string) {
	path := s.store.HTTPCatalogPath(tenantID)
	cat, err := tools.LoadHTTPCatalog(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		if _, loaded := httpCatalogLogged.LoadOrStore(tenantID, true); !loaded {
			log.Printf("agentloopd: tenant=%s http catalog: %v (http_call not registered)", tenantID, err)
		}
		return
	}
	if cat != nil && len(cat.Endpoints) > 0 {
		opt.HTTP = cat
	}
}

func (s *Server) executeRun(rec RunRecord, p auth.Principal) {
	defer s.quota.release(rec.TenantID)
	defer func() {
		if x := recover(); x != nil {
			rec.Status = statusFailed
			rec.Error = "internal panic"
			_ = s.store.putRun(rec)
			_ = s.store.appendEvent(rec.TenantID, rec.ID, Event{Type: "status", Status: statusFailed, Error: rec.Error})
		}
	}()

	rec.Status = statusRunning
	if err := s.store.putRun(rec); err != nil {
		log.Printf("agentloopd: status write tenant=%s run=%s: %v", rec.TenantID, rec.ID, err)
	}
	_ = s.store.appendEvent(rec.TenantID, rec.ID, Event{Type: "status", Status: statusRunning})

	m, err := s.newMod(rec.Model)
	if err != nil {
		rec.Status = statusFailed
		rec.Error = err.Error()
		_ = s.store.putRun(rec)
		_ = s.store.appendEvent(rec.TenantID, rec.ID, Event{Type: "status", Status: statusFailed, Error: rec.Error})
		return
	}
	ws := s.store.workspace(rec.TenantID)
	if err := os.MkdirAll(ws, 0o750); err != nil {
		rec.Status = statusFailed
		rec.Error = err.Error()
		_ = s.store.putRun(rec)
		return
	}
	mem, err := memory.Open(ws)
	if err != nil {
		rec.Status = statusFailed
		rec.Error = err.Error()
		_ = s.store.putRun(rec)
		return
	}
	opt := tools.Options{
		Workspace:   ws,
		Memory:      mem,
		ToolTimeout: s.cfg.ToolTimeout,
	}
	s.attachHTTPCatalog(&opt, rec.TenantID)
	if s.cfg.ExtraTools != nil {
		opt.Extra = s.cfg.ExtraTools(opt)
	}
	reg := tools.Default(opt)
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RunTimeout)
	defer cancel()
	ctx = auth.WithPrincipal(ctx, p)
	tenantID := p.TenantID
	secrets := s.secrets
	ctx = tools.WithRuntime(ctx, tools.Runtime{
		TenantID: p.TenantID,
		Subject:  p.Subject,
		Scopes:   append([]string(nil), p.Scopes...),
		Secret: func(name string) (string, bool) {
			if secrets == nil {
				return "", false
			}
			return secrets.Get(tenantID, name)
		},
	})
	res, err := agent.Run(ctx, agent.Config{
		Workspace:   ws,
		Goal:        rec.Goal,
		Model:       m,
		Registry:    reg,
		Memory:      mem,
		Timeout:     s.cfg.RunTimeout,
		ToolTimeout: s.cfg.ToolTimeout,
		RunID:       rec.ID,
	})
	if err != nil && res == nil {
		rec.Status = statusFailed
		rec.Error = err.Error()
		_ = s.store.putRun(rec)
		_ = s.store.appendEvent(rec.TenantID, rec.ID, Event{Type: "status", Status: statusFailed, Error: rec.Error})
		return
	}
	if res != nil {
		rec.Final = res.Final
		rec.StopReason = res.StopReason
		rec.Steps = res.Steps
	}
	if err != nil {
		rec.Status = statusFailed
		rec.Error = err.Error()
	} else {
		rec.Status = statusCompleted
	}
	_ = s.store.putRun(rec)
	_ = s.store.appendEvent(rec.TenantID, rec.ID, Event{
		Type:       "status",
		Status:     rec.Status,
		Final:      rec.Final,
		Error:      rec.Error,
		StopReason: rec.StopReason,
	})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	rec, err := s.store.getRun(p.TenantID, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if _, err := s.store.getRun(p.TenantID, id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	seen := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		evs, err := s.store.readEvents(p.TenantID, id)
		if err != nil {
			return
		}
		for i := seen; i < len(evs); i++ {
			raw, _ := json.Marshal(evs[i])
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			flush()
		}
		seen = len(evs)
		rec, err := s.store.getRun(p.TenantID, id)
		if err != nil {
			return
		}
		if rec.Status == statusCompleted || rec.Status == statusFailed {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, code, errBody{Error: kind, Message: msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid json")
	}
	return nil
}
