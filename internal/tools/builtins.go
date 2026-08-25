package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YaoLang/agentloop/internal/memory"
	"github.com/YaoLang/agentloop/internal/sandbox"
)

// Options configure the built-in tool set.
type Options struct {
	Workspace   string
	Memory      *memory.Store
	ToolTimeout time.Duration
	MaxOutput   int
	AllowBins   []string
	DenyBins    []string
}

// Default registers exec, read_file, write_file, memory_write, memory_read.
func Default(opt Options) *Registry {
	if opt.ToolTimeout <= 0 {
		opt.ToolTimeout = 5 * time.Second
	}
	if opt.MaxOutput <= 0 {
		opt.MaxOutput = 64 * 1024
	}
	r := NewRegistry()
	r.Register(execTool(opt))
	r.Register(readFileTool(opt))
	r.Register(writeFileTool(opt))
	r.Register(memoryWriteTool(opt))
	r.Register(memoryReadTool(opt))
	return r
}

type execArgs struct {
	Command []string `json:"command"`
	CWD     string   `json:"cwd"`
}

func execTool(opt Options) *Tool {
	return &Tool{
		Name:        "exec",
		Description: "Run an allow-listed binary inside the workspace jail. Path arguments that escape the workspace are refused. No Docker required.",
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("command"),
			"properties": map[string]any{
				"command": map[string]any{"type": "array", "items": prop("string"), "description": "argv, e.g. [\"echo\",\"hi\"]"},
				"cwd":     map[string]any{"type": "string", "description": "optional subdirectory of the workspace"},
			},
		},
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			var a execArgs
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			if len(a.Command) == 0 {
				return "", fmt.Errorf("exec: empty command")
			}
			ws := opt.Workspace
			if a.CWD != "" {
				jailed, err := sandbox.JailPath(opt.Workspace, a.CWD)
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(jailed, 0o755); err != nil {
					return "", err
				}
				ws = jailed
			}
			res, err := sandbox.Run(ctx, a.Command, sandbox.Limits{
				Workspace: ws,
				Timeout:   opt.ToolTimeout,
				MaxOutput: opt.MaxOutput,
				Allow:     opt.AllowBins,
				Deny:      opt.DenyBins,
			})
			if err != nil {
				return formatExec(res, err), err
			}
			return formatExec(res, nil), nil
		},
	}
}

func formatExec(res sandbox.Result, err error) string {
	var b strings.Builder
	if err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	}
	fmt.Fprintf(&b, "exit=%d timed_out=%v truncated=%v duration_ms=%d\n",
		res.ExitCode, res.TimedOut, res.Truncated, res.Duration.Milliseconds())
	if res.Stdout != "" {
		fmt.Fprintf(&b, "stdout:\n%s\n", res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", res.Stderr)
	}
	return strings.TrimSpace(b.String())
}

type pathArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func readFileTool(opt Options) *Tool {
	return &Tool{
		Name:        "read_file",
		Description: "Read a UTF-8 file inside the workspace. Absolute or .. paths that leave the workspace are refused.",
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("path"),
			"properties": map[string]any{
				"path": prop("string"),
			},
		},
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var a pathArgs
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			full, err := sandbox.JailPath(opt.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return "", err
			}
			if len(raw) > opt.MaxOutput {
				return string(raw[:opt.MaxOutput]) + "\n…(truncated)", nil
			}
			return string(raw), nil
		},
	}
}

func writeFileTool(opt Options) *Tool {
	return &Tool{
		Name:        "write_file",
		Description: "Write a file inside the workspace (parents created). Path jail applies.",
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("path", "content"),
			"properties": map[string]any{
				"path":    prop("string"),
				"content": prop("string"),
			},
		},
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var a pathArgs
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			full, err := sandbox.JailPath(opt.Workspace, a.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

type memArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope"`
}

func memoryWriteTool(opt Options) *Tool {
	return &Tool{
		Name:        "memory_write",
		Description: "Store a note. scope=session (default, in-process) or longterm (append-only JSONL on disk).",
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("key", "value"),
			"properties": map[string]any{
				"key":   prop("string"),
				"value": prop("string"),
				"scope": prop("string"),
			},
		},
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var a memArgs
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			if opt.Memory == nil {
				return "", fmt.Errorf("memory store is not configured")
			}
			if err := opt.Memory.Write(a.Scope, a.Key, a.Value); err != nil {
				return "", err
			}
			scope := a.Scope
			if scope == "" {
				scope = memory.ScopeSession
			}
			return fmt.Sprintf("stored %s key=%s", scope, a.Key), nil
		},
	}
}

func memoryReadTool(opt Options) *Tool {
	return &Tool{
		Name:        "memory_read",
		Description: "Read a previously stored note by key and scope.",
		Timeout:     opt.ToolTimeout,
		Schema: map[string]any{
			"type":     "object",
			"required": required("key"),
			"properties": map[string]any{
				"key":   prop("string"),
				"scope": prop("string"),
			},
		},
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var a memArgs
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			if opt.Memory == nil {
				return "", fmt.Errorf("memory store is not configured")
			}
			v, ok, err := opt.Memory.Read(a.Scope, a.Key)
			if err != nil {
				return "", err
			}
			if !ok {
				return fmt.Sprintf("not found: key=%s", a.Key), nil
			}
			return v, nil
		},
	}
}
