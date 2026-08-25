package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
