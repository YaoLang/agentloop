// Package cli implements the agentloop subcommands: run, eval, replay, demo.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/YaoLang/agentloop/internal/agent"
	"github.com/YaoLang/agentloop/internal/eval"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/trace"
)

const version = "0.1.0"

const usage = `agentloop — a Go agent loop with sandbox tool-use, memory, and an eval harness.

Usage:
  agentloop run    --workspace DIR --goal "..." [--model mock|openai]
  agentloop eval   --suite FILE [--report FILE] [--judge]
  agentloop replay --trace FILE
  agentloop demo
  agentloop version

Environment (openai model only):
  OPENAI_API_KEY    required for --model openai
  OPENAI_BASE_URL   default https://api.openai.com/v1
  OPENAI_MODEL      default gpt-4o-mini
`

// Run is the process entrypoint.
func Run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stdout, usage)
		return nil
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "eval":
		return cmdEval(args[1:])
	case "replay":
		return cmdReplay(args[1:])
	case "demo":
		return cmdDemo(args[1:])
	case "version", "-v", "--version":
		fmt.Println("agentloop", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	ws := fs.String("workspace", "", "workspace directory (created if missing)")
	goal := fs.String("goal", "", "user goal")
	modelName := fs.String("model", "mock", "mock | openai")
	maxSteps := fs.Int("max-steps", 12, "maximum model→tool iterations")
	timeout := fs.Duration("timeout", 2*time.Minute, "wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ws == "" || *goal == "" {
		return fmt.Errorf("run requires --workspace and --goal")
	}
	if err := os.MkdirAll(*ws, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(*ws)
	if err != nil {
		return err
	}
	m, err := selectModel(*modelName)
	if err != nil {
		return err
	}
	fmt.Printf("== run  model=%s  workspace=%s\n", m.Name(), abs)
	fmt.Printf("goal: %s\n\n", *goal)
	res, err := agent.Run(context.Background(), agent.Config{
		Workspace: abs,
		Goal:      *goal,
		Model:     m,
		MaxSteps:  *maxSteps,
		Timeout:   *timeout,
	})
	if err != nil {
		return err
	}
	printResult(res)
	return nil
}

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	suite := fs.String("suite", "", "JSONL suite path")
	report := fs.String("report", "", "optional JSON report path")
	judge := fs.Bool("judge", false, "enable optional LLM-as-judge (default OFF; CI stays hermetic)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *suite == "" {
		return fmt.Errorf("eval requires --suite")
	}
	repPath := *report
	if repPath == "" {
		repPath = filepath.Join(os.TempDir(), "agentloop-eval-report.json")
	}
	rep, err := eval.RunFile(context.Background(), *suite, eval.Options{
		Judge:      *judge,
		ReportPath: repPath,
	})
	if err != nil {
		return err
	}
	fmt.Print(rep.Table())
	fmt.Printf("Report  %s\n", repPath)
	if *judge {
		fmt.Println("Judge   ON (not used by CI; default is OFF)")
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d case(s) failed", rep.Failed)
	}
	return nil
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	path := fs.String("trace", "", "JSONL trace file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("replay requires --trace")
	}
	evs, err := trace.ReadAll(*path)
	if err != nil {
		return err
	}
	fmt.Printf("=== Trace replay: %s (%d events) ===\n", *path, len(evs))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tTYPE\tSTEP\tDETAIL")
	for _, ev := range evs {
		detail := ev.Name
		if ev.Type == "model_call" {
			detail = fmt.Sprintf("model=%s finish=%s tokens=%v", ev.Model, ev.Finish, ev.Tokens)
		}
		if ev.Type == "tool_call" {
			ok := "?"
			if ev.OK != nil {
				if *ev.OK {
					ok = "ok"
				} else {
					ok = "ERR " + ev.Error
				}
			}
			detail = fmt.Sprintf("%s %s", ev.Name, ok)
		}
		if ev.Type == "final" {
			detail = fmt.Sprintf("stop=%s %s", ev.StopReason, truncate(ev.Content, 80))
		}
		if ev.Type == "budget" {
			detail = fmt.Sprintf("tokens=%v cost=$%.6f", ev.Tokens, ev.CostUSD)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n",
			ev.TS.Format("15:04:05.000"), ev.Type, ev.Step, detail)
	}
	_ = w.Flush()
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ws, err := os.MkdirTemp("", "agentloop-demo-")
	if err != nil {
		return err
	}
	goal := "Write a short note to notes.txt, remember the secret word 'lumen' in session memory, then summarize."
	script := []model.Step{
		{Tool: "write_file", Args: map[string]any{"path": "notes.txt", "content": "AgentLoop demo note. Secret word stored separately."}},
		{Tool: "read_file", Args: map[string]any{"path": "notes.txt"}},
		{Tool: "memory_write", Args: map[string]any{"key": "secret", "value": "lumen", "scope": "session"}},
		{Tool: "memory_read", Args: map[string]any{"key": "secret", "scope": "session"}},
		{Tool: "exec", Args: map[string]any{"command": []string{"echo", "demo-ok"}}},
		{Content: "Created notes.txt, stored secret='lumen' in session memory, and verified exec inside the workspace jail."},
	}
	fmt.Println("== AgentLoop demo (mock model, no network) ==")
	fmt.Printf("workspace: %s\n", ws)
	fmt.Printf("goal: %s\n\n", goal)

	res, err := agent.Run(context.Background(), agent.Config{
		Workspace: ws,
		Goal:      goal,
		Model:     model.NewScripted(script),
		MaxSteps:  8,
		Timeout:   15 * time.Second,
		RunID:     "demo",
	})
	if err != nil {
		return err
	}

	for _, tr := range res.ToolLog {
		status := "ok"
		if tr.Error != "" {
			status = "ERR"
		}
		fmt.Printf("step %d  tool=%-13s %s  %s\n", tr.Step, tr.Name, status, short(tr.Result, 70))
	}
	fmt.Println()
	printResult(res)
	fmt.Printf("\nInspect  %s\n", ws)
	return nil
}

func selectModel(name string) (model.Model, error) {
	switch strings.ToLower(name) {
	case "mock", "":
		return model.NewMock(), nil
	case "openai":
		return model.FromEnv()
	default:
		return nil, fmt.Errorf("unknown model %q (want mock|openai)", name)
	}
}

func printResult(res *agent.Result) {
	fmt.Println("Final:")
	fmt.Println(" ", strings.TrimSpace(res.Final))
	fmt.Println()
	fmt.Printf("Trace   %s\n", res.TracePath)
	fmt.Printf("Tokens  %d   Cost $%.6f   Steps %d   Latency %s   Stop %s\n",
		res.Tokens.TotalTokens, res.CostUSD, res.Steps, res.Latency.Round(time.Millisecond), res.StopReason)
	if res.JailHits > 0 {
		fmt.Printf("Safety  jail hits=%d\n", res.JailHits)
	}
	if res.Timeouts > 0 {
		fmt.Printf("Safety  tool timeouts=%d\n", res.Timeouts)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func short(s string, n int) string { return truncate(s, n) }
