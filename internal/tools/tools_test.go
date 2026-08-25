package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YaoLang/agentloop/internal/memory"
	"github.com/YaoLang/agentloop/internal/sandbox"
)

func setup(t *testing.T) (*Registry, string, *memory.Store) {
	t.Helper()
	ws := t.TempDir()
	mem, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	reg := Default(Options{Workspace: ws, Memory: mem, ToolTimeout: time.Second})
	return reg, ws, mem
}

func TestReadWriteRoundTrip(t *testing.T) {
	reg, _, _ := setup(t)
	out, err := reg.Call(context.Background(), "write_file", `{"path":"a/b.txt","content":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote") {
		t.Fatalf("out=%q", out)
	}
	got, err := reg.Call(context.Background(), "read_file", `{"path":"a/b.txt"}`)
	if err != nil || got != "hi" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestFileJail(t *testing.T) {
	reg, _, _ := setup(t)
	_, err := reg.Call(context.Background(), "read_file", `{"path":"/etc/passwd"}`)
	if !errors.Is(err, sandbox.ErrPathEscape) {
		t.Fatalf("err=%v", err)
	}
	_, err = reg.Call(context.Background(), "write_file", `{"path":"../../../tmp/pwned","content":"x"}`)
	if !errors.Is(err, sandbox.ErrPathEscape) {
		t.Fatalf("err=%v", err)
	}
}

func TestMemoryTools(t *testing.T) {
	reg, _, mem := setup(t)
	if _, err := reg.Call(context.Background(), "memory_write", `{"key":"k","value":"v","scope":"session"}`); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Call(context.Background(), "memory_read", `{"key":"k","scope":"session"}`)
	if err != nil || got != "v" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := reg.Call(context.Background(), "memory_write", `{"key":"k","value":"lt","scope":"longterm"}`); err != nil {
		t.Fatal(err)
	}
	got, err = reg.Call(context.Background(), "memory_read", `{"key":"k","scope":"longterm"}`)
	if err != nil || got != "lt" {
		t.Fatalf("longterm got %q err=%v", got, err)
	}
	snap := mem.SessionSnapshot()
	if snap["k"] != "v" {
		t.Fatalf("session snapshot %+v", snap)
	}
}

func TestSchemaValidation(t *testing.T) {
	reg, _, _ := setup(t)
	err := reg.Validate("read_file", `{"nope":1}`)
	if err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("err=%v", err)
	}
	err = reg.Validate("exec", `{"command":"echo"}`)
	if err == nil || !strings.Contains(err.Error(), "array") {
		t.Fatalf("err=%v", err)
	}
	if err := reg.Validate("exec", `{"command":["echo","hi"]}`); err != nil {
		t.Fatal(err)
	}
}

func TestDenyGate(t *testing.T) {
	reg, _, _ := setup(t)
	reg.Deny("exec")
	if err := reg.Validate("exec", `{"command":["echo"]}`); err == nil {
		t.Fatal("expected deny")
	}
}

func TestExecEcho(t *testing.T) {
	reg, ws, _ := setup(t)
	out, err := reg.Call(context.Background(), "exec", `{"command":["echo","ok"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("out=%q", out)
	}
	// workspace should not be polluted by the binary itself
	if _, err := os.Stat(filepath.Join(ws, "echo")); err == nil {
		t.Fatal("echo binary leaked into workspace")
	}
}

func TestValidateArgsTable(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": required("path"),
		"properties": map[string]any{
			"path":  prop("string"),
			"flag":  prop("boolean"),
			"items": prop("array"),
		},
	}
	cases := []struct {
		args string
		ok   bool
	}{
		{`{"path":"a"}`, true},
		{`{"path":"a","flag":true}`, true},
		{`{}`, false},
		{`{"path":1}`, false},
		{`not-json`, false},
	}
	for _, tc := range cases {
		err := ValidateArgs(schema, tc.args)
		if tc.ok && err != nil {
			t.Fatalf("args %s: %v", tc.args, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("args %s: expected error", tc.args)
		}
	}
}
