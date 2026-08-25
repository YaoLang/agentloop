package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAI is a thin OpenAI-compatible Chat Completions client.
// Configure with OPENAI_BASE_URL (default https://api.openai.com/v1),
// OPENAI_API_KEY, and optional OPENAI_MODEL (default gpt-4o-mini).
type OpenAI struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// FromEnv builds a client from the environment. Returns an error if
// OPENAI_API_KEY is missing so the CLI can fail closed.
func FromEnv() (*OpenAI, error) {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}
	base := strings.TrimRight(os.Getenv("OPENAI_BASE_URL"), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	name := os.Getenv("OPENAI_MODEL")
	if name == "" {
		name = "gpt-4o-mini"
	}
	return &OpenAI{
		BaseURL: base,
		APIKey:  key,
		Model:   name,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *OpenAI) Name() string { return c.Model }

func (c *OpenAI) endpoint() string {
	b := strings.TrimRight(c.BaseURL, "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/chat/completions"
	}
	return b + "/v1/chat/completions"
}

type oaiReq struct {
	Model      string    `json:"model"`
	Messages   []oaiMsg  `json:"messages"`
	Tools      []oaiTool `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
	MaxTokens  int       `json:"max_tokens,omitempty"`
}

type oaiMsg struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiResp struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      oaiMsg `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (c *OpenAI) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	body := oaiReq{
		Model:     c.Model,
		Messages:  toOAI(req.Messages),
		MaxTokens: req.MaxTokens,
	}
	if len(req.Tools) > 0 {
		body.ToolChoice = "auto"
		for _, t := range req.Tools {
			var ot oaiTool
			ot.Type = "function"
			ot.Function.Name = t.Name
			ot.Function.Description = t.Description
			ot.Function.Parameters = t.Parameters
			body.Tools = append(body.Tools, ot)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return CompleteResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(raw))
	if err != nil {
		return CompleteResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	start := time.Now()
	httpResp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return CompleteResponse{}, err
	}
	defer httpResp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return CompleteResponse{}, err
	}
	var parsed oaiResp
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return CompleteResponse{}, fmt.Errorf("openai: decode: %w (status %d)", err, httpResp.StatusCode)
	}
	if parsed.Error != nil {
		return CompleteResponse{}, fmt.Errorf("openai: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if httpResp.StatusCode >= 300 {
		return CompleteResponse{}, fmt.Errorf("openai: HTTP %d: %s", httpResp.StatusCode, truncate(string(payload), 400))
	}
	if len(parsed.Choices) == 0 {
		return CompleteResponse{}, fmt.Errorf("openai: empty choices")
	}
	ch := parsed.Choices[0]
	msg := Message{
		Role:       nz(ch.Message.Role, "assistant"),
		Content:    ch.Message.Content,
		Name:       ch.Message.Name,
		ToolCallID: ch.Message.ToolCallID,
	}
	for _, tc := range ch.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if parsed.Usage.TotalTokens == 0 {
		parsed.Usage.TotalTokens = parsed.Usage.PromptTokens + parsed.Usage.CompletionTokens
	}
	return CompleteResponse{
		Message:      msg,
		Usage:        parsed.Usage,
		Latency:      time.Since(start),
		Model:        nz(parsed.Model, c.Model),
		FinishReason: ch.FinishReason,
	}, nil
}

func toOAI(in []Message) []oaiMsg {
	out := make([]oaiMsg, 0, len(in))
	for _, m := range in {
		om := oaiMsg{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			ot := oaiToolCall{ID: tc.ID, Type: "function"}
			ot.Function.Name = tc.Name
			ot.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, ot)
		}
		out = append(out, om)
	}
	return out
}

func nz(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
