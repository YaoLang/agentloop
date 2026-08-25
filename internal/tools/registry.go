package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/YaoLang/agentloop/internal/model"
)

// Handler executes a tool. The returned string is the observation.
type Handler func(ctx context.Context, argsJSON string) (string, error)

// Tool is one registered capability.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Timeout     time.Duration
	Handler     Handler
}

// Registry is a name → tool map with an allow/deny gate.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
	deny  map[string]bool
}

// NewRegistry is empty.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Tool{}, deny: map[string]bool{}}
}

// Register adds or replaces a tool.
func (r *Registry) Register(t *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
}

// Deny marks a tool name as uncallable (allow/deny switch).
func (r *Registry) Deny(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deny[name] = true
}

// Allow clears a previous deny.
func (r *Registry) Allow(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deny, name)
}

// Get returns a tool or false.
func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Denied reports the allow/deny gate.
func (r *Registry) Denied(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.deny[name]
}

// Specs is the list advertised to the model.
func (r *Registry) Specs() []model.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]model.ToolSpec, 0, len(names))
	for _, n := range names {
		if r.deny[n] {
			continue
		}
		t := r.tools[n]
		out = append(out, model.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		})
	}
	return out
}

// Validate checks name existence, deny list, and JSON schema.
func (r *Registry) Validate(name, argsJSON string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.deny[name] {
		return fmt.Errorf("tool %q is denied", name)
	}
	t, ok := r.tools[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	return ValidateArgs(t.Schema, argsJSON)
}

// Call validates and runs a tool with its timeout.
// Handler panics are recovered so a buggy tool cannot crash the process.
func (r *Registry) Call(ctx context.Context, name, argsJSON string) (obs string, err error) {
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = r.Validate(name, argsJSON); err != nil {
		return "", err
	}
	r.mu.RLock()
	t := r.tools[name]
	r.mu.RUnlock()
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	defer func() {
		if rec := recover(); rec != nil {
			obs = ""
			err = fmt.Errorf("tool panic: %v", rec)
		}
	}()
	return t.Handler(ctx, argsJSON)
}
