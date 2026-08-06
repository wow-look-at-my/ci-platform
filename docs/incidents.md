# The incidents this platform exists to prevent

These are not background. They are the acceptance criteria, and each one has a
test in `test/chaos/` that fails if the behaviour regresses.

## 1. Silent infrastructure failure reported as build failure

A registry blob upload died with `failed: 524 : error code: 524` — a Cloudflare
origin timeout. Three of six matrix legs failed. Nothing distinguished "the
network ate this" from "your code is broken", and the aggregate status read
`5/9 builds failed`.

**What we do instead.** `internal/classify` turns every failure into a
`user` / `infra` / `config` decision with recorded evidence. Infra failures
retry with backoff, and if they exhaust retries they get their own conclusion
(`infra_failure`) which is deliberately *not* the red X a broken build gets.
The classification decision — the rule that matched and the text it matched —
is written into the job log and into the check run, so the operator can see why
something was called infra rather than having to trust it.

Test: `test/chaos` returns 524 from a fake registry and asserts the attempt is
classified infra, retried, and never surfaced as a build failure.

## 2. Control plane unable to start jobs

Runner logs showed `POST .../runnerresolve/actions failed (HTTP Status:
BadGateway)`, then `ServiceUnavailable`, retried three times with backoff, and
the job never started. From the outside the run simply sat there.

**What we do instead.** A runner that cannot reach the control plane classifies
that as infra by construction — there is no user code in the dispatch path. The
job stays queued rather than failing, the queue page shows it queued and shows
that its label has no reachable runners, and the check run says so.

Test: `test/chaos` makes the control plane unreachable mid-job and asserts the
job reattaches by lease rather than failing.

## 3. A run attempt cancelled mid-flight with no reason anywhere

Attempt 2 went from in-progress to `cancelled`. No log line, no annotation, no
UI explanation. Working out what happened meant diffing job lists across
attempts.

**What we do instead.** `model.CancelReason` cannot be constructed without an
actor and a human sentence, and `Validate` rejects one that lacks either. Every
cancellation writes an event row, a log line, and text in the check run output.
There is exactly one cancel helper in the scheduler and it validates.

Test: `test/chaos` cancels a run and asserts the reason is present in the job
log, the event timeline, the API, and the check run output.

## 4. Job setup taking 5.5 minutes before the first step ran

No queue depth, no runner-availability signal, nothing to explain the wait.

**What we do instead.** Setup is a measured phase, not an inference from
timestamps. The runner reports a breakdown (`container_create`, `dockerd_ready`,
`image_pull`, `workspace_prepare`) and whether the image cache was warm. Every
job page shows queued / setup / execute as three numbers, and the queue page
shows depth over time, the oldest waiting job, and per-label starvation.

## 5. A merged, green PR that never published

CI passed, the deploy image was never built, and the only symptom was a
user-visible bug that persisted. Nothing reported that the publish had failed
on the default branch.

**What we do instead.** A run on the repo's default branch that ends non-success
fires `scheduler.Options.Notify`. This is the one alarm that is on by default,
because a failure nobody is told about is the failure that costs the most.

## The rule these share

The platform never reports a status the work has not earned. `model.Aggregate`
refuses to call zero units of work a success, refuses to launder an all-skipped
set into a green, and reports the worst thing that happened rather than the most
convenient. A red build means your code is broken; anything else gets a
different colour and a sentence explaining itself.
