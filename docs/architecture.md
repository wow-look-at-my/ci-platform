# Architecture

Two binaries, both shipped as containers.

```
GitHub ──webhook──▶ control plane (cmd/ciplatform)
                      │  ├─ internal/github/webhook   signature verification, event dispatch
                      │  ├─ internal/github/app       App JWT + installation tokens
                      │  ├─ internal/workflow         GHA YAML → IR
                      │  ├─ internal/workflow/expr    expression evaluator
                      │  ├─ internal/plan             IR → concrete jobs (matrix, DAG, naming)
                      │  ├─ internal/scheduler        dispatch, leases, retries, concurrency
                      │  ├─ internal/github/checks    check runs (coalesced)
                      │  ├─ internal/github/statuses  legacy commit statuses
                      │  ├─ internal/logstore         append-only logs, streamed
                      │  ├─ internal/artifacts        artifact service
                      │  ├─ internal/cachesvc         actions/cache service
                      │  ├─ internal/oidc             ID tokens + JWKS
                      │  ├─ internal/api              REST + SSE
                      │  └─ internal/webui            embedded UI
                      │
                      ▼ HTTP long-poll, mutually authenticated
                   runner agent (cmd/ci-runner, one per host)
                      └─ per job: fresh DinD container
                           ├─ its own dockerd (isolated image cache + network)
                           ├─ workspace volume
                           └─ step executor
```

## The layering rule

`internal/model` is the vocabulary. Everything imports it; it imports nothing of
ours. `internal/store` is the persistence contract, `internal/protocol` the
runner wire contract. Those three are the frozen middle that lets the rest be
worked on independently.

The parser targets `model.Workflow` — an IR, deliberately not a GitHub Actions
AST. Nothing downstream of the parser knows what YAML looks like, which is what
makes a second frontend possible without touching the scheduler or the executor.
See `docs/format-trajectory.md`.

## Request paths

**A push arrives.** `github/webhook` verifies the HMAC and dispatches. The
control plane fetches the repo's workflow files at the head SHA, parses each to
IR, and rejects the run with `config_error` if anything is unsupported — never
silently ignoring a key. `plan.Build` expands matrices and resolves the DAG.
`scheduler.StartRun` creates one job per node and enqueues the ready ones.
`github/checks` creates a check run per job, named exactly as GHA would name it.

**A runner picks up work.** The agent long-polls `/runner/v1/acquire`.
`store.Dequeue` claims a job atomically (`FOR UPDATE SKIP LOCKED`) and takes a
lease. The assignment is keyed `run_id/job_id/attempt`; the agent refuses a key
it has already started, so a redelivery cannot double-execute.

**A job runs.** One fresh DinD container, its own `dockerd`, a workspace volume.
Setup is timed by phase. Steps stream logs back in batches; every failure goes
through `internal/classify` and the decision is written into the log.

**Something goes wrong.** If the class is `infra` and the retry policy permits,
the scheduler requeues with backoff and the attempt number is visible in the UI
and the check run. If the runner disappears, its lease expires, `ReapExpiredLeases`
requeues the job with actor `runner_lost`, and the job is neither failed nor
lost. If anything cancels, a `model.CancelReason` with an actor and a human
sentence is recorded and surfaced.

## Why the control plane never lies

Three mechanisms, each of which is a type rather than a convention:

- `model.CancelReason` cannot be constructed without an actor and a sentence,
  and `Validate` rejects one that lacks either.
- `model.Aggregate` is the only reducer from many conclusions to one. It returns
  neutral for zero units of work and refuses to launder an all-skipped set into
  a success.
- `classify.Decision` carries the rule that matched and the text it matched, so
  "this was infrastructure" is a claim with evidence attached rather than an
  assertion.

## Storage

Postgres holds run, job, and step metadata plus the durable queue; the schema is
migration-driven and forward-only, and a migration whose checksum changed is
refused rather than silently reapplied. Logs, artifacts, and cache objects go to
a blob store — `internal/blob/disk` for single-node, `internal/blob/s3` for
anything larger. Cache eviction is always logged with what was removed.

`internal/store/mem` exists for tests and reports `Durable() false`, which the
control plane logs loudly at startup and surfaces in `/healthz`.

## Security boundary

The job container is the isolation boundary. It gets no host docker socket and
no control-plane credentials beyond a per-job token scoped to that job's
artifacts, cache, logs, and OIDC. Installation tokens never reach a job.
Secrets are injected per job, masked by value in every log line before it leaves
the runner process, and never written into the workspace. Fork PRs get no
secrets and no OIDC, and need explicit approval — the same posture as GHA.
