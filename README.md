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

## Run one in ten minutes

You need Docker with Compose, and a GitHub App. The App is not optional: check
runs can only be written with an App installation token.

**1. Create the GitHub App.** In your org's settings, *Developer settings → GitHub
Apps → New*. Set the webhook URL to `https://<your-host>/webhook`, invent a
webhook secret, and grant: Checks `write`, Commit statuses `write`, Contents
`read`, Metadata `read`, Pull requests `read`, Actions `read`. Subscribe to
Push, Pull request, Check run, and Check suite events. Generate a private key,
download the `.pem`, and note the App ID.

**2. Configure.** Put the key next to `compose.yaml` as `app-private-key.pem`,
then write a `.env`:

```sh
POSTGRES_PASSWORD=$(openssl rand -hex 16)
CIPLATFORM_WEBHOOK_SECRET=<the secret you invented>
CIPLATFORM_APP_ID=<your app id>
CIPLATFORM_JOB_TOKEN_SECRET=$(openssl rand -hex 32)
CI_RUNNER_TOKEN=$(openssl rand -hex 32)   # must differ from the job token secret
CIPLATFORM_PUBLIC_URL=http://ci.localhost:8080
```

The public URL's hostname must end in `.localhost` or `.ghe.com`. That is a
constraint the official `actions/upload-artifact` imposes on any non-github.com
server, not a preference of ours — see [docs/deviations.md](docs/deviations.md).

**3. Start it.**

```sh
docker compose up -d --build
```

**4. Install the App** on a repository and push. The run appears at
`http://ci.localhost:8080`, and the check runs appear on the commit.

Add capacity with `docker compose up -d --scale runner=4`.

## Docs

- [docs/incidents.md](docs/incidents.md) — the five failures this exists to
  prevent, and the test that covers each.
- [docs/architecture.md](docs/architecture.md) — how the pieces fit together.
- [docs/compatibility.md](docs/compatibility.md) — what of GitHub Actions is
  supported, what is not, and what deviates.
- [docs/deviations.md](docs/deviations.md) — every deliberate difference, and the
  client-imposed constraints we verified rather than assumed.

## Scope

GitHub stays the source of truth for code, pull requests, and branch protection.
This is not a GitHub replacement and not a multi-tenant SaaS — it is one
organisation's CI, run by that organisation.

Anything it does not support **fails the run with `unsupported: X`**. Silently
ignoring a key you wrote is worse than refusing to run.

## Licence

MIT. See [LICENSE](LICENSE).
