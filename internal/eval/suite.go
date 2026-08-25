package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/YaoLang/agentloop/internal/model"
)

// Case is one JSONL eval record.
type Case struct {
	ID            string       `json:"id"`
	Goal          string       `json:"goal"`
	MaxSteps      int          `json:"max_steps"`
	ToolTimeoutMS int          `json:"tool_timeout_ms"`
	Script        []model.Step `json:"script"`
	Expect        Expect       `json:"expect"`
}

// Expect is the deterministic contract for a case.
type Expect struct {
	Success       bool              `json:"success"`
	Files         map[string]string `json:"files,omitempty"`
	ToolsUsed     []string          `json:"tools_used,omitempty"`
	JailCaught    bool              `json:"jail_caught,omitempty"`
	TimeoutCaught bool              `json:"timeout_caught,omitempty"`
	MaxSteps      int               `json:"max_steps,omitempty"`
	MaxLatencyMS  int               `json:"max_latency_ms,omitempty"`
	Memory        map[string]string `json:"memory,omitempty"`
	StopReason    string            `json:"stop_reason,omitempty"`
}

// LoadJSONL reads one Case per non-empty, non-# line.
func LoadJSONL(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var out []Case
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		if c.ID == "" {
			return nil, fmt.Errorf("%s:%d: missing id", path, lineNo)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}
