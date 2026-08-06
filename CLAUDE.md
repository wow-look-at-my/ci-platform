# ci-platform

A self-hosted CI platform that ingests GitHub Actions workflow YAML unchanged, executes every job in a fresh Docker-in-Docker sandbox, and reports
per-job
status into GitHub's checks UI. Go control plane + Go runner agent, TypeScript UI bundled by esbuild and `go:embed`ed.

## The point, in one line

It never lies about what happened, and it distinguishes its own failures from yours. A red build means your code is broken; anything else gets a
different
conclusion and a sentence explaining itself. Read `docs/incidents.md` first -- those five incidents are the acceptance criteria, not background.

## Non-negotiables (each is enforced by a type or a test, not by convention)

- Infrastructure failure is never reported as build failure. `internal/classify` decides user/infra/config with recorded evidence.
- - Nothing is cancelled without a reason. `model.CancelReason` cannot be constructed without an actor and a human sentence; `Validate` rejects one
  that lacks
  either.
- A job whose runner disappears is requeued, never lost and never failed. Leases + heartbeats; `ReapExpiredLeases` is the mechanism.
- - Green means the work ran. `model.Aggregate` is the only reducer; it returns neutral for zero work and refuses to launder an all-skipped set into
  success.
- The queue is durable and dispatch is idempotent on `(run_id, job_id, attempt)`.
- A key the parser does not implement fails the run with `unsupported: X`. Silently ignoring `if:` is worse than not running.

## Layout

- `internal/model` -- the shared vocabulary: status, conclusion, failure class, cancel reason, and the IR the parser targets. Imports nothing of ours.
- `internal/store` -- persistence contract: durable queue, lease protocol. Implementations in `store/pg` (production) and `store/mem` (tests, reports
  `Durable() false`).
- `internal/protocol` -- the runner wire contract. HTTP+JSON long-poll; cancellation always carries its reason.
- `internal/classify` -- the user/infra/config classifier and its rule set.
- `internal/config` -- startup configuration; reports every problem at once and refuses a public URL the artifact client would reject.
- - `internal/ingest` -- webhook event to runs: discover workflows at the SHA, parse, match triggers, plan, start. A workflow it cannot run fails
  visibly.
- `internal/runnerapi` -- the control-plane side of the runner protocol, where a cancellation always carries its reason and a lost lease is told so.
- `internal/workflow`, `internal/workflow/expr` -- GHA YAML frontend and the expression evaluator.
- `internal/plan`, `internal/scheduler` -- matrix expansion and DAG, then dispatch, leases, retries, concurrency, rollup.
- `internal/github` -- App auth, webhook ingest, check runs (coalesced), legacy commit statuses.
- - `internal/blob`, `internal/logstore`, `internal/artifacts`, `internal/cachesvc`, `internal/oidc`, `internal/jobtoken` -- the services unmodified
  actions
  talk to.
- `internal/api`, `internal/webui`, `web-src`, `cmd/buildweb` -- REST + SSE, and the embedded UI.
- `internal/runner`, `cmd/ci-runner` -- the agent, the DinD sandbox, the step executor.
- - `migrations` -- forward-only numbered SQL, `go:embed`ed and applied by the runner in `internal/store/pg`; an edited applied file is a hard stop,
  not a
  silent divergence.
- `internal/store/storetest` -- one conformance suite both stores run, so "works in memory, breaks in Postgres" is a test failure.
- `test/fakes`, `test/chaos`, `test/e2e`, `test/conformance` -- test doubles and the suites that hold the non-negotiables up.

## Docs

- `docs/incidents.md` -- the five failures this exists to prevent, and which test covers each.
- `docs/architecture.md` -- the two binaries, the request paths, the layering rule.
- `docs/compatibility.md` -- the GHA compatibility matrix: supported, unsupported, deviating.
- `docs/deviations.md` -- every deliberate difference from GHA, and the client-imposed constraints we verified rather than assumed.
- `docs/format-trajectory.md` -- why the parser targets an IR and what a native frontend would fix.

## Working here

- Go validation runs through `go-toolchain` (no bare `go build`/`go test`); coverage gate is 80%.
- Never name a job, workflow, or check run `all-builds` -- the org app owns that context and a job wearing the name only shadows it.
- Never write a "not configured yet" mode that idles green. Missing config fails loudly at startup, naming the field.
- `web/` is the committed esbuild output that `go:embed` ships; `go run ./cmd/buildweb -check` fails CI if it is stale.
