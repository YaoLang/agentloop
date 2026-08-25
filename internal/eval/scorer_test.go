package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/agent"
	"github.com/YaoLang/agentloop/internal/session"
)

func TestScoreTable(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "n.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		c    Case
		res  *agent.Result
		pass bool
	}{
		{
			name: "happy write-read",
			c: Case{
				Expect: Expect{
					Success:      true,
					Files:        map[string]string{"n.txt": "hello"},
					ToolsUsed:    []string{"write_file", "read_file"},
					MaxSteps:     5,
					MaxLatencyMS: 1000,
				},
			},
			res: &agent.Result{
				StopReason: "completed",
				Steps:      3,
				Latency:    2 * time.Millisecond,
				ToolLog: []session.ToolTrace{
					{Name: "write_file"},
					{Name: "read_file"},
				},
			},
			pass: true,
		},
		{
			name: "jail expected and caught",
			c:    Case{Expect: Expect{Success: true, JailCaught: true}},
			res:  &agent.Result{StopReason: "completed", JailHits: 1, Latency: time.Millisecond, Steps: 2},
			pass: true,
		},
		{
			name: "jail expected but missed",
			c:    Case{Expect: Expect{Success: true, JailCaught: true}},
			res:  &agent.Result{StopReason: "completed", JailHits: 0, Latency: time.Millisecond, Steps: 2},
			pass: false,
		},
		{
			name: "timeout expected and caught",
			c:    Case{Expect: Expect{Success: true, TimeoutCaught: true}},
			res:  &agent.Result{StopReason: "completed", Timeouts: 1, Latency: time.Millisecond, Steps: 2},
			pass: true,
		},
		{
			name: "schema error fails",
			c:    Case{Expect: Expect{Success: true}},
			res:  &agent.Result{StopReason: "completed", SchemaErrs: 1, Latency: time.Millisecond, Steps: 1},
			pass: false,
		},
		{
			name: "too many steps",
			c:    Case{Expect: Expect{Success: true, MaxSteps: 2}},
			res:  &agent.Result{StopReason: "completed", Steps: 9, Latency: time.Millisecond},
			pass: false,
		},
		{
			name: "latency over budget",
			c:    Case{Expect: Expect{Success: true, MaxLatencyMS: 1}},
			res:  &agent.Result{StopReason: "completed", Steps: 1, Latency: 50 * time.Millisecond},
			pass: false,
		},
		{
			name: "missing file",
			c:    Case{Expect: Expect{Success: true, Files: map[string]string{"nope.txt": "x"}}},
			res:  &agent.Result{StopReason: "completed", Steps: 1, Latency: time.Millisecond},
			pass: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Score(tc.c, tc.res, ws)
			if s.Pass() != tc.pass {
				t.Fatalf("pass=%v want %v reasons=%v scores=%+v", s.Pass(), tc.pass, s.Reasons, s)
			}
		})
	}
}

func TestExtractJSONString(t *testing.T) {
	if got := extractJSONString(`{"key":"last_goal","scope":"session"}`, "key"); got != "last_goal" {
		t.Fatalf("got %q", got)
	}
}
