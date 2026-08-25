package tools

import "context"

type runtimeKey struct{}

// Runtime is the authenticated tenant environment for in-process Go
// tool handlers. It is stored on context and must never be serialized
// into model messages, tool observations, or the exec jail environment.
//
// Secret looks up a per-tenant named secret. Missing names return
// ok=false. Never log the returned values; never print them in
// observations; never copy them into sandbox exec env (so `echo $TOKEN`
// / printenv inside the jail cannot see tenant secrets).
type Runtime struct {
	TenantID string
	Subject  string
	Scopes   []string
	Secret   func(name string) (string, bool)
}

// WithRuntime stores rt on ctx. A nil Secret is replaced with a lookup
// that always returns ok=false.
func WithRuntime(ctx context.Context, rt Runtime) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.Secret == nil {
		rt.Secret = func(string) (string, bool) { return "", false }
	}
	if rt.Scopes != nil {
		rt.Scopes = append([]string(nil), rt.Scopes...)
	}
	return context.WithValue(ctx, runtimeKey{}, rt)
}

// RuntimeFrom returns the Runtime previously stored with WithRuntime.
func RuntimeFrom(ctx context.Context) (Runtime, bool) {
	if ctx == nil {
		return Runtime{}, false
	}
	rt, ok := ctx.Value(runtimeKey{}).(Runtime)
	return rt, ok
}

// lookupSecret is a nil-safe Secret call. Missing Runtime or nil Secret
// is absent. Handlers should prefer rt.Secret after RuntimeFrom; this
// helper exists so a zero Runtime does not panic.
func (rt Runtime) lookupSecret(name string) (string, bool) {
	if rt.Secret == nil {
		return "", false
	}
	return rt.Secret(name)
}
