package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/YaoLang/agentloop/internal/auth"
)

const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// TenantMeta is data/tenants/{id}/meta.json.
type TenantMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// RunRecord is persisted under data/tenants/{tid}/runs/{rid}/status.json.
type RunRecord struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Status     string    `json:"status"`
	Goal       string    `json:"goal"`
	Model      string    `json:"model"`
	Final      string    `json:"final,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	Steps      int       `json:"steps,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Event is one SSE payload persisted as JSONL.
type Event struct {
	Type       string    `json:"type"`
	Status     string    `json:"status,omitempty"`
	Final      string    `json:"final,omitempty"`
	Error      string    `json:"error,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	TS         time.Time `json:"ts"`
}

// Store is the on-disk multi-tenant layout under --data.
type Store struct {
	root string
	mu   sync.Mutex
}

func newStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "tenants"), 0o750); err != nil {
		return nil, err
	}
	return &Store{root: abs}, nil
}

func (s *Store) keysPath() string { return filepath.Join(s.root, "keys.json") }

func (s *Store) tenantDir(id string) string {
	return filepath.Join(s.root, "tenants", id)
}

func (s *Store) workspace(id string) string {
	return filepath.Join(s.tenantDir(id), "workspace")
}

// HTTPCatalogPath is data/tenants/{id}/http.json — next to meta.json,
// not inside workspace/ (write_file cannot rewrite the allowlist).
func (s *Store) HTTPCatalogPath(id string) string {
	return filepath.Join(s.tenantDir(id), "http.json")
}

// ContextCatalogPath is data/tenants/{id}/context.json — next to meta.json,
// not inside workspace/ (write_file cannot rewrite inject rules).
func (s *Store) ContextCatalogPath(id string) string {
	return filepath.Join(s.tenantDir(id), "context.json")
}

func (s *Store) runDir(tenantID, runID string) string {
	return filepath.Join(s.tenantDir(tenantID), "runs", runID)
}

func (s *Store) createTenant(id, name string) (TenantMeta, error) {
	if !auth.SafeID(id) {
		return TenantMeta{}, fmt.Errorf("invalid tenant id")
	}
	if id == auth.AdminTenant {
		return TenantMeta{}, fmt.Errorf("reserved tenant id")
	}
	if name == "" {
		name = id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.tenantDir(id)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		return TenantMeta{}, errExists
	}
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o750); err != nil {
		return TenantMeta{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o750); err != nil {
		return TenantMeta{}, err
	}
	meta := TenantMeta{ID: id, Name: name, CreatedAt: time.Now().UTC()}
	if err := writeFileJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return TenantMeta{}, err
	}
	return meta, nil
}

func (s *Store) getTenant(id string) (TenantMeta, error) {
	if !auth.SafeID(id) {
		return TenantMeta{}, errNotFound
	}
	var meta TenantMeta
	if err := readJSON(filepath.Join(s.tenantDir(id), "meta.json"), &meta); err != nil {
		if os.IsNotExist(err) {
			return TenantMeta{}, errNotFound
		}
		return TenantMeta{}, err
	}
	return meta, nil
}

func (s *Store) listTenants() ([]TenantMeta, error) {
	ents, err := os.ReadDir(filepath.Join(s.root, "tenants"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TenantMeta
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var meta TenantMeta
		if err := readJSON(filepath.Join(s.tenantDir(e.Name()), "meta.json"), &meta); err != nil {
			continue
		}
		out = append(out, meta)
	}
	return out, nil
}

func (s *Store) createRun(tenantID, goal, modelName string) (RunRecord, error) {
	if !auth.SafeID(tenantID) {
		return RunRecord{}, errNotFound
	}
	if _, err := s.getTenant(tenantID); err != nil {
		// allow the admin tenant to exist implicitly
		if tenantID != auth.AdminTenant {
			return RunRecord{}, err
		}
		if err := os.MkdirAll(s.workspace(tenantID), 0o750); err != nil {
			return RunRecord{}, err
		}
	}
	id, err := newID("run_")
	if err != nil {
		return RunRecord{}, err
	}
	now := time.Now().UTC()
	rec := RunRecord{
		ID:        id,
		TenantID:  tenantID,
		Status:    statusQueued,
		Goal:      goal,
		Model:     modelName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	dir := s.runDir(tenantID, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return RunRecord{}, err
	}
	if err := os.MkdirAll(s.workspace(tenantID), 0o750); err != nil {
		return RunRecord{}, err
	}
	if err := writeFileJSON(filepath.Join(dir, "status.json"), rec); err != nil {
		return RunRecord{}, err
	}
	return rec, nil
}

func (s *Store) getRun(tenantID, runID string) (RunRecord, error) {
	if !auth.SafeID(tenantID) || !auth.SafeID(runID) {
		return RunRecord{}, errNotFound
	}
	var rec RunRecord
	if err := readJSON(filepath.Join(s.runDir(tenantID, runID), "status.json"), &rec); err != nil {
		if os.IsNotExist(err) {
			return RunRecord{}, errNotFound
		}
		return RunRecord{}, err
	}
	return rec, nil
}

func (s *Store) putRun(rec RunRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	return writeFileJSON(filepath.Join(s.runDir(rec.TenantID, rec.ID), "status.json"), rec)
}

func (s *Store) appendEvent(tenantID, runID string, ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	dir := s.runDir(tenantID, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return err
}

func (s *Store) readEvents(tenantID, runID string) ([]Event, error) {
	path := filepath.Join(s.runDir(tenantID, runID), "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

func writeFileJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	mode := os.FileMode(0o640)
	if filepath.Base(path) == "keys.json" {
		mode = 0o600
	}
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

var (
	errNotFound = fmt.Errorf("not found")
	errExists   = fmt.Errorf("already exists")
)
