package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const apiKeyPrefix = "alk_"

// KeyRecord is the on-disk form of an API key. The secret is never stored;
// only a SHA-256 hex hash is kept.
type KeyRecord struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	TenantID  string    `json:"tenant_id"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}

type keyFile struct {
	Keys []KeyRecord `json:"keys"`
}

// FileKeyStore persists API keys as SHA-256 hashes in a JSON file.
type FileKeyStore struct {
	path string
	mu   sync.Mutex
}

// NewFileKeyStore opens (or creates) path.
func NewFileKeyStore(path string) (*FileKeyStore, error) {
	s := &FileKeyStore{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.save(keyFile{Keys: []KeyRecord{}}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *FileKeyStore) load() (keyFile, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return keyFile{}, nil
		}
		return keyFile{}, err
	}
	if len(raw) == 0 {
		return keyFile{}, nil
	}
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return keyFile{}, err
	}
	return kf, nil
}

func (s *FileKeyStore) save(kf keyFile) error {
	if kf.Keys == nil {
		kf.Keys = []KeyRecord{}
	}
	raw, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// FindByHash looks up a SHA-256 digest with a timing-safe compare against
// every stored hash. The presented secret is never logged.
func (s *FileKeyStore) FindByHash(sum []byte) (KeyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kf, err := s.load()
	if err != nil {
		return KeyRecord{}, false
	}
	var found KeyRecord
	matched := 0
	for _, rec := range kf.Keys {
		have, err := hex.DecodeString(rec.Hash)
		if err != nil || len(have) != len(sum) {
			continue
		}
		if subtle.ConstantTimeCompare(sum, have) == 1 {
			found = rec
			matched = 1
		}
	}
	return found, matched == 1
}

// Mint creates a new alk_… secret, stores only its SHA-256 hash, and
// returns the secret once. The secret is never written to disk.
func (s *FileKeyStore) Mint(tenantID string, scopes []string) (secret string, rec KeyRecord, err error) {
	if !SafeID(tenantID) {
		return "", KeyRecord{}, fmt.Errorf("auth: invalid tenant id")
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeRunsWrite}
	}
	var secretBytes [32]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return "", KeyRecord{}, err
	}
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", KeyRecord{}, err
	}
	secret = apiKeyPrefix + hex.EncodeToString(secretBytes[:])
	sum := sha256.Sum256([]byte(secret))
	rec = KeyRecord{
		ID:        "key_" + hex.EncodeToString(idBytes[:]),
		Hash:      hex.EncodeToString(sum[:]),
		TenantID:  tenantID,
		Scopes:    append([]string(nil), scopes...),
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kf, err := s.load()
	if err != nil {
		return "", KeyRecord{}, err
	}
	kf.Keys = append(kf.Keys, rec)
	if err := s.save(kf); err != nil {
		return "", KeyRecord{}, err
	}
	return secret, rec, nil
}

// APIKey authenticates Authorization: Bearer alk_… against a FileKeyStore.
type APIKey struct {
	Store *FileKeyStore
}

// NewAPIKey wraps a key store.
func NewAPIKey(store *FileKeyStore) *APIKey { return &APIKey{Store: store} }

func (a *APIKey) Name() string { return "apikey" }

func (a *APIKey) Authenticate(r *http.Request) (Principal, error) {
	if a == nil || a.Store == nil {
		return Principal{}, ErrSkip
	}
	tok, ok := BearerToken(r)
	if !ok || !strings.HasPrefix(tok, apiKeyPrefix) {
		return Principal{}, ErrSkip
	}
	sum := sha256.Sum256([]byte(tok))
	rec, ok := a.Store.FindByHash(sum[:])
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		TenantID: rec.TenantID,
		Subject:  rec.ID,
		Scopes:   rec.Scopes,
	}, nil
}
