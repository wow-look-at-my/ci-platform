# The demo site

Every push publishes the web UI, running against a captured snapshot, to a
buildhost site:

- `https://sites.pazer.build/ci-platform/` — the default branch
- `https://sites.pazer.build/ci-platform/@<branch>/` — any other branch

It is the real UI. The bundle is built from the same `web-src/` the server
embeds; only two modules are swapped at build time, and nothing about the demo
reaches the shipped bundle.

## What it shows, and why those runs

The snapshot is not a screenshot of a happy path. It is chosen so every
incident in [incidents.md](incidents.md) is visible:

| In the demo | Incident |
|---|---|
| `Release #88` concluded **infra_failure**, amber, with the matched rule and the retry in its log | 1 — a 524 is not a build failure |
| `Nightly #31` succeeded with a requeue, and a `runner_lost` event naming the runner that vanished | 2 — a lost runner is requeued, not failed |
| `CI #410` cancelled, with the actor (`concurrency_group`) and a sentence | 3 — nothing is cancelled without a reason |
| Every job's queued / setup / execute bar, and the `gpu` label starving on the queue page | 4 — where the time went, and why nothing picked the work up |
| `CI #412` green, with four steps that ran and an artifact that exists | 5 — green means the work ran |
| `CI #411` failed and classified **user**, with the failing assertion annotated | the contrast that makes the amber one mean something |

## How it is built

`cmd/demofixtures` seeds a real SQLite store through `internal/demoseed`, serves
it with the real `internal/api` handlers, and records the JSON those handlers
return into `web-src/demo/fixtures.json`. The demo's client answers from that
file.

That indirection is the point. A hand-written fixture file would be a second,
unchecked description of the API, and the first shape to drift would be one no
test covers — a demo showing something the product does not produce is exactly
the quiet lie this platform exists to refuse. `go run ./cmd/demofixtures -check`
fails CI when the committed snapshot no longer matches a fresh capture.

`cmd/buildweb -demo` bundles `web-src/` with `web-src/demo/api.ts` and
`web-src/demo/auth.ts` substituted for their live counterparts, by an esbuild
plugin that rewrites those two imports. The alternative — a runtime flag
threaded through the shipped modules — would put demo branches in production
code, and `internal/webui/webbuild` has tests asserting the shipped bundle
contains no fixture data and the demo bundle contains no live client.

## What it deliberately cannot do

- **Cancel and re-run refuse**, naming the reason: there is no control plane
  behind the page.
- **There is no sign-in**, because there is no session. The real UI is gated
  (see [security.md](security.md)); a demo prompting for a credential nothing
  would accept is worse than no prompt.
- **Nothing streams.** Every job in the snapshot is finished, so the log view
  never opens an `EventSource` it cannot service. A page that says
  "reconnecting…" forever would be a demo of a broken instance.
- **Relative times are relative to the capture**, which the banner states along
  with the instant it was taken. As the snapshot ages the page says "3 days
  ago" rather than pretending to be live.

## Changing it

Edit `internal/demoseed`, then:

```sh
go run ./cmd/demofixtures        # recapture the snapshot
go run ./cmd/buildweb -demo -out /tmp/demo-site
```

Serve `/tmp/demo-site` under a path prefix to check it the way buildhost does —
an absolute asset URL would work at the root and 404 on the real site, which is
why `TestDemoAssetsAreRelative` exists.
