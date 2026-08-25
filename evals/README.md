# Evals

The harness scores **behavior**, not vibes. Every case in `suites/basic.jsonl` is a JSON object with a scripted mock model, so `go test ./...` and `agentloop eval` stay hermetic (no network, no API key).

```bash
go run ./cmd/agentloop eval --suite evals/suites/basic.jsonl
```

`--judge` turns on an optional LLM-as-judge pass. It is **off by default** so CI cannot flake on a model.

## What a case looks like

```json
{
  "id": "write-read",
  "goal": "Write hello to notes.txt then read it back.",
  "max_steps": 5,
  "script": [
    {"tool": "write_file", "args": {"path": "notes.txt", "content": "hello"}},
    {"tool": "read_file", "args": {"path": "notes.txt"}},
    {"content": "notes.txt contains hello"}
  ],
  "expect": {
    "success": true,
    "files": {"notes.txt": "hello"},
    "tools_used": ["write_file", "read_file"],
    "max_steps": 5,
    "max_latency_ms": 8000
  }
}
```

The `script` is what the **mock model** emits. The sandbox, tools, memory, and loop are real. That is the point: we grade the harness, not the LLM.

## Scorers

| Scorer | Passes when |
| --- | --- |
| **Success** | The agent completed (or handled a safety case) and file/memory/tool contracts match. |
| **Tool-schema validity** | Every tool call's JSON arguments satisfy the registered schema. Unknown or denied tools fail this bit. |
| **Jail / path-escape** | If `expect.jail_caught` is true, the sandbox must have refused the path or binary. |
| **Timeout** | If `expect.timeout_caught` is true, a tool must have hit its deadline. |
| **Latency** | Wall clock for the case is under `expect.max_latency_ms` (default 15s). |
| **Step count** | Model turns ≤ `expect.max_steps`. |

A case **PASS**es only if every applicable scorer is true. `go test ./internal/eval` fails the package if the suite success rate drops below 100%.

## Suite map (`basic.jsonl`)

| ID | What it proves |
| --- | --- |
| `write-read` | Workspace-scoped file tools |
| `exec-echo` | Allow-listed process exec |
| `jail-abs-path` | `read_file /etc/passwd` is refused |
| `jail-dotdot` | `../` escape is refused |
| `jail-exec-abs` | `exec cat /etc/passwd` is refused |
| `write-jail` | `write_file` cannot leave the workspace |
| `denied-binary` | `ssh` (and friends) cannot run |
| `timeout-sleep` | `sleep` is killed at the tool deadline |
| `memory-recall` | Session memory write → read |
| `longterm-memory` | Append-only on-disk notes |
| `multi-step` | Four-tool chain, one goal |
| `schema-valid` | Well-formed arguments stay green |
| `nested-write` | Parent directories are created inside the jail |
| `exec-cwd-escape` | `cwd` is jailed the same way as paths |

## Adding a case

1. Append one JSON line to `suites/basic.jsonl`.
2. Keep the mock script aligned with `expect`.
3. Run `go test ./...` — if success rate drops, the PR is red on purpose.
