// Package trace writes a JSONL event log per agent run.
package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one JSONL line. Type is model_call | model_retry | tool_call | budget | final.
type Event struct {
	TS         time.Time      `json:"ts"`
	Type       string         `json:"type"`
	RunID      string         `json:"run_id,omitempty"`
	Step       int            `json:"step,omitempty"`
	Model      string         `json:"model,omitempty"`
	Name       string         `json:"name,omitempty"`
	Args       any            `json:"args,omitempty"`
	OK         *bool          `json:"ok,omitempty"`
	Error      string         `json:"error,omitempty"`
	Content    string         `json:"content,omitempty"`
	LatencyMS  int64          `json:"latency_ms,omitempty"`
	Tokens     map[string]int `json:"tokens,omitempty"`
	CostUSD    float64        `json:"cost_usd,omitempty"`
	Finish     string         `json:"finish,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
}

// Writer appends events to a JSONL file.
type Writer struct {
	path string
	mu   sync.Mutex
}

// New creates (or truncates) path and returns a writer.
func New(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return &Writer{path: path}, nil
}

// Path returns the JSONL file path.
func (w *Writer) Path() string { return w.path }

// Log appends one event.
func (w *Writer) Log(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(ev)
}

// ReadAll loads every event from a JSONL file (replay).
func ReadAll(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var out []Event
	for dec.More() {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}
