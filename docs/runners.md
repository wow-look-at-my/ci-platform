# Runners

A runner is the thing that executes a job. A **runner host** is the machine they
run on, and `runner-host` is the supervisor you actually install there.

## Identity: a key the machine generates, a fingerprint you approve

There is no runner token, no registration secret, and nothing to invent, type,
or rotate across a fleet.

1. `runner-host` generates an Ed25519 keypair the first time it starts and keeps
   the private half in its state directory, mode 0600.
2. It sends the public half once. The control plane records it as **pending** and
   does nothing else with it. A pending host is refused everywhere.
3. You compare the fingerprint the host logged against the one on the Runners
   page and approve it. That is the moment the machine is allowed to run jobs.
4. From then on the host signs a short-lived request and gets a token good for
   ten minutes, renewed automatically.

The fingerprint is the same shape ssh prints — `SHA256:` and a base64 digest —
because the job is the same job you already do when a host key changes.

### What this buys over a shared token

- **Nothing secret crosses the wire, ever.** Not at enrolment, not at renewal.
- **Revoking one host revokes one host.** A shared token means re-keying the
  fleet; here you press Revoke and that machine stops within a token lifetime,
  because approval is re-read on every renewal rather than baked into the token.
- **Every request is attributable to one machine.**
- **A machine that has not been approved is inert**, rather than trusted because
  it knew a string.

### The replay window

A signed request carries a timestamp and a nonce. The timestamp must be within
two minutes of the control plane's clock, and a nonce is accepted once inside
that window. Both are needed: a nonce with no expiry would need an unbounded
record of every nonce ever seen, and a timestamp with no nonce would let a
captured request be replayed for the whole window.

A signature also names what it is for, so the one a host makes to enrol cannot
be replayed to get a token.

### Labels are bounded by the approval

A runner's labels decide which jobs it is offered. What a runner asks for is
intersected with what the host was approved for, and anything dropped is logged.
A host that is compromised later cannot widen its own reach by claiming
`production-secrets`. Set the allowed labels when you approve, or leave them as
enrolled to serve anything.

## runner-host

One process per machine. It owns a pool of runner containers and answers the
questions a self-hosted fleet otherwise leaves to a pile of shell scripts.

```sh
docker run -d --name runner-host \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v ci-runner-host-state:/var/lib/ci-runner-host \
  -e CI_CONTROL_PLANE_URL=http://192.168.1.84:8080 \
  -e CI_RUNNER_HOST_RUNNERS=4 \
  -e CI_RUNNER_LABELS=self-hosted,linux,x64 \
  oci.pazer.build/ci-platform/runner-host:latest
```

Then read the fingerprint out of `docker logs runner-host` and approve it.

**Keep the state volume.** It holds the identity key. Lose it and the machine
comes back as a new fingerprint that has to be approved again.

### What it guarantees

- **N runners, always.** One container per slot, named deterministically, so a
  supervisor restart adopts what is running instead of doubling the pool.
- **Fresh storage per runner.** A runner's state is its container's own writable
  layer. Replacing the runner is what cleans it up; there is no directory
  filling with the last month of workspaces.
- **Runners stay current.** It checks for a newer runner image every fifteen
  minutes and rebuilds the pool when there is one.
- **An update never kills a build.** Each runner is drained before it is
  replaced: `docker stop` with a six-hour default timeout, and the agent
  responds by taking no more work and finishing the job it holds. Slots are
  replaced one at a time, so the machine never drops to no capacity.
- **Updating the supervisor is safe too.** Stopping `runner-host` deliberately
  leaves the runners running, so an image updater — docker-updater, watchtower —
  can replace it at any moment, mid-build included.

Point your image updater at the `runner-host` image only. It keeps the runner
image current itself, so nothing else on the machine needs to know about it.

The image cache is per slot and survives a runner being replaced, because it is
a cache: throwing it away would make every job after an update re-pull its
images. It is removed when the slot itself goes.

## Reaching the control plane

The runner protocol is plain HTTP+JSON. A runner needs one URL:

- **On the LAN**, point it straight at the coordinator: `http://192.168.1.84:8080`.
  No TLS, no proxy, no public DNS.
- **Over the internet**, point it at the public URL: `https://ci.pazer.build`.

Both are the same `CI_CONTROL_PLANE_URL`, and hosts of both kinds can serve the
same instance at once. Nothing about the enrolment or the protocol differs; a
LAN host is simply one that took a shorter path.

### The one thing to watch: jobs use the public URL

A runner talks to the control plane on the URL you gave it. A **job** does not —
it gets `CIPLATFORM_PUBLIC_URL` for artifacts, cache, and OIDC, because the
control plane owns those URLs and signs them.

So `CIPLATFORM_PUBLIC_URL` has to resolve from every machine that runs jobs,
including the ones on your LAN. Most routers hairpin a public name back inside
without help. If yours does not, map the name to the LAN address in your
resolver, or in `/etc/hosts` on the runner machines. The symptom otherwise is
narrow and obvious: jobs run, and `actions/upload-artifact` cannot reach the
server.

## Running a single runner without the supervisor

`ci-runner run` works on its own, and generates and enrols its own key exactly
the same way. Mount something at `/var/lib/ci-runner` when you do, or the key is
regenerated on every restart and you approve a new fingerprint every time.

The supervisor is worth it as soon as you want more than one runner, or want
them to stay up to date without you.
