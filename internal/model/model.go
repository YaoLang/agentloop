// Package model defines the LLM surface the agent loop talks to.
// Implementations: a deterministic mock (tests/demo) and an
// OpenAI-compatible HTTP client (OPENAI_BASE_URL + OPENAI_API_KEY).
package model

import (
	"context"
	"time"
)

// Message is one turn in the conversation (user / assistant / tool).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON object
}

// ToolSpec is advertised to the model (JSON Schema parameters).
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// CompleteRequest is one model invocation.
type CompleteRequest struct {
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

// Usage is token accounting for one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add accumulates usage.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// CompleteResponse is the model's next message plus telemetry.
type CompleteResponse struct {
	Message      Message
	Usage        Usage
	Latency      time.Duration
	Model        string
	FinishReason string
}

// Model produces the next assistant message (optionally with tool calls).
type Model interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
	Name() string
}

// EstimateTokens is a cheap offline stand-in (~4 chars/token).
func EstimateTokens(s string) int {
	n := len(s) / 4
	if n < 1 && s != "" {
		return 1
	}
	return n
}

// DefaultUSD prices (gpt-4o-mini-class) used for budget bookkeeping.
const (
	PromptUSDPerMTok     = 0.15
	CompletionUSDPerMTok = 0.60
)

// CostUSD estimates dollar cost from token counts.
func CostUSD(prompt, completion int) float64 {
	return float64(prompt)/1e6*PromptUSDPerMTok + float64(completion)/1e6*CompletionUSDPerMTok
}
