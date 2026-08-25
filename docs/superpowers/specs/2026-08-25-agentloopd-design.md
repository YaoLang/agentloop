# agentloopd — multi-tenant daemon

Date: 2026-08-25  
Status: implemented  
Module: `github.com/YaoLang/agentloop`  
Binary: `cmd/agentloopd`  
Packages: `internal/auth`, `internal/daemon`

This is original OSS. It is not affiliated with any employer.

## Goal

One process serving many tenants. Auth is a plugin chain. Isolation is the
existing process jail (`internal/sandbox`) — **no Docker**. Each tenant's
`agent.Run` is pointed at `data/tenants/{id}/workspace` with a fresh
`tools.Registry`. The local CLI (`cmd/agentloop`) is unchanged.

## Flags and env

```
agentloopd -addr :8080 -data ./data -model mock
```

| Env | Role |
| --- | --- |
| `AGENTLOOP_ADMIN_KEY` | Bearer token → principal `tenant=_admin`, scopes `admin`, `runs:write`, `runs:read` |
| `AGENTLOOP_JWT_SECRET` | HS256 secret for the JWT plugin |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | only when a run requests `--model openai` |

Never commit `data/` (hashes and workspaces). Never log raw keys.

## Data layout

```
data/
  keys.json                          # SHA-256 hashes only; mode 0600
  tenants/{tenantID}/meta.json
  tenants/{tenantID}/workspace/      # agent.Run Workspace
  tenants/{tenantID}/runs/{runID}/status.json
  tenants/{tenantID}/runs/{runID}/events.jsonl
```

Tenant IDs and run IDs are single path components (`[A-Za-z0-9_-]{1,64}`).
`_admin` is reserved (cannot be created via the API). Cross-tenant lookup
never walks another tenant's directory: a run is loaded only from
`tenants/{caller.TenantID}/runs/{id}`. Missing or foreign IDs return 404
without contents.

## Auth plugins (`internal/auth`)

```go
type Principal struct {
    TenantID string
    Subject  string
    Scopes   []string
}

type Authenticator interface {
    Name() string
    Authenticate(r *http.Request) (Principal, error) // ErrSkip if this method's creds are absent
}

type Chain []Authenticator // first non-skip success wins
```

Default chain (first match wins; non-skip error is 401):

1. **Admin** — `Authorization: Bearer $AGENTLOOP_ADMIN_KEY`. Timing-safe
   compare (SHA-256 then `subtle.ConstantTimeCompare`). Mismatch **skips**
   so API keys / JWT can still succeed.
2. **API key** — Bearer `alk_…`. Skip if the prefix is absent. SHA-256 the
   presented secret; timing-safe compare against every hash in
   `data/keys.json`. Hit → principal `{TenantID, Subject: key id, Scopes}`.
   Miss → 401. The plaintext secret is returned **once** at mint and never
   stored or logged.
3. **JWT HS256** — skip if secret unset, if token is `alk_…`, or if it is
   not three base64url parts. Claims: `sub`, `tid`, `scp` (array or
   space-separated string). `alg` must be HS256 (`none` rejected). Optional
   `exp`. Invalid signature / expired / unsafe `tid` → 401.

HTTP mapping:

- 401 missing or invalid credentials (`WWW-Authenticate: Bearer`)
- 403 authenticated but wrong scope (e.g. non-admin hitting `/v1/admin/*`)
- 404 run IDs that do not belong to the caller (no leak)

### Adding an Authenticator

Implement `auth.Authenticator` in a new file under `internal/auth`. Skip
(`return Principal{}, ErrSkip`) when your credential type is absent so
other plugins can run. Return `ErrUnauthorized` when your credential type
**is** present but invalid. Insert the plugin into the default chain in
`daemon.New` (or pass `Config.Auth` in tests):

```go
chain := auth.Chain{
    auth.NewAdmin(cfg.AdminKey),
    auth.NewAPIKey(ks),
    auth.NewJWT(cfg.JWTSecret),
    NewMyPlugin(...), // first non-skip success still wins
}
```

Do not log the raw `Authorization` header.

## Isolation

- Workspace = `tenants/{tenantID}/workspace`. The sandbox `JailPath` /
  binary allow-list already refuse `..`, absolute paths outside the
  workspace, and non-allow-listed binaries.
- Fresh `memory.Store` + `tools.Default` registry per run (no shared tool
  state across tenants).
- Per-tenant in-process concurrency, default **8**. `POST /v1/runs`
  returns **429** when the tenant is at the cap. The slot is acquired
  before the goroutine starts and released when the run finishes (or
  panics).
- Tenant A cannot `GET /v1/runs/{B's id}` (404). Tenant B's jail cannot
  resolve A's workspace paths.

## HTTP (`net/http`, Go 1.22 mux)

Unauthenticated:

- `GET /healthz` → `{"ok":true}`

`runs:write`:

- `POST /v1/runs` `{"goal","model":"mock"|"openai"}` → **202** `{"id","status"}`.
  Body `model` overrides `-model`. Runs execute in a goroutine; status is
  persisted to `status.json`.

`runs:write` or `runs:read`:

- `GET /v1/runs/{id}` → run record (caller tenant only)
- `GET /v1/runs/{id}/events` → SSE (`text/event-stream`), replays
  `events.jsonl` then follows until `completed`/`failed`

`admin`:

- `POST /v1/admin/tenants` `{"id","name"}` → 201 meta
- `GET /v1/admin/tenants` → `{"tenants":[...]}`
- `POST /v1/admin/keys` `{"tenant_id","scopes":[]}` → 201
  `{"id","secret","tenant_id","scopes"}` (secret once)

## Run lifecycle

1. Auth + `runs:write` + validate goal/model + tenant exists.
2. Acquire tenant quota (else 429).
3. Persist `status=queued`, append event, return 202.
4. Goroutine: `status=running`, `agent.Run` with tenant workspace, persist
   `completed` or `failed` plus a final event.

The request context is **not** used for the loop (the client disconnects
after 202). A process-wide timeout (`Config.RunTimeout`, default 2m)
bounds the run.

## Tests (hermetic)

See `internal/auth/auth_test.go` and `internal/daemon/server_test.go`:

- 401 missing / wrong key
- JWT happy path
- API key happy path
- Admin mints keys; non-admin cannot
- Isolation: tenant A mock run writes a file; tenant B cannot read that
  path (sandbox jail) or A's run id (404, no contents)
- Concurrency quota → 429
- Existing `go test ./...` (CLI / eval / sandbox) stays green
- `go vet ./...` clean

No network: models are mock or a test double. `httptest` is in-process.
