package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/inject"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/tools"
)

func TestLoopWriteThenRead(t *testing.T) {
	ws := t.TempDir()
	m := model.NewScripted([]model.Step{
		{Tool: "write_file", Args: map[string]any{"path": "n.txt", "content": "hello"}},
		{Tool: "read_file", Args: map[string]any{"path": "n.txt"}},
		{Content: "the file says hello"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "write then read n.txt",
		Model:     m,
		MaxSteps:  6,
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "completed" {
		t.Fatalf("stop=%s final=%s", res.StopReason, res.Final)
	}
	if res.Final != "the file says hello" {
		t.Fatalf("final=%q", res.Final)
	}
	raw, err := os.ReadFile(filepath.Join(ws, "n.txt"))
	if err != nil || string(raw) != "hello" {
		t.Fatalf("file=%q err=%v", raw, err)
	}
	if res.Steps != 3 {
		t.Fatalf("steps=%d", res.Steps)
	}
	if res.TracePath == "" {
		t.Fatal("missing trace")
	}
}

func TestLoopJailIsObserved(t *testing.T) {
	ws := t.TempDir()
	m := model.NewScripted([]model.Step{
		{Tool: "read_file", Args: map[string]any{"path": "/etc/passwd"}},
		{Content: "refused"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "read /etc/passwd",
		Model:     m,
		MaxSteps:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.JailHits == 0 {
		t.Fatalf("expected jail hit, tools=%+v", res.ToolLog)
	}
	if res.Final != "refused" {
		t.Fatalf("final=%q", res.Final)
	}
}

func TestLoopMaxSteps(t *testing.T) {
	ws := t.TempDir()
	m := model.NewScripted([]model.Step{
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "1"}}},
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "2"}}},
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "3"}}},
		{Content: "never reached if max_steps=2"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "loop",
		Model:     m,
		MaxSteps:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "max_steps" {
		t.Fatalf("stop=%s", res.StopReason)
	}
	if res.Steps != 2 {
		t.Fatalf("steps=%d", res.Steps)
	}
}

