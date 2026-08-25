package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YaoLang/agentloop/internal/agent"
	"github.com/YaoLang/agentloop/internal/memory"
	"github.com/YaoLang/agentloop/internal/model"
	"github.com/YaoLang/agentloop/internal/tools"
)

// Options control a suite run.
type Options struct {
	// Judge enables an optional LLM-as-judge pass. Default OFF so CI
	// stays hermetic. When true, JudgeModel is queried; if nil, the
	// case model is reused (the mock judge always agrees with Score).
	Judge      bool
	JudgeModel model.Model
	// ReportPath writes the JSON report. Empty → skip.
	ReportPath string
}

// CaseReport is one scored case.
type CaseReport struct {
	ID         string `json:"id"`
	Pass       bool   `json:"pass"`
	Scores     Scores `json:"scores"`
	StopReason string `json:"stop_reason"`
	Steps      int    `json:"steps"`
	LatencyMS  int64  `json:"latency_ms"`
	JailHits   int    `json:"jail_hits"`
	Timeouts   int    `json:"timeouts"`
	Tokens     int    `json:"tokens"`
	Judge      string `json:"judge,omitempty"`
	Final      string `json:"final,omitempty"`
}

// Report is the suite roll-up.
type Report struct {
	Suite        string       `json:"suite"`
	Model        string       `json:"model"`
	Cases        []CaseReport `json:"cases"`
	Passed       int          `json:"passed"`
	Failed       int          `json:"failed"`
	SuccessRate  float64      `json:"success_rate"`
	SchemaRate   float64      `json:"schema_rate"`
	JailCaught   int          `json:"jail_caught"`
	JailExpected int          `json:"jail_expected"`
	AvgLatencyMS float64      `json:"avg_latency_ms"`
	AvgSteps     float64      `json:"avg_steps"`
	GeneratedAt  time.Time    `json:"generated_at"`
}

// RunFile loads a JSONL suite and scores every case against the mock
// (or the script attached to the case). No network.
func RunFile(ctx context.Context, suitePath string, opt Options) (*Report, error) {
	cases, err := LoadJSONL(suitePath)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Suite:       suitePath,
		Model:       "mock",
		GeneratedAt: time.Now().UTC(),
	}
	var latSum int64
	var stepSum int
	for _, c := range cases {
		cr, err := runCase(ctx, c, opt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.ID, err)
		}
		rep.Cases = append(rep.Cases, cr)
		if cr.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
		if cr.Scores.SchemaValid {
			rep.SchemaRate += 1
		}
		if c.Expect.JailCaught {
			rep.JailExpected++
			if cr.JailHits > 0 {
				rep.JailCaught++
			}
		}
		latSum += cr.LatencyMS
		stepSum += cr.Steps
	}
	n := float64(len(rep.Cases))
	if n > 0 {
		rep.SuccessRate = float64(rep.Passed) / n
		rep.SchemaRate = rep.SchemaRate / n
		rep.AvgLatencyMS = float64(latSum) / n
		rep.AvgSteps = float64(stepSum) / n
	}
	if opt.ReportPath != "" {
		if err := writeReport(opt.ReportPath, rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func runCase(ctx context.Context, c Case, opt Options) (CaseReport, error) {
	ws, err := os.MkdirTemp("", "agentloop-eval-"+c.ID+"-")
	if err != nil {
		return CaseReport{}, err
	}
	defer os.RemoveAll(ws)

	mem, err := memory.Open(ws)
	if err != nil {
		return CaseReport{}, err
	}
	timeout := 5 * time.Second
	if c.ToolTimeoutMS > 0 {
		timeout = time.Duration(c.ToolTimeoutMS) * time.Millisecond
	}
	reg := tools.Default(tools.Options{Workspace: ws, Memory: mem, ToolTimeout: timeout})
	maxSteps := c.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}
	m := model.NewScripted(c.Script)
	res, err := agent.Run(ctx, agent.Config{
		Workspace:   ws,
		Goal:        c.Goal,
		Model:       m,
		Registry:    reg,
		Memory:      mem,
		MaxSteps:    maxSteps,
		ToolTimeout: timeout,
		Timeout:     30 * time.Second,
		RunID:       c.ID,
	})
	if err != nil && res == nil {
		return CaseReport{}, err
	}
	scores := Score(c, res, ws)
	if opt.Judge {
		scores = applyJudge(ctx, opt.JudgeModel, c, res, scores)
	}
	return CaseReport{
		ID:         c.ID,
		Pass:       scores.Pass(),
		Scores:     scores,
		StopReason: res.StopReason,
		Steps:      res.Steps,
		LatencyMS:  res.Latency.Milliseconds(),
		JailHits:   res.JailHits,
		Timeouts:   res.Timeouts,
		Tokens:     res.Tokens.TotalTokens,
		Final:      res.Final,
	}, nil
}

func applyJudge(ctx context.Context, m model.Model, c Case, res *agent.Result, s Scores) Scores {
	if m == nil {
		// Hermetic default: the mock judge restates the deterministic score.
		if !s.Pass() {
			s.Reasons = append(s.Reasons, "judge: agree with deterministic fail")
		}
		return s
	}
	prompt := fmt.Sprintf("Did the agent achieve the goal?\nGoal: %s\nFinal: %s\nReply PASS or FAIL.", c.Goal, res.Final)
	resp, err := m.Complete(ctx, model.CompleteRequest{
		Messages: []model.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		s.Reasons = append(s.Reasons, "judge error: "+err.Error())
		return s
	}
	if strings.Contains(strings.ToUpper(resp.Message.Content), "FAIL") {
		s.Success = false
		s.Reasons = append(s.Reasons, "llm-judge: FAIL")
	}
	return s
}

func writeReport(path string, rep *Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		// Dir may be "."
		_ = err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Table renders a recruiter-friendly score table.
func (r *Report) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "SUITE  %s\n", r.Suite)
	fmt.Fprintf(&b, "MODEL  %s\n\n", r.Model)
	fmt.Fprintf(&b, "%-22s %-7s %-6s %-8s %5s %8s %s\n",
		"ID", "SUCCESS", "JAIL", "TIMEOUT", "STEPS", "LATENCY", "SCHEMA")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("─", 72))
	for _, c := range r.Cases {
		jail := "-"
		if c.JailHits > 0 {
			jail = "yes"
		}
		to := "-"
		if c.Timeouts > 0 {
			to = "yes"
		}
		schema := "ok"
		if !c.Scores.SchemaValid {
			schema = "FAIL"
		}
		succ := "PASS"
		if !c.Pass {
			succ = "FAIL"
		}
		fmt.Fprintf(&b, "%-22s %-7s %-6s %-8s %5d %6dms %s\n",
			c.ID, succ, jail, to, c.Steps, c.LatencyMS, schema)
		if !c.Pass && len(c.Scores.Reasons) > 0 {
			fmt.Fprintf(&b, "  ↳ %s\n", strings.Join(c.Scores.Reasons, "; "))
		}
	}
	fmt.Fprintf(&b, "\nScore   success=%d/%d (%.0f%%)  schema=%.0f%%  jail=%d/%d  avg_latency=%.1fms  avg_steps=%.1f\n",
		r.Passed, r.Passed+r.Failed, r.SuccessRate*100, r.SchemaRate*100,
		r.JailCaught, r.JailExpected, r.AvgLatencyMS, r.AvgSteps)
	return b.String()
}
