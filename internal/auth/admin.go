package auth

import "net/http"

const (
	// AdminTenant is the tenant id of the process admin principal.
	AdminTenant = "_admin"
	// ScopeAdmin gates /v1/admin/*.
	ScopeAdmin = "admin"
	// ScopeRunsWrite gates POST /v1/runs.
	ScopeRunsWrite = "runs:write"
	// ScopeRunsRead gates GET /v1/runs/{id} (runs:write also satisfies).
	ScopeRunsRead = "runs:read"
)

// Admin authenticates the process-wide AGENTLOOP_ADMIN_KEY.
// On mismatch it skips so other plugins can try.
type Admin struct {
	Key string
}

// NewAdmin returns an admin authenticator. An empty key always skips.
func NewAdmin(key string) *Admin { return &Admin{Key: key} }

func (a *Admin) Name() string { return "admin" }

func (a *Admin) Authenticate(r *http.Request) (Principal, error) {
	if a == nil || a.Key == "" {
		return Principal{}, ErrSkip
	}
	tok, ok := BearerToken(r)
	if !ok {
		return Principal{}, ErrSkip
	}
	if !TimingSafeEqual(tok, a.Key) {
		return Principal{}, ErrSkip
	}
	return Principal{
		TenantID: AdminTenant,
		Subject:  "admin",
		Scopes:   []string{ScopeAdmin, ScopeRunsWrite, ScopeRunsRead},
	}, nil
}
