// Package session is the per-run conversation: messages, tool traces,
// and the workspace directory they live under.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/YaoLang/agentloop/internal/model"
)

// ToolTrace is one observed tool invocation.
type ToolTrace struct {
	Step      int           `json:"step"`
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Args      string        `json:"args"`
	Result    string        `json:"result"`
	Error     string        `json:"error,omitempty"`
	Latency   time.Duration `json:"latency"`
	Jail      bool          `json:"jail,omitempty"`
	Timeout   bool          `json:"timeout,omitempty"`
	SchemaErr bool          `json:"schema_err,omitempty"`
}

// Session is persisted under workspace/.agentloop/session.json.
type Session struct {
	ID        string          `json:"id"`
	Goal      string          `json:"goal"`
	Workspace string          `json:"workspace"`
	Messages  []model.Message `json:"messages"`
	ToolLog   []ToolTrace     `json:"tool_log"`
	CreatedAt time.Time       `json:"created_at"`
}

// New starts an empty session.
func New(id, workspace, goal string) *Session {
	return &Session{
		ID:        id,
		Goal:      goal,
		Workspace: workspace,
		CreatedAt: time.Now().UTC(),
	}
}

func (s *Session) Add(m model.Message) { s.Messages = append(s.Messages, m) }

func (s *Session) AddTool(t ToolTrace) { s.ToolLog = append(s.ToolLog, t) }

// Dir is workspace/.agentloop.
func (s *Session) Dir() string { return filepath.Join(s.Workspace, ".agentloop") }

// Save writes session.json.
func (s *Session) Save() error {
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir(), "session.json"), raw, 0o644)
}

// LoadSession reads a previously saved session.
func LoadSession(workspace string) (*Session, error) {
	raw, err := os.ReadFile(filepath.Join(workspace, ".agentloop", "session.json"))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
