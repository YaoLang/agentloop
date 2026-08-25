// Package sandbox is a process jail: workspace path confinement, binary
// allow-list, wall-clock timeout, and stdout/stderr caps. It does not
// require Docker or root — it is a portable OS-process sandbox.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrPathEscape is returned when a path argument leaves the workspace.
	ErrPathEscape = errors.New("sandbox: path escapes workspace")
	// ErrDeniedBin is returned when the binary is not on the allow-list
	// (or is explicitly denied).
	ErrDeniedBin = errors.New("sandbox: binary not allowed")
	// ErrTimeout is returned when the process exceeds Limits.Timeout.
	ErrTimeout = errors.New("sandbox: command timed out")
	// ErrEmptyCommand is returned when argv is empty.
	ErrEmptyCommand = errors.New("sandbox: empty command")
)

// DefaultAllow is the default set of binaries the exec tool may invoke.
// Keep this list boring and local: no network clients, no privilege tools.
var DefaultAllow = []string{
	"echo", "sleep", "true", "false", "cat", "ls", "pwd",
	"date", "wc", "head", "mkdir", "touch", "printf",
	"basename", "dirname", "tr", "sort", "uniq",
}

// Limits configure one sandboxed process.
type Limits struct {
	Workspace string
	Timeout   time.Duration
	MaxOutput int // bytes per stream; 0 → 64KiB
	Allow     []string
	Deny      []string
}

// Result is the observed outcome of a process.
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	TimedOut  bool
	Duration  time.Duration
}

// JailPath resolves userPath against workspace and refuses any result
// that is not inside the workspace (after Clean). Absolute paths are
// allowed only if they already sit under the workspace.
func JailPath(workspace, userPath string) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	ws = filepath.Clean(ws)

	var full string
	if filepath.IsAbs(userPath) {
		full = filepath.Clean(userPath)
	} else {
		full = filepath.Clean(filepath.Join(ws, userPath))
	}

	rel, err := filepath.Rel(ws, full)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrPathEscape, userPath)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrPathEscape, userPath)
	}
	return full, nil
}

// CheckBinary enforces the allow/deny lists. Absolute binary paths and
// relative paths that contain a separator are refused — only bare names
// from the allow-list may run (resolved via PATH).
func CheckBinary(name string, allow, deny []string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty binary", ErrDeniedBin)
	}
	base := filepath.Base(name)
	if name != base {
		return fmt.Errorf("%w: %s (bare name required)", ErrDeniedBin, name)
	}
	for _, d := range deny {
		if base == d {
			return fmt.Errorf("%w: %s", ErrDeniedBin, base)
		}
	}
	if len(allow) == 0 {
		allow = DefaultAllow
	}
	for _, a := range allow {
		if base == a {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrDeniedBin, base)
}

// LooksLikePath reports whether an argument should be treated as a
// filesystem path and therefore jailed.
func LooksLikePath(s string) bool {
	if s == "" {
		return false
	}
	if filepath.IsAbs(s) {
		return true
	}
	if strings.Contains(s, "..") {
		return true
	}
	return strings.ContainsAny(s, `/\`)
}

// Run starts argv inside the workspace jail. On path-escape or denied
// binary it returns before spawning a process. On timeout it kills the
// process group via CommandContext and returns ErrTimeout.
func Run(ctx context.Context, argv []string, lim Limits) (Result, error) {
	if len(argv) == 0 {
		return Result{}, ErrEmptyCommand
	}
	if lim.Timeout <= 0 {
		lim.Timeout = 5 * time.Second
	}
	if lim.MaxOutput <= 0 {
		lim.MaxOutput = 64 * 1024
	}
	ws, err := filepath.Abs(lim.Workspace)
	if err != nil {
		return Result{}, err
	}

	if err := CheckBinary(argv[0], lim.Allow, lim.Deny); err != nil {
		return Result{}, err
	}
	for _, a := range argv[1:] {
		if LooksLikePath(a) {
			if _, err := JailPath(ws, a); err != nil {
				return Result{}, err
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = ws
	// Minimal env: PATH for allow-listed binaries only. Do not inherit
	// the daemon process environment (ADMIN_KEY, JWT, OPENAI_API_KEY,
	// tenant secrets). `echo $TOKEN` / printenv must not see them.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}

	var stdout, stderr bytes.Buffer
	outW := &capWriter{buf: &stdout, n: lim.MaxOutput}
	errW := &capWriter{buf: &stderr, n: lim.MaxOutput}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	runErr := cmd.Run()
	res := Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: outW.trunc || errW.trunc,
		Duration:  time.Since(start),
	}

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("%w after %s: %s", ErrTimeout, lim.Timeout, strings.Join(argv, " "))
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, runErr
	}
	return res, nil
}

type capWriter struct {
	buf   *bytes.Buffer
	n     int
	wrote int
	trunc bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	remain := c.n - c.wrote
	if remain <= 0 {
		c.trunc = true
		return len(p), nil
	}
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		c.wrote += remain
		c.trunc = true
		return len(p), nil
	}
	n, err := c.buf.Write(p)
	c.wrote += n
	return n, err
}
