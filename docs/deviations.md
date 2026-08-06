# Deliberate deviations from GitHub Actions

Bug-for-bug compatibility is a non-goal. Where GitHub Actions' behaviour is
actively bad we deviate — and every deviation is surfaced in the UI at the point
it matters, not buried here.

Anything we do **not** implement fails the run with `unsupported: X`. Silently
ignoring a key is never a deviation, it is a defect; see
`docs/compatibility.md` for the supported surface.

## Status reporting

### `infra_failure` maps to the Checks API conclusion `action_required`

GitHub Actions has no way to say "this failed and it was not your fault", so
every failure is a red X. We add the `infra_failure` conclusion and report it as
`action_required`, which renders distinctly from `failure`. `config_error` does
the same.

Rationale: a red build must mean the user's code is broken. Reusing `failure`
for an infrastructure fault would reintroduce the exact ambiguity the platform
exists to remove. On the legacy commit-status API both map to `error` rather
than `failure`, which is that API's own "something went wrong that isn't your
build" state.

Consequence to be aware of: a branch protection rule requiring a check to be
green will still block on `action_required`. That is intended — the work did not
succeed — but the operator can tell at a glance whether to re-run or to fix
code.

### Steps are a markdown table, not a native expander

A third-party GitHub App cannot render the per-step expander in the checks UI;
that surface is first-party only. We render the step list as a markdown table in
`output.text` (name, conclusion, duration) and point `details_url` at the
platform's own live view. We do not pretend to have the native UI.

### Check-run updates are coalesced

At most one API call per check run per interval (default 2s), plus a final
update on completion that is never dropped. An uncoalesced step executor eats
the API rate limit, and a dropped final update leaves a check run stuck
in_progress forever.

## Artifacts and cache

### `GITHUB_SERVER_URL` must end in `.ghe.com` or `.localhost`

`actions/upload-artifact@v4` and `download-artifact@v4` call `isGhes()`
(`@actions/artifact`, `src/internal/shared/config.ts`) and throw
`GHESNotSupportedError` before issuing a single request unless the
`GITHUB_SERVER_URL` hostname is `github.com`, ends with `.ghe.com`, or ends with
`.localhost`. There is no override.

So the platform's public URL must satisfy that test, and startup fails loudly if
the configured URL would not. This is a constraint imposed by the client, not a
choice; it is recorded here because it looks arbitrary from the outside.

### Artifacts speak Twirp-over-JSON, cache speaks the v1 REST API

Artifact v4 uses `POST {ACTIONS_RESULTS_URL}/twirp/github.actions.results.api.v1.ArtifactService/{Method}`
with JSON bodies, then uploads bytes to a signed URL using Azure Block Blob
semantics. We implement both, including an Azure-Block-Blob-compatible endpoint
in front of our own blob store.

`@actions/cache` selects v1 unless `ACTIONS_CACHE_SERVICE_V2` is set, so the
`_apis/artifactcache` REST surface is the one that matters.

### `ACTIONS_RUNTIME_TOKEN` carries an `scp` claim

The artifact client parses the runtime token as a JWT and requires a claim
`scp` containing `Actions.Results:<runBackendId>:<jobBackendId>`. Our per-job
token therefore carries that claim alongside our own scopes. It still grants no
repository access and expires with the job.

## Runner protocol

### HTTP + JSON long-poll rather than gRPC

The runner protocol is a handful of calls per job. A JSON surface can be driven
with `curl` during an incident, which is worth more here than protobuf's
efficiency. `internal/protocol` is the contract, versioned by `APIVersion`; a
runner announcing an unknown version is rejected rather than half-understood.

We deliberately do not implement GitHub's own runner protocol, so the official
`actions/runner` binary does not connect to this platform. That protocol is
undocumented, and adopting it would forfeit the lease/heartbeat/requeue and
failure-classification semantics that are the point of the project.

## Execution model

### Retry is first-class and declarative

GitHub Actions has no retry primitive; workflows hand-roll one per step. We add
`retry: { attempts, on: [infra], backoff, initial, max, jitter }` on jobs and
steps, defaulting to "infra retries three times with exponential backoff, user
failures never retry, config errors never retry".

This is additive: a workflow that does not use it behaves as it does on GHA,
except that infra failures retry instead of failing the build.

### One job, one fresh Docker-in-Docker sandbox

Each job gets its own container running its own `dockerd`, so no image, network,
or filesystem state leaks between jobs. GitHub's hosted runners give a fresh VM;
self-hosted runners famously do not, and the resulting cross-job contamination
is a class of bug we decline to inherit.
