# ci-platform

A self-hosted CI platform that runs your existing GitHub Actions workflows,
unchanged, and reports back into GitHub's checks UI so branch protection and the
PR page keep working exactly as they do today.

It exists for one reason: **it never lies about what happened, and it tells its
own failures apart from yours.** A red build means your code is broken. A
registry timeout, a lost runner, or a bad workflow file each get their own
conclusion and a sentence explaining themselves.

## What that buys you

- **Infrastructure failures are never reported as build failures.** They are
  classified, retried with backoff, and shown in their own colour.
- **Nothing is cancelled without a recorded reason** — who or what cancelled it,
  in a sentence, in the UI and in the check run.
- **A job whose runner disappears is requeued**, not lost and not failed.
- **Green means the work ran.** Skipped never counts as passed, and a job that
  did nothing cannot satisfy a required check.
- **You can see where the time went** — queued, setting up, executing — instead
  of inferring it from timestamps.

**See it**: [a demo of the web UI](https://sites.pazer.build/ci-platform/), running
against a snapshot captured from the real API — including a run that failed on
infrastructure and is not coloured like a build failure.

**Images**: `oci.pazer.build/ci-platform` (control plane),
`oci.pazer.build/ci-platform/runner` (agent), and
`oci.pazer.build/ci-platform/runner-host` (the supervisor you install on a
runner machine). Every push publishes all three, tagged with its commit and
branch; `:latest` only ever moves on the default branch.

## Run one in ten minutes

You need Docker with Compose, and a GitHub App. The App is not optional: check
runs can only be written with an App installation token.

**1. Create the GitHub App.** In your org's settings, *Developer settings → GitHub
Apps → New*. Set the webhook URL to `https://<your-host>/webhook` and the
callback URL to `https://<your-host>/auth/github/callback`, invent a webhook
secret, and grant: Checks `write`, Commit statuses `write`, Contents `read`,
Metadata `read`, Pull requests `read`, Actions `read`. Subscribe to Push, Pull
request, Check run, and Check suite events. Generate a private key, download the
`.pem`, and note the App ID and the OAuth client ID and secret.

The App can be public. An install by an account you did not list is inert: it
schedules nothing and gets nothing — [docs/security.md](docs/security.md). Say
so in the App's description, so somebody who installs it out of curiosity
learns why nothing happened from the App rather than from a support thread.

**2. Configure.** Put the key next to `compose.yaml` as `app-private-key.pem`,
then write a `.env`:

```sh
CIPLATFORM_PUBLIC_URL=https://ci.pazer.build
CIPLATFORM_WEBHOOK_SECRET=<the secret you invented>
CIPLATFORM_APP_ID=<your app id>
CIPLATFORM_OAUTH_CLIENT_ID=<from the App's settings page>
CIPLATFORM_OAUTH_CLIENT_SECRET=<generated on the App's settings page>
CIPLATFORM_ALLOWED_OWNERS=PazerOP
CIPLATFORM_ADMIN_LOGINS=PazerOP
CIPLATFORM_JOB_TOKEN_SECRET=$(openssl rand -hex 32)
CIPLATFORM_OPERATOR_TOKEN=$(openssl rand -hex 32)
CIPLATFORM_SESSION_SECRET=$(openssl rand -hex 32)
```

`CIPLATFORM_ALLOWED_OWNERS` is the list of accounts this instance runs work for,
and `CIPLATFORM_ADMIN_LOGINS` the accounts that may sign in. Both are required:
"everybody" is spelled `*`, out loud, and never by leaving a value empty.

There is no runner credential to invent. A runner host generates its own key and
you approve its fingerprint once — [docs/runners.md](docs/runners.md).

**3. Start it.**

```sh
docker compose pull && docker compose up -d
```

That takes the images CI publishes. To run the source you have checked out
instead, `docker compose up -d --build`.

**4. Approve the runner host.** Open your public URL, *Sign in with GitHub*, and
go to Runners. The host that just started is listed as pending; check its
fingerprint against the one in `docker compose logs runner-host` and approve it.

**5. Install the App** on a repository and push. The run is on the dashboard and
the check runs are on the commit.

Add capacity by raising `CI_RUNNER_HOST_RUNNERS`, or by running the runner-host
image on another machine — see [docs/runners.md](docs/runners.md) for a LAN or
internet runner.

There is no database to provision: everything lives in one SQLite file on the
`ciplatform-data` volume, created on first start. Back it up by copying it.

## Docs

- [docs/incidents.md](docs/incidents.md) — the five failures this exists to
  prevent, and the test that covers each.
- [docs/architecture.md](docs/architecture.md) — how the pieces fit together.
- [docs/compatibility.md](docs/compatibility.md) — what of GitHub Actions is
  supported, what is not, and what deviates.
- [docs/deviations.md](docs/deviations.md) — every deliberate difference, and the
  client-imposed constraints we verified rather than assumed.
- [docs/security.md](docs/security.md) — what guards each route, and what this
  deliberately does not defend against.
- [docs/demo.md](docs/demo.md) — the demo site: what it shows, and what it
  deliberately cannot do.

## Scope

GitHub stays the source of truth for code, pull requests, and branch protection.
This is not a GitHub replacement and not a multi-tenant SaaS — it is one
organisation's CI, run by that organisation.

Anything it does not support **fails the run with `unsupported: X`**. Silently
ignoring a key you wrote is worse than refusing to run.

## Licence

MIT. See [LICENSE](LICENSE).
