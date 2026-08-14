# Trust boundaries

Two facts shape everything here.

**A job container can reach the control plane.** It has to.
`actions/upload-artifact`, `actions/cache` and `actions/github-script`'s OIDC
helper all talk to `CIPLATFORM_PUBLIC_URL`, and that URL is in the job's
environment because we put it there. A fork PR's workflow is a stranger's code
running with that address in hand.

**The GitHub App can be published, so anybody can install it.** Installation is
the installer's decision, not the operator's. A workflow run is arbitrary code
on a privileged Docker-in-Docker runner, so "somebody installed the App" must
not be anywhere near enough to get one.

So every route on that listener is either credentialed or safe to hand a
stranger, and everything downstream of a webhook is gated on an account the
operator named. There is no "internal" surface, because there is no network
position from which the platform is internal.

## What guards each route

| Route | Credential | Held by |
|---|---|---|
| `/webhook` | HMAC-SHA256 over the body, then the account allowlist | GitHub, then `internal/installpolicy` |
| `/runner/v1/enrol`, `/runner/v1/session` | an Ed25519 signature over a timestamped, nonced message | the runner host's own key |
| `/runner/v1/*` (everything else) | a session token minted for an approved host | runner agents |
| `/twirp/*`, `/_apis/results/artifacts/upload/*` | per-job token, scoped `artifacts:*` | the running job |
| `/_apis/results/artifacts/download/*`, `/_apis/artifactcache/artifacts/*` | a signature in the URL | whoever the URL was issued to |
| `/_apis/artifactcache/*` | per-job token, scoped `cache:*` | the running job |
| `/_apis/oidc/token` | per-job token, scoped `oidc:issue` | non-fork jobs only |
| `/api/v1/*`, `/healthz` | a dashboard session, or `CIPLATFORM_OPERATOR_TOKEN` | operators |
| `/auth/{login,logout,status}`, `/auth/github/*` | none — this is where you exchange a credential | anybody |
| `/.well-known/docker-updater/health` | none — a status code and the word `ok` | orchestrators |
| `/.well-known/jwks.json`, `/.well-known/openid-configuration` | none — public by definition | anybody verifying an ID token |
| `/`, `/app.mjs`, `/app.css` | none — the shipped bundle, no data in it | anybody |

## Publishing the App: what a stranger who installs it gets

Nothing. Walked through surface by surface, because "nothing" is a claim that
has to be checked rather than asserted:

- **Their pushes are real, signed deliveries.** GitHub signs every delivery for
  this App with this App's webhook secret, so the HMAC check passes — it proves
  the delivery came from GitHub, not that it came from somebody welcome. The
  account allowlist is what refuses it, in front of every event handler, before
  any GitHub API call is made on their behalf. `test/e2e` runs a stranger's push
  against the real binary and asserts no run exists anywhere afterwards.
- **No repository row is written**, so a later delivery has nothing to match on.
- **No installation token is ever minted for their installation**, because the
  only thing that mints one is the ingest path the guard sits in front of.
- **No job, so no job token**, so nothing reaches artifacts, cache, or OIDC.
- **They can sign in with GitHub, and it refuses them by name.** Authenticating
  as themselves is expected; the admin list is a separate question, checked
  after GitHub confirms who they are.
- **They can enrol a runner key.** It lands as pending, and pending is inert:
  the session endpoint refuses it, and the fingerprint sits on the Runners page
  where it is obviously not one of yours.

The cost of a stranger's push is one HMAC check and one log line. Nothing
unbounded happens before the refusal.

What they DO get is what any App install grants: this App holds the read
permissions they agreed to on their own repositories. That is a liability worth
being aware of when publishing — the App can read repositories of accounts that
install it — and it is theirs to grant or revoke, not something this instance
can reach.

## Who may administer it

Two ways in, for two kinds of caller:

- **A person** signs in with GitHub. The App is its own OAuth provider, so this
  needs no second App: the callback exchanges the code, asks GitHub which
  account it belongs to, and mints a session only if that login is in
  `CIPLATFORM_ADMIN_LOGINS`. The session names the account, so a cancel or a
  re-run is recorded against a person instead of "operator".
