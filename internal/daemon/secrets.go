package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/YaoLang/agentloop/internal/auth"
)

// SecretStore is the per-tenant named-secret file store.
// Layout: data/tenants/{id}/secrets.json mode 0600
//
//	{"secrets":[{"name":"github","value":"..."}]}
//
// Get is in-process only — HTTP list endpoints return names, never values.
// Never log secret values.
type SecretStore struct {
	root string
	mu   sync.Mutex
}

type secretFile struct {
	Secrets []secretEntry `json:"secrets"`
}

type secretEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func newSecretStore(root string) (*SecretStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &SecretStore{root: abs}, nil
}

func (s *SecretStore) path(tenant string) string {
	return filepath.Join(s.root, "tenants", tenant, "secrets.json")
}

func (s *SecretStore) load(tenant string) (secretFile, error) {
	raw, err := os.ReadFile(s.path(tenant))
	if err != nil {
		if os.IsNotExist(err) {
			return secretFile{Secrets: []secretEntry{}}, nil
		}
		return secretFile{}, err
	}
	if len(raw) == 0 {
		return secretFile{Secrets: []secretEntry{}}, nil
	}
	var f secretFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return secretFile{}, err
	}
	if f.Secrets == nil {
		f.Secrets = []secretEntry{}
	}
	return f, nil
}

func (s *SecretStore) save(tenant string, f secretFile) error {
	if f.Secrets == nil {
		f.Secrets = []secretEntry{}
	}
	dir := filepath.Dir(s.path(tenant))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(tenant)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

// Put upserts a named secret for tenant. Names follow auth.SafeID rules.
func (s *SecretStore) Put(tenant, name, value string) error {
	if !auth.SafeID(tenant) {
		return fmt.Errorf("invalid tenant id")
	}
	if !auth.SafeID(name) {
		return fmt.Errorf("invalid secret name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load(tenant)
	if err != nil {
		return err
	}
	found := false
	for i, e := range f.Secrets {
		if e.Name == name {
			f.Secrets[i].Value = value
			found = true
			break
		}
	}
	if !found {
		f.Secrets = append(f.Secrets, secretEntry{Name: name, Value: value})
	}
	return s.save(tenant, f)
}

// Get returns a secret value in-process. Missing tenant/name → ok=false.
// Never log the value.
func (s *SecretStore) Get(tenant, name string) (string, bool) {
	if s == nil || !auth.SafeID(tenant) || !auth.SafeID(name) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load(tenant)
	if err != nil {
		return "", false
	}
	for _, e := range f.Secrets {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// ListNames returns secret names for tenant, never values.
func (s *SecretStore) ListNames(tenant string) ([]string, error) {
	if !auth.SafeID(tenant) {
		return nil, fmt.Errorf("invalid tenant id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load(tenant)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f.Secrets))
	for _, e := range f.Secrets {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names, nil
}

// Delete removes a named secret. Missing name → errNotFound.
func (s *SecretStore) Delete(tenant, name string) error {
	if !auth.SafeID(tenant) || !auth.SafeID(name) {
		return errNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load(tenant)
	if err != nil {
		return err
	}
	kept := make([]secretEntry, 0, len(f.Secrets))
	found := false
	for _, e := range f.Secrets {
		if e.Name == name {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return errNotFound
	}
	f.Secrets = kept
	return s.save(tenant, f)
}