func TestLoopTokenBudget(t *testing.T) {
	ws := t.TempDir()
	m := model.NewScripted([]model.Step{
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}},
		{Content: "done"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      strings.Repeat("goal ", 40),
		Model:     m,
		MaxSteps:  8,
		MaxTokens: 1, // trip immediately after first complete
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "token_budget" && res.StopReason != "completed" {
		// first iteration always runs; budget is checked at the top of the next
		// iteration. With MaxTokens=1 we should stop after step 1.
		t.Fatalf("stop=%s tokens=%d", res.StopReason, res.Tokens.TotalTokens)
	}
	if res.StopReason == "completed" && res.Steps > 1 {
		t.Fatalf("budget did not bind: steps=%d tokens=%d", res.Steps, res.Tokens.TotalTokens)
	}
}

func TestLoopSchemaErrorIsTraced(t *testing.T) {
	ws := t.TempDir()
	m := model.NewScripted([]model.Step{
		{Tool: "read_file", Args: map[string]any{"nope": true}},
		{Content: "handled"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "bad schema",
		Model:     m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SchemaErrs == 0 {
		t.Fatalf("expected schema error, log=%+v", res.ToolLog)
	}
}

func TestLoopRequiresGoal(t *testing.T) {
	_, err := Run(context.Background(), Config{Workspace: t.TempDir(), Model: model.NewMock()})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeniedTool(t *testing.T) {
	ws := t.TempDir()
	reg := tools.Default(tools.Options{Workspace: ws, ToolTimeout: time.Second})
	reg.Deny("exec")
	m := model.NewScripted([]model.Step{
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "x"}}},
		{Content: "denied ok"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "exec",
		Model:     m,
		Registry:  reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SchemaErrs == 0 {
		t.Fatal("expected deny to surface as schema/policy error")
	}
}

type flakyModel struct {
	calls int
}

func (m *flakyModel) Name() string { return "flaky" }

func (m *flakyModel) Complete(_ context.Context, _ model.CompleteRequest) (model.CompleteResponse, error) {
	m.calls++
	if m.calls <= 2 {
		return model.CompleteResponse{}, &model.RetryableError{Status: 503, Err: fmt.Errorf("flaky 503")}
	}
	return model.CompleteResponse{
		Message: model.Message{Role: "assistant", Content: "recovered"},
		Model:   m.Name(),
	}, nil
}

func TestLoopRetriesRetryableModelError(t *testing.T) {
	ws := t.TempDir()
	m := &flakyModel{}
	res, err := Run(context.Background(), Config{
		Workspace:      ws,
		Goal:           "retry me",
		Model:          m,
		MaxSteps:       4,
		ModelRetryWait: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "completed" {
		t.Fatalf("stop=%s final=%s", res.StopReason, res.Final)
	}
	if res.Final != "recovered" {
		t.Fatalf("final=%q", res.Final)
	}
	if m.calls != 3 {
		t.Fatalf("Complete called %d times, want 3", m.calls)
	}
}

type stickyErrModel struct {
	calls int
	err   error
}

func (m *stickyErrModel) Name() string { return "sticky" }

func (m *stickyErrModel) Complete(_ context.Context, _ model.CompleteRequest) (model.CompleteResponse, error) {
	m.calls++
	return model.CompleteResponse{}, m.err
}

func TestLoopPermanentModelError(t *testing.T) {
	ws := t.TempDir()
	m := &stickyErrModel{err: fmt.Errorf("openai: HTTP 400: bad request")}
	res, err := Run(context.Background(), Config{
		Workspace:      ws,
		Goal:           "fail",
		Model:          m,
		MaxSteps:       4,
		ModelRetryWait: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if res == nil || res.StopReason != "model_error" {
		t.Fatalf("stop=%v err=%v", res, err)
	}
	if m.calls != 1 {
		t.Fatalf("Complete called %d times, want 1", m.calls)
	}
	if !strings.Contains(res.Final, "400") {
		t.Fatalf("final should include error, got %q", res.Final)
	}
}

func TestLoopToolPanicContinues(t *testing.T) {
	ws := t.TempDir()
	reg := tools.Default(tools.Options{Workspace: ws, ToolTimeout: time.Second})
	reg.Register(&tools.Tool{
		Name:        "boom",
		Description: "panics",
		Schema:      map[string]any{"type": "object"},
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			panic("kaboom")
		},
	})
	m := model.NewScripted([]model.Step{
		{Tool: "boom", Args: map[string]any{}},
		{Content: "survived the boom"},
	})
	res, err := Run(context.Background(), Config{
		Workspace: ws,
		Goal:      "survive panic",
		Model:     m,
		Registry:  reg,
		MaxSteps:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "completed" {
		t.Fatalf("stop=%s final=%s", res.StopReason, res.Final)
	}
	if res.Panics < 1 {
		t.Fatalf("Panics=%d log=%+v", res.Panics, res.ToolLog)
	}
	if res.Final != "survived the boom" {
		t.Fatalf("final=%q", res.Final)
	}
	if len(res.ToolLog) == 0 || !strings.Contains(res.ToolLog[0].Result, "error:panic:") {
		t.Fatalf("expected panic observation, log=%+v", res.ToolLog)
	}
}

func TestLoopContextInjectDirectReply(t *testing.T) {
	ws := t.TempDir()
	cat := &inject.Catalog{
		Rules: []inject.Rule{{
			ID:          "ping",
			Match:       inject.Match{GoalPrefix: "ping"},
			DirectReply: "pong",
		}},
	}
	if err := cat.Compile(); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Config{
		Workspace:     ws,
		Goal:          "ping health",
		Model:         &mustNotCallModel{t: t},
		ContextInject: cat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "direct_reply" || res.Final != "pong" {
		t.Fatalf("stop=%s final=%q", res.StopReason, res.Final)
	}
	if res.Steps != 0 {
		t.Fatalf("steps=%d want 0", res.Steps)
	}
}

func TestLoopContextInjectMessages(t *testing.T) {
	ws := t.TempDir()
	cat := &inject.Catalog{
		Rules: []inject.Rule{{
			ID:    "hours",
			Match: inject.Match{GoalContains: []string{"hours"}},
			Messages: []model.Message{
				{Role: "user", Content: "demo question"},
				{Role: "assistant", Content: "demo answer"},
			},
		}},
	}
	if err := cat.Compile(); err != nil {
		t.Fatal(err)
	}
	probe := &messageProbe{}
	res, err := Run(context.Background(), Config{
		Workspace:     ws,
		Goal:          "store hours?",
		Model:         probe,
		ContextInject: cat,
		MaxSteps:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !probe.sawDemo {
		t.Fatal("injected messages missing from first Complete request")
	}
	if res.StopReason != "completed" || res.Final != "done" {
		t.Fatalf("stop=%s final=%q", res.StopReason, res.Final)
	}
}

type mustNotCallModel struct {
	t *testing.T
}

func (m *mustNotCallModel) Name() string { return "must-not-call" }

func (m *mustNotCallModel) Complete(_ context.Context, _ model.CompleteRequest) (model.CompleteResponse, error) {
	m.t.Fatal("model.Complete should not run for direct_reply")
	return model.CompleteResponse{}, nil
}

type messageProbe struct {
	sawDemo bool
}

func (m *messageProbe) Name() string { return "probe" }

func (m *messageProbe) Complete(_ context.Context, req model.CompleteRequest) (model.CompleteResponse, error) {
	for i, msg := range req.Messages {
		if msg.Role == "user" && msg.Content == "demo question" {
			m.sawDemo = true
		}
		if i == len(req.Messages)-1 && msg.Role == "user" && msg.Content != "store hours?" {
			return model.CompleteResponse{}, fmt.Errorf("last user message should be goal")
		}
	}
	return model.CompleteResponse{
		Message: model.Message{Role: "assistant", Content: "done"},
	}, nil
}
