// Package memory provides session-scoped key/value storage and an
// append-only long-term notes log on disk under the workspace.
package memory

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	ScopeSession  = "session"
	ScopeLongTerm = "longterm"
)

// Note is one append-only long-term record.
type Note struct {
	TS    time.Time `json:"ts"`
	Key   string    `json:"key"`
	Value string    `json:"value"`
}

// Store holds session memory in process and long-term notes on disk.
type Store struct {
	session map[string]string
	path    string
	mu      sync.Mutex
}

// Open prepares .agentloop/ under workspace and the memory.jsonl log.
func Open(workspace string) (*Store, error) {
	dir := filepath.Join(workspace, ".agentloop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{
		session: map[string]string{},
		path:    filepath.Join(dir, "memory.jsonl"),
	}, nil
}

// Write stores a value. scope is "session" (default) or "longterm".
func (s *Store) Write(scope, key, value string) error {
	if key == "" {
		return fmt.Errorf("memory: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if scope == "" {
		scope = ScopeSession
	}
	switch scope {
	case ScopeSession:
		s.session[key] = value
		return nil
	case ScopeLongTerm:
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		rec := Note{TS: time.Now().UTC(), Key: key, Value: value}
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("memory: unknown scope %q (want session|longterm)", scope)
	}
}

// Read returns the value for key. Long-term reads scan the log; the
// last write for that key wins.
func (s *Store) Read(scope, key string) (string, bool, error) {
	if key == "" {
		return "", false, fmt.Errorf("memory: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if scope == "" {
		scope = ScopeSession
	}
	switch scope {
	case ScopeSession:
		v, ok := s.session[key]
		return v, ok, nil
	case ScopeLongTerm:
		f, err := os.Open(s.path)
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		defer f.Close()
		var last string
		found := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var n Note
			if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
				continue
			}
			if n.Key == key {
				last = n.Value
				found = true
			}
		}
		return last, found, sc.Err()
	default:
		return "", false, fmt.Errorf("memory: unknown scope %q", scope)
	}
}

// SessionSnapshot copies session memory (for evals / persistence).
func (s *Store) SessionSnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.session))
	for k, v := range s.session {
		out[k] = v
	}
	return out
}

// Path is the long-term JSONL file.
func (s *Store) Path() string { return s.path }
