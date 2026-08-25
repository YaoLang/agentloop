// Package agent is the model → tool → observe loop.
//
//	for step := 1; step <= max; step++ {
//	    check budget (steps, tokens, cost, wall clock)
//	    resp := model.Complete(messages, tools)
//	    if no tool calls { return final }
//	    for each tool call { validate; execute; append observation }
//	}
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/YaoLang/agentloop/internal/memory"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/sandbox"
	"github.com/YaoLang/agentloop/internal/session"
	"github.com/YaoLang/agentloop/internal/tools"
	"github.com/YaoLang/agentloop/internal/trace"
)

// Config is one run.
type Config struct {
	Workspace   string
	Goal        string
	Model       model.Model
	Registry    *tools.Registry
	Memory      *memory.Store
	MaxSteps    int
	MaxTokens   int
	MaxCostUSD  float64
	Timeout     time.Duration
	ToolTimeout time.Duration
	RunID       string
}

// Result is the outcome of a loop.
type Result struct {
	Final      string
	Steps      int
	Tokens     model.Usage
	CostUSD    float64
	Latency    time.Duration
	TracePath  string
	StopReason string
	ToolLog    []session.ToolTrace
	JailHits   int
	Timeouts   int
	SchemaErrs int
	Session    *session.Session
}

// Defaults fills zero-value limits.
func Defaults(cfg *Config) {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 32_000
	}
	if cfg.MaxCostUSD <= 0 {
		cfg.MaxCostUSD = 1.00
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 5 * time.Second
	}
	if cfg.RunID == "" {
		cfg.RunID = newID()
	}
}

// Run executes the agent loop against cfg.Goal.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	Defaults(&cfg)
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("agent: workspace is required")
	}
	if cfg.Goal == "" {
		return nil, fmt.Errorf("agent: goal is required")
	}
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: model is required")
	}

	if cfg.Memory == nil {
		mem, err := memory.Open(cfg.Workspace)
		if err != nil {
			return nil, err
		}
		cfg.Memory = mem
	}
	if cfg.Registry == nil {
		cfg.Registry = tools.Default(tools.Options{
			Workspace:   cfg.Workspace,
			Memory:      cfg.Memory,
			ToolTimeout: cfg.ToolTimeout,
		})
	}

	sess := session.New(cfg.RunID, cfg.Workspace, cfg.Goal)
	sess.Add(model.Message{
		Role:    "system",
		Content: systemPrompt(),
	})
	sess.Add(model.Message{Role: "user", Content: cfg.Goal})

	tw, err := trace.New(filepath.Join(cfg.Workspace, ".agentloop", "traces", cfg.RunID+".jsonl"))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	out := &Result{TracePath: tw.Path(), Session: sess}
	start := time.Now()
	specs := cfg.Registry.Specs()

	for step := 1; step <= cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			out.StopReason = "timeout"
			out.Latency = time.Since(start)
			_ = finish(tw, sess, out)
			return out, err
		}
		if out.Tokens.TotalTokens >= cfg.MaxTokens {
			out.StopReason = "token_budget"
			break
		}
		if out.CostUSD >= cfg.MaxCostUSD {
			out.StopReason = "cost_budget"
			break
		}

		resp, err := cfg.Model.Complete(ctx, model.CompleteRequest{
			Messages: sess.Messages,
			Tools:    specs,
		})
		if err != nil {
			out.StopReason = "model_error"
			out.Latency = time.Since(start)
			_ = finish(tw, sess, out)
			return out, err
		}
		out.Steps = step
		out.Tokens.Add(resp.Usage)
		out.CostUSD = model.CostUSD(out.Tokens.PromptTokens, out.Tokens.CompletionTokens)

		_ = tw.Log(trace.Event{
			Type:      "model_call",
			RunID:     cfg.RunID,
			Step:      step,
			Model:     resp.Model,
			LatencyMS: resp.Latency.Milliseconds(),
			Tokens: map[string]int{
				"prompt":     resp.Usage.PromptTokens,
				"completion": resp.Usage.CompletionTokens,
				"total":      resp.Usage.TotalTokens,
			},
			CostUSD: out.CostUSD,
			Finish:  resp.FinishReason,
			Content: resp.Message.Content,
		})

		sess.Add(resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			out.Final = resp.Message.Content
			out.StopReason = "completed"
			out.Latency = time.Since(start)
			_ = finish(tw, sess, out)
			return out, nil
		}

		for _, tc := range resp.Message.ToolCalls {
			tr := session.ToolTrace{
				Step: step,
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Arguments,
			}
			t0 := time.Now()
			if err := cfg.Registry.Validate(tc.Name, tc.Arguments); err != nil {
				tr.Error = err.Error()
				tr.SchemaErr = true
				tr.Result = "schema/policy error: " + err.Error()
				out.SchemaErrs++
			} else {
				obs, callErr := cfg.Registry.Call(ctx, tc.Name, tc.Arguments)
				tr.Result = obs
				if callErr != nil {
					tr.Error = callErr.Error()
					if errors.Is(callErr, sandbox.ErrPathEscape) || errors.Is(callErr, sandbox.ErrDeniedBin) {
						tr.Jail = true
						out.JailHits++
					}
					if errors.Is(callErr, sandbox.ErrTimeout) || errors.Is(callErr, context.DeadlineExceeded) {
						tr.Timeout = true
						out.Timeouts++
					}
				}
				if tr.Result == "" && callErr != nil {
					tr.Result = "error: " + callErr.Error()
				}
			}
			tr.Latency = time.Since(t0)
			sess.AddTool(tr)
			out.ToolLog = append(out.ToolLog, tr)

			ok := tr.Error == ""
			_ = tw.Log(trace.Event{
				Type:      "tool_call",
				RunID:     cfg.RunID,
				Step:      step,
				Name:      tc.Name,
				Args:      tc.Arguments,
				OK:        &ok,
				Error:     tr.Error,
				Content:   truncate(tr.Result, 2000),
				LatencyMS: tr.Latency.Milliseconds(),
			})

			sess.Add(model.Message{
				Role:       "tool",
				Name:       tc.Name,
				ToolCallID: tc.ID,
				Content:    tr.Result,
			})
		}

		_ = tw.Log(trace.Event{
			Type:    "budget",
			RunID:   cfg.RunID,
			Step:    step,
			Tokens:  map[string]int{"total": out.Tokens.TotalTokens},
			CostUSD: out.CostUSD,
		})
	}

	if out.StopReason == "" {
		out.StopReason = "max_steps"
	}
	if out.Final == "" {
		out.Final = "stopped: " + out.StopReason
	}
	out.Latency = time.Since(start)
	_ = finish(tw, sess, out)
	return out, nil
}

func finish(tw *trace.Writer, sess *session.Session, out *Result) error {
	_ = tw.Log(trace.Event{
		Type:       "final",
		RunID:      sess.ID,
		Content:    out.Final,
		StopReason: out.StopReason,
		CostUSD:    out.CostUSD,
		Tokens:     map[string]int{"total": out.Tokens.TotalTokens},
		LatencyMS:  out.Latency.Milliseconds(),
		Step:       out.Steps,
	})
	return sess.Save()
}

func systemPrompt() string {
	return "You are AgentLoop, a careful coding agent. Use tools to work inside the workspace. " +
		"Never attempt to read or write paths outside the workspace. Prefer small, reversible steps. " +
		"When the goal is met, answer concisely without further tool calls."
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
