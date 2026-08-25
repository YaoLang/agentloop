package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxRetries = 3
	defaultBackoff    = 200 * time.Millisecond
	maxBackoff        = 2 * time.Second
	maxBodyBytes      = 4 << 20
)

// OpenAI is a thin OpenAI-compatible Chat Completions client.
// Configure with OPENAI_BASE_URL (default https://api.openai.com/v1),
// OPENAI_API_KEY, and optional OPENAI_MODEL (default gpt-4o-mini).
type OpenAI struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTP       *http.Client
	MaxRetries int           // default 3 (plus the first attempt)
	Backoff    time.Duration // default 200ms; exponential, cap 2s
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
		BaseURL:    base,
		APIKey:     key,
		Model:      name,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		MaxRetries: defaultMaxRetries,
		Backoff:    defaultBackoff,
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
	ht := c.HTTP
	if ht == nil {
		ht = &http.Client{Timeout: 60 * time.Second}
	}
	maxRetries := c.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = defaultBackoff
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

	start := time.Now()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return CompleteResponse{}, err
		}
		resp, err := c.doOnce(ctx, ht, raw)
		if err == nil {
			resp.Latency = time.Since(start)
			return resp, nil
		}
		lastErr = err
		if !IsRetryable(err) || attempt == maxRetries {
			return CompleteResponse{}, err
		}
		wait := retryWait(err, backoff, attempt)
		if re, ok := lastErr.(*RetryableError); ok && re.After == 0 {
			re.After = wait
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return CompleteResponse{}, err
		}
	}
	return CompleteResponse{}, lastErr
}

func (c *OpenAI) doOnce(ctx context.Context, ht *http.Client, raw []byte) (CompleteResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(raw))
	if err != nil {
		return CompleteResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	httpResp, err := ht.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return CompleteResponse{}, ctx.Err()
		}
		// Do not wrap with %w: client timeouts are DeadlineExceeded but
		// are still transport failures we retry. Parent ctx is returned above.
		return CompleteResponse{}, &RetryableError{Err: fmt.Errorf("transport: %v", err)}
	}
	defer httpResp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes))
	if err != nil {
		if ctx.Err() != nil {
			return CompleteResponse{}, ctx.Err()
		}
		return CompleteResponse{}, &RetryableError{Status: httpResp.StatusCode, Err: fmt.Errorf("transport: %v", err)}
	}

	status := httpResp.StatusCode
	after := parseRetryAfter(httpResp.Header)
	var parsed oaiResp
	decErr := json.Unmarshal(payload, &parsed)

	if retryableStatus(status) {
		inner := fmt.Errorf("openai: HTTP %d: %s", status, truncate(string(payload), 400))
		if decErr == nil && parsed.Error != nil {
			inner = fmt.Errorf("openai: %s: %s", parsed.Error.Type, parsed.Error.Message)
		} else if decErr != nil {
			inner = fmt.Errorf("openai: decode: %w (status %d)", decErr, status)
		}
		return CompleteResponse{}, &RetryableError{Status: status, After: after, Err: inner}
	}

	if decErr != nil {
		err := fmt.Errorf("openai: decode: %w (status %d)", decErr, status)
		if status >= 200 && status < 300 {
			return CompleteResponse{}, &RetryableError{Status: status, Err: err}
		}
		return CompleteResponse{}, err
	}
	if parsed.Error != nil {
		return CompleteResponse{}, fmt.Errorf("openai: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if status >= 300 {
		return CompleteResponse{}, fmt.Errorf("openai: HTTP %d: %s", status, truncate(string(payload), 400))
	}
	if len(parsed.Choices) == 0 {
		return CompleteResponse{}, &RetryableError{Status: status, Err: fmt.Errorf("openai: empty choices")}
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
		Model:        nz(parsed.Model, c.Model),
		FinishReason: ch.FinishReason,
	}, nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

func retryWait(err error, base time.Duration, attempt int) time.Duration {
	var re *RetryableError
	if errors.As(err, &re) && re.After > 0 {
		return re.After
	}
	return expBackoff(base, attempt, maxBackoff)
}

func expBackoff(base time.Duration, attempt int, cap time.Duration) time.Duration {
	if base <= 0 {
		base = defaultBackoff
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 20 {
		return cap
	}
	d := base * time.Duration(1<<uint(attempt))
	if d > cap || d < 0 {
		return cap
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
