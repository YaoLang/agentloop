package eval

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/YaoLang/agentloop/internal/agent"
)

// Scores are the deterministic graders. A case PASSES only if every
// applicable scorer is true.
type Scores struct {
	Success     bool     `json:"success"`
	SchemaValid bool     `json:"schema_valid"`
	JailOK      bool     `json:"jail_ok"`
	TimeoutOK   bool     `json:"timeout_ok"`
	LatencyOK   bool     `json:"latency_ok"`
	StepsOK     bool     `json:"steps_ok"`
	Reasons     []string `json:"reasons,omitempty"`
}

// Pass is the conjunction of all scorers.
func (s Scores) Pass() bool {
	return s.Success && s.SchemaValid && s.JailOK && s.TimeoutOK && s.LatencyOK && s.StepsOK
}

// Score compares a loop result against the case contract.
func Score(c Case, res *agent.Result, workspace string) Scores {
	var s Scores
	s.SchemaValid = res.SchemaErrs == 0
	if !s.SchemaValid {
		s.Reasons = append(s.Reasons, "tool-schema invalid")
	}

	if c.Expect.JailCaught {
		s.JailOK = res.JailHits > 0
		if !s.JailOK {
			s.Reasons = append(s.Reasons, "expected jail/path-escape to be caught")
		}
	} else {
		s.JailOK = true
	}

	if c.Expect.TimeoutCaught {
		s.TimeoutOK = res.Timeouts > 0
		if !s.TimeoutOK {
			s.Reasons = append(s.Reasons, "expected tool timeout")
		}
	} else {
		s.TimeoutOK = true
	}

	limit := c.Expect.MaxLatencyMS
	if limit <= 0 {
		limit = 15_000
	}
	s.LatencyOK = res.Latency.Milliseconds() <= int64(limit)
	if !s.LatencyOK {
		s.Reasons = append(s.Reasons, "latency over budget")
	}

	maxSteps := c.Expect.MaxSteps
	if maxSteps <= 0 {
		maxSteps = c.MaxSteps
	}
	if maxSteps <= 0 {
		maxSteps = 12
	}
	s.StepsOK = res.Steps <= maxSteps
	if !s.StepsOK {
		s.Reasons = append(s.Reasons, "too many steps")
	}

	if c.Expect.StopReason != "" && res.StopReason != c.Expect.StopReason {
		s.Reasons = append(s.Reasons, "stop_reason want "+c.Expect.StopReason+" got "+res.StopReason)
	}

	for path, want := range c.Expect.Files {
		raw, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil {
			s.Reasons = append(s.Reasons, "missing file "+path)
			continue
		}
		if string(raw) != want {
			s.Reasons = append(s.Reasons, "file content mismatch "+path)
		}
	}

	if len(c.Expect.ToolsUsed) > 0 {
		seen := map[string]bool{}
		for _, tr := range res.ToolLog {
			seen[tr.Name] = true
		}
		for _, name := range c.Expect.ToolsUsed {
			if !seen[name] {
				s.Reasons = append(s.Reasons, "tool not used: "+name)
			}
		}
	}

	if res.Session != nil && len(c.Expect.Memory) > 0 {
		// Memory is on the store, not the session. We check tool results
		// for memory_read hits and also look at written values via traces.
		got := map[string]string{}
		for _, tr := range res.ToolLog {
			if tr.Name == "memory_read" && tr.Error == "" && !strings.HasPrefix(tr.Result, "not found") {
				// recover key from args if possible
				key := extractJSONString(tr.Args, "key")
				if key != "" {
					got[key] = tr.Result
				}
			}
		}
		for k, v := range c.Expect.Memory {
			if got[k] != v {
				s.Reasons = append(s.Reasons, "memory["+k+"] want "+v+" got "+got[k])
			}
		}
	}

	// Success scorer: the agent finished (or handled a safety case) and
	// no extra reasons were recorded. expect.success=false means we
	// *want* the run to fail the success bit (unused in the basic suite).
	clean := true
	for _, r := range s.Reasons {
		if strings.HasPrefix(r, "tool-schema") ||
			strings.HasPrefix(r, "expected jail") ||
			strings.HasPrefix(r, "expected tool timeout") ||
			strings.HasPrefix(r, "latency") ||
			strings.HasPrefix(r, "too many") {
			continue
		}
		clean = false
		break
	}
	completed := res.StopReason == "completed" || (c.Expect.JailCaught && res.JailHits > 0) || (c.Expect.TimeoutCaught && res.Timeouts > 0)
	s.Success = c.Expect.Success && completed && clean
	if c.Expect.Success && !s.Success && completed && clean {
		// already true
	}
	if !c.Expect.Success {
		s.Success = !completed
	}
	if c.Expect.Success && !s.Success {
		if !completed {
			s.Reasons = append(s.Reasons, "agent did not complete ("+res.StopReason+")")
		}
	}
	return s
}

func extractJSONString(raw, key string) string {
	// tiny extractor so we do not depend on a second unmarshal everywhere
	needle := `"` + key + `"`
	i := strings.Index(raw, needle)
	if i < 0 {
		return ""
	}
	rest := raw[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return rest[:k]
}