- **A script** sends `CIPLATFORM_OPERATOR_TOKEN` as a bearer header. A shared
  credential names nobody, so its actions are recorded as `operator`.

The session cookie is HttpOnly, `SameSite=Lax`, and Secure on an https
deployment. Lax rather than Strict is load-bearing: the browser arrives back
from github.com on a sign-in, and Strict is not sent on a cross-site
navigation, so every sign-in would land signed-out. Lax still withholds the
cookie from every cross-site POST, which is what this API's mutations are.

Sessions are signed statements rather than rows in a table, so there is nothing
to look up per request. Removing an account from the admin list takes effect on
its next request — the list is re-read every time — but rotating
`CIPLATFORM_SESSION_SECRET` is the only way to end a specific session early, and
it ends all of them.

## Runner hosts

A machine proves which machine it is with a key it generated, and an operator
approves the fingerprint once. There is no runner token to store, transmit, or
rotate. Depth, including the replay window and how labels are bounded by the
approval, is in [runners.md](runners.md).

The properties that matter here:

- An unapproved host is inert, not trusted-by-default.
- Approval is re-read on every request and every renewal, so revoking a host
  stops it within one ten-minute token lifetime rather than whenever its token
  happened to expire.
- Approval names the operator who granted it, and is written to the event log.
- A runner cannot claim a label its host was not approved for.

## Credential separation, enforced at startup

`CIPLATFORM_OPERATOR_TOKEN` and `CIPLATFORM_JOB_TOKEN_SECRET` must differ, and
so must `CIPLATFORM_SESSION_SECRET` if it is set. Each reaches somewhere the
others must not: the signing key mints every job's credentials, the operator
token is pasted into scripts, and the session key signs cookies that open the
API. The operator token also has a minimum length, because nothing rate-limits
guesses at it.

The runner session key is generated at startup rather than configured. Runner
tokens live for minutes and are re-signed from a key the host holds, so a
restart costs every runner one silent re-authentication and saves an operator a
secret to look after.

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

- **A GitHub login is not a stable identity.** Accounts can be renamed, and a
  freed-up login can be claimed by somebody else. An admin list of logins
  inherits that: if the account named in `CIPLATFORM_ADMIN_LOGINS` is renamed
  and its old name is taken, the new holder can sign in. Watch the name, or
  keep the operator token as the real key to the door.
- **A job token stays valid after its job ends**, up to the job's timeout. The
  container holding it is gone by then, but a token captured mid-run can still
  write to that run's artifacts and cache until it expires. Rejecting tokens for
  completed jobs would need a store lookup on every chunk upload; the lifetime
  bound above is the cheaper half of the fix, and this is the half that is not
  done.
- **An approved host is trusted with every job matching its labels.** Approval
  is machine-level, not per-repository. A host that must not see one
  repository's secrets needs its own labels and a workflow that asks for them.
- **Jobs on the same runner share an image store.** The image cache is one
  Docker graph directory under an exclusive lock, so an image one job builds or
  pulls is visible to the next job on that runner. That is what the cache is
  for. A job that must not see another job's layers needs its own runner.
- **A privileged sandbox is a privileged sandbox.** Docker-in-Docker needs
  `--privileged`; a kernel escape from the inner daemon reaches the runner host.
  Run runners on hosts you are willing to lose, and keep fork approval on
  (`CIPLATFORM_REQUIRE_FORK_APPROVAL`, default on).
- **`runner-host` holds the Docker socket**, so anything that compromises the
  supervisor owns that machine's daemon. It is the same trust a CI runner
  already needs; it is not additional exposure, but it is not isolation either.
- **A runner pointed at a plain-HTTP URL sends job secrets in the clear.**
  Enrolment and renewal are signatures and give nothing away, but an assignment
  carries the job's secrets and its token. A LAN deployment straight to the
  coordinator is a legitimate setup and this does not refuse it; it logs a
  warning naming the consequence, so it is a choice rather than an oversight.
