# Trust boundaries

The starting fact that shapes everything here: **a job container can reach the
control plane.** It has to. `actions/upload-artifact`, `actions/cache` and
`actions/github-script`'s OIDC helper all talk to `CIPLATFORM_PUBLIC_URL`, and
that URL is in the job's environment because we put it there. A fork PR's
workflow is a stranger's code running with that address in hand.

So every route on that listener is either credentialed or safe to hand a
stranger. There is no "internal" surface, because there is no network position
from which the platform is internal.

## What guards each route

| Route | Credential | Held by |
|---|---|---|
| `/webhook` | HMAC-SHA256 over the body | GitHub |
| `/runner/v1/*` | `CIPLATFORM_RUNNER_TOKEN` | runner agents |
| `/twirp/*`, artifact upload/download | per-job token, scoped `artifacts:*` | the running job |
| `/_apis/artifactcache/*` | per-job token, scoped `cache:*` | the running job |
| `/_apis/oidc/token` | per-job token, scoped `oidc:issue` | non-fork jobs only |
| `/api/v1/*`, `/healthz` | `CIPLATFORM_OPERATOR_TOKEN` | operators |
| `/auth/{login,logout,status}` | none — this is where you exchange the credential | anybody |
| `/.well-known/docker-updater/health` | none — a status code and the word `ok` | orchestrators |
| `/`, `/app.mjs`, `/app.css` | none — the shipped bundle, no data in it | anybody |

The operator gate is `internal/operatorauth`. It takes the credential as
`Authorization: Bearer` or as an HttpOnly, SameSite=Strict session cookie set by
`POST /auth/login`; the cookie exists because an `EventSource` log tail and a
download link cannot send a header. Comparison is constant time.

`/healthz` is behind the gate because it names each degraded subsystem. The
docker-updater endpoint stays open so liveness probing needs no secret; it is
status-code-only by contract (see `internal/api/health.go`).

## Credential separation, enforced at startup

`CIPLATFORM_OPERATOR_TOKEN`, `CIPLATFORM_RUNNER_TOKEN` and
`CIPLATFORM_JOB_TOKEN_SECRET` must be three different values, and the control
plane refuses to start if any two match. Each reaches somewhere the others must
not: the runner token sits on every runner host, the signing key mints every
job's credentials, and the operator token is typed into a browser. The operator
token also has a minimum length, because nothing rate-limits guesses at it.

## Job tokens

A job token carries the job's identity, its repository, and a scope list — never
repository write access, and never a GitHub token. Its scopes are decided when
it is minted (`cmd/ciplatform/lookups.go`) and checked again in every handler
that acts on them; a scope the minter withholds but no handler checks is a
permission that exists only in prose.

A fork PR's job is minted without `oidc:issue` and with `cache:read` instead of
`cache:rw`, and its OIDC environment variables are not set at all — the endpoint
is not merely refused, it is not there to be found.

The token's lifetime is the job's own `timeout-minutes`, clamped to the run
timeout, plus the signer's clock-skew grace.

## What this does not defend against

Stated plainly, because a boundary you believe in but do not have is worse than
one you know you lack:

- **One operator identity, not many.** Anybody holding the operator credential
  can read every repository's logs and artifacts and cancel every run. There is
  no per-repository scoping and no per-person audit trail: `X-CI-Actor` on a
  cancel is a display name the caller chooses, not an identity. This is a
  single-organisation deployment (see the README's scope section); if you need
  per-team isolation, run more than one instance.
- **A job token stays valid after its job ends**, up to the job's timeout. The
  container holding it is gone by then, but a token captured mid-run can still
  write to that run's artifacts and cache until it expires. Rejecting tokens for
  completed jobs would need a store lookup on every chunk upload; the lifetime
  bound above is the cheaper half of the fix, and this is the half that is not
  done.
- **Jobs on the same runner share an image store.** `ImageCacheVolume` is one
  Docker graph directory under an exclusive lock, so an image one job builds or
  pulls is visible to the next job on that runner. That is what the cache is
  for. A job that must not see another job's layers needs its own runner.
- **A privileged sandbox is a privileged sandbox.** Docker-in-Docker needs
  `--privileged`; a kernel escape from the inner daemon reaches the runner host.
  Run runners on hosts you are willing to lose, and keep fork approval on
  (`CIPLATFORM_REQUIRE_FORK_APPROVAL`, default on).
