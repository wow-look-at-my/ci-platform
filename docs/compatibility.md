# GitHub Actions compatibility matrix

Three states, and only three. **Supported** means implemented and tested.
**Unsupported** means the run *fails* with `unsupported: <path> <feature>`, at
parse time, naming the YAML path and line. There is no fourth state where a key
you wrote is quietly ignored.

The key allow-lists are derived from GitHub's own workflow schema
(`workflow-v1.0.json` in `actions/runner`) rather than from memory, so a key
that exists in Actions and not in this table is a bug in this table.

## Workflow level

| Key | State |
|---|---|
| `name`, `description`, `run-name` | Supported |
| `on` | Supported for the events below |
| `env`, `defaults`, `concurrency`, `permissions`, `jobs` | Supported |
| anything else | **Config error** |

### Events

| Event | State |
|---|---|
| `push` (with `branches`/`tags`/`paths` and their `-ignore` forms) | Supported |
| `pull_request` (with `types` and the same filters) | Supported |
| `workflow_dispatch` (with typed `inputs`) | Supported |
| `schedule` (cron) | Supported |
| `workflow_call` | **Unsupported** — parsed, then fails the run |
| other webhook events | Accepted, filtered by name only |

## Job level

| Key | State |
|---|---|
| `name`, `needs`, `if`, `runs-on`, `env`, `defaults`, `outputs` | Supported |
| `steps`, `timeout-minutes`, `continue-on-error`, `permissions` | Supported |
| `concurrency` (including `cancel-in-progress`) | Supported |
| `strategy.matrix` with `include`/`exclude`, `fail-fast`, `max-parallel` | Supported |
| `retry` | Supported — this platform's own, see below |
| `container` | **Unsupported** |
| `services` | **Unsupported** |
| `environment` | **Unsupported** |
| `uses` (reusable workflow call), `with`, `secrets` | **Unsupported** |

`permissions` accepts the full scope set from the schema — `actions`,
`artifact-metadata`, `attestations`, `checks`, `contents`, `deployments`,
`discussions`, `id-token`, `issues`, `models`, `packages`, `pages`,
`pull-requests`, `repository-projects`, `security-events`, `statuses`,
`vulnerability-alerts` — at `read`, `write`, or `none`.

## Step level

| Key | State |
|---|---|
| `name`, `id`, `if`, `uses`, `run`, `with`, `env` | Supported |
| `working-directory`, `continue-on-error`, `timeout-minutes` | Supported |
| `shell`: `bash`, `sh`, `python`, `node` | Supported |
| `shell`: `pwsh`, `powershell`, `cmd` | **Unsupported** — named as not implemented, not as unknown |
| `retry` | Supported |

## Actions (`uses:`)

| Kind | State |
|---|---|
| `owner/repo@ref`, `owner/repo/path@ref` | Supported |
| `./local/path` | Supported |
| JavaScript actions, `using: node20` and `node24`, with `pre`/`post` | Supported |
| Composite actions, `using: composite`, nested `uses:` | Supported |
| `docker://image` and `using: docker` | **Unsupported** |

An action ref that cannot be resolved is a **config** failure naming the ref —
never a retry, because retrying cannot fix a ref that does not exist.

## Expressions

Supported: all contexts (`github`, `env`, `vars`, `secrets`, `job`, `jobs`,
`steps`, `runner`, `needs`, `strategy`, `matrix`, `inputs`); the full operator
set with GitHub's own loose-equality and truthiness rules, including
case-insensitive string comparison and `&&`/`||` returning operands rather than
booleans; object filters (`a.*.b`); and the functions `contains`, `startsWith`,
`endsWith`, `format`, `join`, `toJSON`, `fromJSON`, `hashFiles`, `success`,
`failure`, `always`, `cancelled`.

An unknown named-value or function is a **config error**, never a silent null.

The evaluator is validated against 244 cases ported from two independent
implementations (`nektos/act` and `rhysd/actionlint`). Where those disagree,
GitHub's own runner settles it.

## Workflow commands and files

Supported: `::error::`, `::warning::`, `::notice::` (with `file`/`line`/`col`/
`title`, mapped to check-run annotations on the PR diff), `::group::`/
`::endgroup::`, `::add-mask::`, the `::stop-commands::<token>` pause protocol,
and `$GITHUB_OUTPUT`, `$GITHUB_ENV`, `$GITHUB_PATH`, `$GITHUB_STEP_SUMMARY`,
`$GITHUB_STATE` including the heredoc form.

`::set-output::` and `::save-state::` work and emit a deprecation warning.

## First-party actions

| Action | State |
|---|---|
| `actions/checkout` | Supported |
| `actions/setup-*` | Supported |
| `actions/cache` | Supported — v1 cache REST API |
| `actions/upload-artifact@v4`, `actions/download-artifact@v4` | Supported — requires the `.localhost`/`.ghe.com` hostname constraint, see `docs/deviations.md` |

## What this platform adds

`retry` on jobs and steps:

```yaml
retry:
  attempts: 3
  on: [infra]
  backoff: exponential
  initial: 5s
  max: 2m
  jitter: true
```

Absent, the default applies: infra failures retry three times with exponential
backoff, user failures never retry, and config errors never retry because
retrying cannot help.

## This platform can run its own CI

Our own `.github/workflows/ci.yml` uses nothing this platform does not
implement. It stopped needing service containers when the store moved to SQLite:
the tests bring their own database, so no job declares a `services:` block.

`test/conformance` keeps it that way — it parses our workflow and fails if any
job uses an unimplemented key, so the day someone adds one is the day CI says
so, rather than the day somebody notices we do not eat our own cooking.

Service containers themselves are still unsupported, and still Phase 2; a
workflow that declares one is refused by name.

## Known behavioural differences

Beyond the unsupported list, the deliberate differences — the `infra_failure`
conclusion, the markdown step table, coalesced check-run updates, one fresh
Docker-in-Docker sandbox per job, and the constraints the official artifact
client imposes on any non-github.com server — are each written up in
[docs/deviations.md](deviations.md).

One worth stating here because it looks like a contradiction: on the **legacy
commit-status API** a skipped job's own context reports `success`, because that
API has only `error`/`failure`/`pending`/`success` and GitHub itself treats a
skipped required check as satisfied. The **aggregate** never does this: a run
whose jobs were all skipped concludes `skipped`, and a mix of success and
skipped concludes `neutral`. Zero units of work never conclude success.
