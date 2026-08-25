package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Step is one scripted model turn. If Tool is set, the mock emits a
// single tool call; otherwise Content is the final assistant message.
type Step struct {
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Content string         `json:"content,omitempty"`
}

// Mock is a deterministic, network-free Model. When Script is non-empty
// it walks the script one Complete() at a time. Otherwise it uses a
// small heuristic planner so `agentloop run --model mock` still does
// something visible.
type Mock struct {
	Script []Step
	mu     sync.Mutex
	i      int
	calls  int
}

// NewMock returns a heuristic mock (used by demo / run).
func NewMock() *Mock { return &Mock{} }

// NewScripted returns a mock that follows steps exactly (used by evals).
func NewScripted(steps []Step) *Mock { return &Mock{Script: steps} }

func (m *Mock) Name() string { return "mock" }

func (m *Mock) Complete(_ context.Context, req CompleteRequest) (CompleteResponse, error) {
	start := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++

	var msg Message
	if len(m.Script) > 0 {
		msg = m.scripted()
	} else {
		msg = m.heuristic(req)
	}

	prompt := 0
	for _, m0 := range req.Messages {
		prompt += EstimateTokens(m0.Content) + EstimateTokens(m0.Name)
		for _, tc := range m0.ToolCalls {
			prompt += EstimateTokens(tc.Arguments)
		}
	}
	comp := EstimateTokens(msg.Content)
	for _, tc := range msg.ToolCalls {
		comp += EstimateTokens(tc.Arguments)
	}
	return CompleteResponse{
		Message:      msg,
		Usage:        Usage{PromptTokens: prompt, CompletionTokens: comp, TotalTokens: prompt + comp},
		Latency:      time.Since(start),
		Model:        m.Name(),
		FinishReason: finishOf(msg),
	}, nil
}

func (m *Mock) scripted() Message {
	if m.i >= len(m.Script) {
		return Message{Role: "assistant", Content: "script complete"}
	}
	s := m.Script[m.i]
	m.i++
	if s.Tool == "" {
		return Message{Role: "assistant", Content: s.Content}
	}
	raw, _ := json.Marshal(s.Args)
	if raw == nil {
		raw = []byte("{}")
	}
	return Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:        fmt.Sprintf("call_%d", m.i),
			Name:      s.Tool,
			Arguments: string(raw),
		}},
	}
}

func (m *Mock) heuristic(req CompleteRequest) Message {
	goal := lastUser(req.Messages)
	// After tools have been observed a few times, finish.
	toolResults := 0
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			toolResults++
		}
	}
	switch {
	case toolResults == 0:
		return toolMsg("write_file", map[string]any{
			"path":    "agent-notes.txt",
			"content": "goal: " + truncate(goal, 400),
		}, 1)
	case toolResults == 1:
		return toolMsg("exec", map[string]any{
			"command": []string{"echo", "working"},
		}, 2)
	case toolResults == 2:
		return toolMsg("memory_write", map[string]any{
			"key":   "last_goal",
			"value": truncate(goal, 200),
			"scope": "session",
		}, 3)
	case toolResults == 3:
		return toolMsg("memory_read", map[string]any{
			"key":   "last_goal",
			"scope": "session",
		}, 4)
	default:
		return Message{
			Role:    "assistant",
			Content: "Done. Wrote agent-notes.txt, echoed a heartbeat, and stored the goal in session memory.",
		}
	}
}

func toolMsg(name string, args map[string]any, id int) Message {
	raw, _ := json.Marshal(args)
	return Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:        fmt.Sprintf("call_%d", id),
			Name:      name,
			Arguments: string(raw),
		}},
	}
}

func lastUser(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func finishOf(m Message) string {
	if len(m.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Calls reports how many times Complete was invoked (tests).
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
