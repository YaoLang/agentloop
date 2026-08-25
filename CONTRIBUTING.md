# Contributing

Thanks for considering a contribution. This is a small, Go-first project — keep it that way.

## Rules

1. **No network in tests.** `go test ./...` must stay hermetic (mock model only).
2. **Do not weaken the sandbox.** Jail and timeout tests are load-bearing. If you change `internal/sandbox`, those tests must still prove path escape is refused and deadlines kill the process.
3. **Eval suite is the contract.** If you add a tool or change loop behavior, add a case under `evals/suites/` and keep the success rate at 100% on the mock.
4. **No secrets, no vendor lock-in, no framework soup.** Stdlib first. The OpenAI client is a thin HTTP wrapper behind a `Model` interface.
5. **Original work only.** Do not paste employer or internal code.

## Dev loop

```bash
go test ./...
go run ./cmd/agentloop demo
go run ./cmd/agentloop eval --suite evals/suites/basic.jsonl
```

Open a PR against `main` with a short "why" and the test/eval output.
