package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJailPath(t *testing.T) {
	ws := t.TempDir()
	inside := filepath.Join(ws, "ok.txt")

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"relative file", "foo.txt", true},
		{"nested relative", "a/b/c.txt", true},
		{"dot current", ".", true},
		{"abs inside workspace", inside, true},
		{"abs etc passwd", "/etc/passwd", false},
		{"dotdot escape", "../../../etc/passwd", false},
		{"dotdot sibling", "../outside.txt", false},
		{"empty", "", false},
		{"rooted fake join", "/etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JailPath(ws, tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("JailPath(%q) unexpected error: %v", tc.in, err)
				}
				rel, err := filepath.Rel(ws, got)
				if err != nil || strings.HasPrefix(rel, "..") {
					t.Fatalf("resolved %q is outside workspace: %s", tc.in, got)
				}
			} else if err == nil {
				t.Fatalf("JailPath(%q) = %q, want escape error", tc.in, got)
			} else if !errors.Is(err, ErrPathEscape) {
				t.Fatalf("JailPath(%q) err = %v, want ErrPathEscape", tc.in, err)
			}
		})
	}
}

func TestRunRefusesPathEscape(t *testing.T) {
	ws := t.TempDir()
	cases := [][]string{
		{"cat", "/etc/passwd"},
		{"cat", "../../../etc/passwd"},
		{"cat", filepath.Join(ws, "..", "outside")},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			_, err := Run(context.Background(), argv, Limits{Workspace: ws, Timeout: time.Second})
			if !errors.Is(err, ErrPathEscape) {
				t.Fatalf("err = %v, want ErrPathEscape", err)
			}
		})
	}
}

func TestRunRefusesDeniedBinary(t *testing.T) {
	ws := t.TempDir()
	cases := []string{"ssh", "sudo", "curl", "nmap", "/bin/sh", "./evil"}
	for _, bin := range cases {
		t.Run(bin, func(t *testing.T) {
			_, err := Run(context.Background(), []string{bin, "x"}, Limits{Workspace: ws})
			if !errors.Is(err, ErrDeniedBin) {
				t.Fatalf("err = %v, want ErrDeniedBin", err)
			}
		})
	}
}

func TestRunTimeout(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()
	res, err := Run(context.Background(), []string{"sleep", "8"}, Limits{
		Workspace: ws,
		Timeout:   200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut flag not set")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestRunEchoInsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	res, err := Run(context.Background(), []string{"echo", "hello-jail"}, Limits{
		Workspace: ws,
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-jail") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestRunCatWorkspaceFile(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "note.txt")
	if err := os.WriteFile(p, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), []string{"cat", "note.txt"}, Limits{
		Workspace: ws,
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "inside" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestStdoutCap(t *testing.T) {
	ws := t.TempDir()
	// printf a 200-byte string, cap at 32 bytes.
	res, err := Run(context.Background(), []string{"printf", strings.Repeat("A", 200)}, Limits{
		Workspace: ws,
		Timeout:   time.Second,
		MaxOutput: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected truncation")
	}
	if len(res.Stdout) > 32 {
		t.Fatalf("stdout len %d > cap", len(res.Stdout))
	}
}

func TestCheckBinaryTable(t *testing.T) {
	cases := []struct {
		name  string
		bin   string
		allow []string
		deny  []string
		ok    bool
	}{
		{"echo allowed", "echo", nil, nil, true},
		{"ssh denied by default", "ssh", nil, nil, false},
		{"explicit deny wins", "echo", nil, []string{"echo"}, false},
		{"custom allow", "custom", []string{"custom"}, nil, true},
		{"abs path", "/bin/echo", nil, nil, false},
		{"rel path", "./echo", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBinary(tc.bin, tc.allow, tc.deny)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrDeniedBin) {
				t.Fatalf("err = %v, want ErrDeniedBin", err)
			}
		})
	}
}
