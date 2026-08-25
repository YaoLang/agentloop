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

func TestCallRecoversPanic(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{
		Name: "boom",
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			panic("kaboom")
		},
	})
	obs, err := reg.Call(context.Background(), "boom", `{}`)
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("obs=%q err=%v", obs, err)
	}
	if obs != "" {
		t.Fatalf("observation should be empty on panic, got %q", obs)
	}
}

func TestCallCanceledContext(t *testing.T) {
	reg := NewRegistry()
	called := false
	reg.Register(&Tool{
		Name: "x",
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			called = true
			return "ok", nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := reg.Call(ctx, "x", `{}`)
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if called {
		t.Fatal("handler must not run when ctx is already done")
	}
}

func TestWhoamiLocalWithoutRuntime(t *testing.T) {
	reg, _, _ := setup(t)
	out, err := reg.Call(context.Background(), "whoami", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "local") {
		t.Fatalf("want tenant=local, out=%q", out)
	}
}

func TestWhoamiWithRuntime(t *testing.T) {
	reg, _, _ := setup(t)
	const secret = "gho_must_not_appear"
	ctx := WithRuntime(context.Background(), Runtime{
		TenantID: "acme",
		Subject:  "user-1",
		Scopes:   []string{"runs:write", "admin"},
		Secret: func(name string) (string, bool) {
			if name == "github" {
				return secret, true
			}
			return "", false
		},
	})
	out, err := reg.Call(ctx, "whoami", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"tenant_id":"acme"`) {
		t.Fatalf("tenant missing: %s", out)
	}
	if !strings.Contains(out, `"subject":"user-1"`) {
		t.Fatalf("subject missing: %s", out)
	}
	if !strings.Contains(out, "runs:write") {
		t.Fatalf("scopes missing: %s", out)
	}
	if strings.Contains(out, secret) || strings.Contains(out, "github") {
		t.Fatalf("whoami must not mention secrets, out=%q", out)
	}
}

func TestExtraToolsRegistered(t *testing.T) {
	ws := t.TempDir()
	mem, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	reg := Default(Options{
		Workspace: ws,
		Memory:    mem,
		Extra: []*Tool{{
			Name: "ping",
			Handler: func(ctx context.Context, argsJSON string) (string, error) {
				return "pong", nil
			},
		}},
	})
	if _, ok := reg.Get("ping"); !ok {
		t.Fatal("extra tool not registered")
	}
	var names []string
	for _, spec := range reg.Specs() {
		names = append(names, spec.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "ping") {
		t.Fatalf("extra tool not advertised: %v", names)
	}
	out, err := reg.Call(context.Background(), "ping", `{}`)
	if err != nil || out != "pong" {
		t.Fatalf("call ping: out=%q err=%v", out, err)
	}
}

func TestHasSecretPresentAbsentNeverLeaksValue(t *testing.T) {
	reg := NewRegistry()
	reg.Register(HasSecretTool())
	const secret = "gho_xxx_super_secret"
	ctxA := WithRuntime(context.Background(), Runtime{
		TenantID: "alpha",
		Secret: func(name string) (string, bool) {
			if name == "github" {
				return secret, true
			}
			return "", false
		},
	})
	out, err := reg.Call(ctxA, "has_secret", `{"name":"github"}`)
	if err != nil || out != "present" {
		t.Fatalf("alpha github: out=%q err=%v", out, err)
	}
	if strings.Contains(out, secret) {
		t.Fatal("secret value leaked in observation")
	}
	out, err = reg.Call(ctxA, "has_secret", `{"name":"missing"}`)
	if err != nil || out != "absent" {
		t.Fatalf("alpha missing: out=%q err=%v", out, err)
	}
	ctxB := WithRuntime(context.Background(), Runtime{
		TenantID: "beta",
		Secret: func(name string) (string, bool) {
			return "", false
		},
	})
	out, err = reg.Call(ctxB, "has_secret", `{"name":"github"}`)
	if err != nil || out != "absent" {
		t.Fatalf("beta github: out=%q err=%v", out, err)
	}
	out, err = reg.Call(context.Background(), "has_secret", `{"name":"github"}`)
	if err != nil || out != "absent" {
		t.Fatalf("no runtime: out=%q err=%v", out, err)
	}
}

func TestNilSecretLookupIsAbsent(t *testing.T) {
	ctx := WithRuntime(context.Background(), Runtime{TenantID: "acme"})
	rt, ok := RuntimeFrom(ctx)
	if !ok {
		t.Fatal("missing runtime")
	}
	_, present := rt.Secret("github")
	if present {
		t.Fatal("nil Secret should look up as absent")
	}
}

func TestExecJailDoesNotSeeRuntimeSecrets(t *testing.T) {
	ws := t.TempDir()
	mem, err := memory.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	allow := append(append([]string{}, sandbox.DefaultAllow...), "printenv")
	reg := Default(Options{Workspace: ws, Memory: mem, ToolTimeout: time.Second, AllowBins: allow})
	const secret = "gho_jail_must_not_see"
	ctx := WithRuntime(context.Background(), Runtime{
		TenantID: "acme",
		Secret: func(name string) (string, bool) {
			if name == "github" || name == "TOKEN" {
				return secret, true
			}
			return "", false
		},
	})
	out, err := reg.Call(ctx, "exec", `{"command":["printenv","TOKEN"]}`)
	if strings.Contains(out, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
		t.Fatalf("TOKEN leaked into jail: out=%s err=%v", out, err)
	}
	out, err = reg.Call(ctx, "exec", `{"command":["printenv","github"]}`)
	if strings.Contains(out, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
		t.Fatalf("github leaked into jail: out=%s err=%v", out, err)
	}
	out, err = reg.Call(ctx, "exec", `{"command":["echo","hi"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked into echo observation: %s", out)
	}
}
