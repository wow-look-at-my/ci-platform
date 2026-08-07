package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustFail asserts that src does not parse and that the message says why.
func mustFail(t *testing.T, src, want string) {
	t.Helper()
	w, err := Parse("ci.yml", []byte(src))
	require.Error(t, err, "expected a config error, got a workflow")
	require.Nil(t, w)
	require.Contains(t, err.Error(), want)

	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "ci.yml", pe.Path)
}

// job wraps a job body in the smallest valid workflow around it.
func job(body string) string {
	return "on: push\njobs:\n  build:\n" + indent(body, "    ")
}

func indent(s, with string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = with + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestWorkflowLevelErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty file", "", "the workflow file is empty"},
		{"not a mapping", "- push\n", "a workflow file must be a mapping, found a sequence"},
		{"scalar document", "hello\n", "must be a mapping, found a scalar"},
		{"no on", "name: x\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n", "declares no `on:` trigger"},
		{"no jobs", "on: push\n", "declares no `jobs:`"},
		{"empty jobs", "on: push\njobs: {}\n", "`jobs:` is empty"},
		{"unknown root key", "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\nrunners: 3\n", "unsupported: runners is not a known key"},
		{"duplicate root key", "on: push\non: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n", "on is declared twice"},
		{"empty name", "name: \"\"\non: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n", "name must not be empty"},
		{"jobs not a mapping", "on: push\njobs: [a, b]\n", "jobs must be a mapping, found a sequence"},
		{"non-scalar key", "on: push\njobs:\n  ? [a]\n  : b\n", "where a key name was expected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestTriggerErrors(t *testing.T) {
	tail := "jobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n"
	cases := []struct{ name, src, want string }{
		{"bare schedule", "on: schedule\n" + tail, "on.schedule requires at least one cron entry"},
		{"unknown bare event", "on: [push, deploy]\n" + tail, "unsupported: on.deploy is not a known GitHub event"},
		{"cron descriptor", "on:\n  schedule:\n    - cron: \"@daily\"\n" + tail, "uses a descriptor"},
		{"cron missing", "on:\n  schedule:\n    - {}\n" + tail, "must set `cron:`"},
		{"schedule not a sequence", "on:\n  schedule:\n    cron: \"* * * * *\"\n" + tail, "on.schedule must be a sequence, found a mapping"},
		{"push types", "on:\n  push:\n    types: [created]\n" + tail, "unsupported: on.push.types is not a known key"},
		{"pr tags", "on:\n  pull_request:\n    tags: [v1]\n" + tail, "unsupported: on.pull_request.tags is not a known key"},
		{"other event extra key", "on:\n  issues:\n    branches: [main]\n" + tail, "unsupported: on.issues.branches is not a known key"},
		{"bad input type", "on:\n  workflow_dispatch:\n    inputs:\n      a:\n        type: json\n" + tail, "type must be one of"},
		{"choice without options", "on:\n  workflow_dispatch:\n    inputs:\n      a:\n        type: choice\n" + tail, "must list `options:`"},
		{"options without choice", "on:\n  workflow_dispatch:\n    inputs:\n      a:\n        type: string\n        options: [x]\n" + tail, "but its type is"},
		{"call choice", "on:\n  workflow_call:\n    inputs:\n      a:\n        type: choice\n" + tail, "cannot be `choice`"},
		{"call options", "on:\n  workflow_call:\n    inputs:\n      a:\n        options: [x]\n" + tail, "only valid on a workflow_dispatch input"},
		{"call output without value", "on:\n  workflow_call:\n    outputs:\n      a:\n        description: x\n" + tail, "must set `value:`"},
		{"branch filter not a scalar", "on:\n  push:\n    branches:\n      - [main]\n" + tail, "on.push.branches[0] must be a scalar value, found a sequence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

// TestOnKeyParsedAsBoolean covers the YAML footgun: a loader following YAML 1.1
// resolves a bare `on` key to the boolean true.
func TestOnKeyParsedAsBoolean(t *testing.T) {
	w, err := Parse("ci.yml", []byte("true: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n"))
	require.NoError(t, err)
	require.NotNil(t, w.On.Push)
}

func TestJobErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"job not a mapping", "on: push\njobs:\n  a: ubuntu\n", "jobs.a must be a mapping, found a scalar"},
		{"no steps or uses", job("runs-on: x\n"), "must declare either `steps:` or `uses:`"},
		{"uses and steps", job("uses: o/r@v1\nsteps:\n  - run: y\n"), "declares both `uses:` and `steps:`"},
		{"with but no uses", job("runs-on: x\nwith:\n  a: b\n"), "unsupported: jobs.build.with is not a known key"},
		{"empty steps", job("runs-on: x\nsteps: []\n"), "is empty; a job with no steps does no work"},
		{"steps not a sequence", job("runs-on: x\nsteps:\n  a: b\n"), "jobs.build.steps must be a sequence, found a mapping"},
		{"duplicate step id", job("runs-on: x\nsteps:\n  - id: s\n    run: a\n  - id: s\n    run: b\n"), "is already used by an earlier step"},
		{"needs itself", job("runs-on: x\nneeds: build\nsteps:\n  - run: y\n"), "needs contains itself"},
		{"needs twice", "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n  b:\n    runs-on: x\n    needs: [a, a]\n    steps:\n      - run: y\n", `needs lists "a" twice`},
		{"runs-on empty mapping", job("runs-on: {}\nsteps:\n  - run: y\n"), "must set `group:` or `labels:`"},
		{"runs-on unknown key", job("runs-on:\n  gruop: x\nsteps:\n  - run: y\n"), "unsupported: jobs.build.runs-on.gruop is not a known key"},
		{"permissions scalar", job("runs-on: x\npermissions: everything\nsteps:\n  - run: y\n"), "must be `read-all`, `write-all`, or a mapping"},
		{"permissions unknown scope", job("runs-on: x\npermissions:\n  everything: read\nsteps:\n  - run: y\n"), "unsupported: jobs.build.permissions.everything is not a known key"},
		{"id-token read", job("runs-on: x\npermissions:\n  id-token: read\nsteps:\n  - run: y\n"), "id-token accepts only write or none"},
		{"concurrency no group", job("runs-on: x\nconcurrency:\n  cancel-in-progress: true\nsteps:\n  - run: y\n"), "must set `group:`"},
		{"environment no name", job("runs-on: x\nenvironment:\n  url: http://x\nsteps:\n  - run: y\n"), "must set `name:`"},
		{"container no image", job("runs-on: x\ncontainer:\n  options: --cpus 1\nsteps:\n  - run: y\n"), "must set `image:`"},
		{"service entrypoint", job("runs-on: x\nservices:\n  db:\n    image: postgres\n    entrypoint: /bin/sh\nsteps:\n  - run: y\n"), "unsupported: jobs.build.services.db.entrypoint is not implemented"},
		{"container entrypoint not allowed", job("runs-on: x\ncontainer:\n  image: golang\n  entrypoint: /bin/sh\nsteps:\n  - run: y\n"), "unsupported: jobs.build.container.entrypoint is not a known key"},
		{"secrets scalar", job("uses: o/r/.github/workflows/w.yml@v1\nsecrets: everything\n"), "must be `inherit` or a mapping"},
		{"defaults unknown", job("runs-on: x\ndefaults:\n  shell: bash\nsteps:\n  - run: y\n"), "unsupported: jobs.build.defaults.shell is not a known key"},
		{"defaults bad shell", job("runs-on: x\ndefaults:\n  run:\n    shell: pwsh\nsteps:\n  - run: y\n"), `unsupported: jobs.build.defaults.run.shell "pwsh" is not implemented`},
		{"env non-scalar", job("runs-on: x\nenv:\n  A: [1]\nsteps:\n  - run: y\n"), "jobs.build.env.A must be a scalar value, found a sequence"},
		{"bad continue-on-error expression", job("runs-on: x\ncontinue-on-error: ${{ 1 == }}\nsteps:\n  - run: y\n"), "unexpected end of expression"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestUsesRefErrors(t *testing.T) {
	cases := []struct{ name, ref, want string }{
		{"no ref", "actions/checkout", "must be pinned to a ref"},
		{"empty ref", "actions/checkout@", "has an empty ref after '@'"},
		{"no owner", "checkout@v4", "must be owner/repo@ref"},
		{"empty owner", "/checkout@v4", "must be owner/repo@ref"},
		{"empty path segment", "o/r//p@v4", "has an empty path segment"},
		{"local with ref", "./local/action@v1", "must not carry an @ref"},
		{"docker with no image", "docker://", "names no image"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustFail(t, job("runs-on: x\nsteps:\n  - uses: "+c.ref+"\n"), c.want)
		})
	}
}

func TestUsesRefAccepted(t *testing.T) {
	for _, ref := range []string{
		"actions/checkout@v4",
		"actions/checkout@8f4b7f8",
		"owner/repo/path/to/action@refs/heads/main",
		"./.github/actions/local",
		"../sibling/action",
		"docker://alpine:3.20",
	} {
		t.Run(ref, func(t *testing.T) {
			w, err := Parse("ci.yml", []byte(job("runs-on: x\nsteps:\n  - uses: "+ref+"\n")))
			require.NoError(t, err)
			require.Equal(t, ref, w.Jobs["build"].Steps[0].Uses)
		})
	}
}

func TestStepErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty run", job("runs-on: x\nsteps:\n  - run: \"   \"\n"), "has an empty `run:`"},
		{"shell on uses", job("runs-on: x\nsteps:\n  - uses: o/r@v1\n    shell: bash\n"), "sets `shell:` on a `uses:` step"},
		{"working-directory on uses", job("runs-on: x\nsteps:\n  - uses: o/r@v1\n    working-directory: /tmp\n"), "sets `working-directory:` on a `uses:` step"},
		{"custom shell template", job("runs-on: x\nsteps:\n  - run: y\n    shell: /bin/bash -e {0}\n"), "is a custom shell template"},
		{"empty step id", job("runs-on: x\nsteps:\n  - id: \"\"\n    run: y\n"), "jobs.build.steps[0].id must not be empty"},
		{"step not a mapping", job("runs-on: x\nsteps:\n  - echo hi\n"), "jobs.build.steps[0] must be a mapping, found a scalar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestStrategyErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty strategy", job("runs-on: x\nstrategy: {}\nsteps:\n  - run: y\n"), "jobs.build.strategy is empty"},
		{"empty matrix", job("runs-on: x\nstrategy:\n  matrix: {}\nsteps:\n  - run: y\n"), "has no dimensions and no `include:`"},
		{"literal matrix scalar", job("runs-on: x\nstrategy:\n  matrix: everything\nsteps:\n  - run: y\n"), "must be a mapping or a ${{ }} expression"},
		{"empty dimension", job("runs-on: x\nstrategy:\n  matrix:\n    os: []\nsteps:\n  - run: y\n"), "would expand to zero legs"},
		{"null dimension", job("runs-on: x\nstrategy:\n  matrix:\n    os:\nsteps:\n  - run: y\n"), "jobs.build.strategy.matrix.os has no value"},
		{"include not a sequence", job("runs-on: x\nstrategy:\n  matrix:\n    os: [a]\n    include:\n      x: y\nsteps:\n  - run: y\n"), "must be a sequence, found a mapping"},
		{"include row not a mapping", job("runs-on: x\nstrategy:\n  matrix:\n    os: [a]\n    include:\n      - a\nsteps:\n  - run: y\n"), "must be a mapping of matrix keys to values"},
		{"include nested value", job("runs-on: x\nstrategy:\n  matrix:\n    os: [a]\n    include:\n      - os: a\n        extra: [1, 2]\nsteps:\n  - run: y\n"), "must be a scalar, found a sequence"},
		{"empty include row", job("runs-on: x\nstrategy:\n  matrix:\n    os: [a]\n    include:\n      - {}\nsteps:\n  - run: y\n"), "strategy.matrix.include[0] is empty"},
		{"empty include list", job("runs-on: x\nstrategy:\n  matrix:\n    os: [a]\n    include: []\nsteps:\n  - run: y\n"), "strategy.matrix.include is an empty list"},
		{"fail-fast not a bool", job("runs-on: x\nstrategy:\n  fail-fast: sometimes\n  matrix:\n    os: [a]\nsteps:\n  - run: y\n"), "must be true or false"},
		{"max-parallel zero", job("runs-on: x\nstrategy:\n  max-parallel: 0\n  matrix:\n    os: [a]\nsteps:\n  - run: y\n"), "must be at least 1"},
		{"max-parallel not a number", job("runs-on: x\nstrategy:\n  max-parallel: many\n  matrix:\n    os: [a]\nsteps:\n  - run: y\n"), "must be a whole number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestMaxParallelExpressionIsDeferred(t *testing.T) {
	w, err := Parse("ci.yml", []byte(job("runs-on: x\nstrategy:\n  max-parallel: ${{ vars.LIMIT }}\n  matrix:\n    os: [a]\nsteps:\n  - run: y\n")))
	require.NoError(t, err)
	require.Equal(t, "${{ vars.LIMIT }}", w.Jobs["build"].Strategy.MaxParallel.Raw)
}

func TestRetryErrors(t *testing.T) {
	body := func(retry string) string {
		return job("runs-on: x\nretry:\n" + indent(retry, "  ") + "steps:\n  - run: y\n")
	}
	cases := []struct{ name, src, want string }{
		{"no attempts", body("backoff: fixed\n"), "must set `attempts:`"},
		{"zero attempts", body("attempts: 0\n"), "must be at least 1"},
		{"attempts not a number", body("attempts: many\n"), "must be a whole number"},
		{"bad backoff", body("attempts: 2\nbackoff: quadratic\n"), "must be none, fixed, linear or exponential"},
		{"unitless duration", body("attempts: 2\ninitial: 5\n"), "must be a duration with a unit"},
		{"negative duration", body("attempts: 2\ninitial: -5s\n"), "must not be negative"},
		{"initial beyond max", body("attempts: 2\ninitial: 5m\nmax: 10s\n"), "is longer than"},
		{"unknown retry key", body("attempts: 2\nceiling: 10s\n"), "unsupported: jobs.build.retry.ceiling is not a known key"},
		{"jitter not a bool", body("attempts: 2\njitter: maybe\n"), "must be true or false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestRetryBackoffNoneAcceptsZeroDurations(t *testing.T) {
	w, err := Parse("ci.yml", []byte(job("runs-on: x\nretry:\n  attempts: 2\n  backoff: none\n  initial: 0s\n  max: 0s\nsteps:\n  - run: y\n")))
	require.NoError(t, err)
	require.Equal(t, 0, int(w.Jobs["build"].Retry.Initial))
}

func TestSelfReferentialCycleReportsItsPath(t *testing.T) {
	src := "on: push\njobs:\n" +
		"  a:\n    runs-on: x\n    needs: [b]\n    steps:\n      - run: y\n" +
		"  b:\n    runs-on: x\n    needs: [a]\n    steps:\n      - run: y\n"
	_, err := Parse("ci.yml", []byte(src))
	require.ErrorContains(t, err, "forms a cycle: a -> b -> a")
}

func TestDeviationsAreRecorded(t *testing.T) {
	src := "name: x\ndescription: what this does\non:\n  release:\n    types: [published]\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: echo ${{ github.sha }}\n"
	w, err := Parse("ci.yml", []byte(src))
	require.NoError(t, err)
	require.Equal(t, "what this does", w.Description, "description is carried, not dropped")
	require.False(t, hasDeviation(w, "description"))
	require.True(t, hasDeviation(w, "on.release"))
	require.True(t, hasDeviation(w, "${{ }}"))
	for _, d := range w.Deviations {
		require.NotEmpty(t, d.GHABehavior, d.Path)
		require.NotEmpty(t, d.OurBehavior, d.Path)
		require.NotEmpty(t, d.Rationale, d.Path)
	}
}

func TestPullRequestTargetIsNameOnly(t *testing.T) {
	src := "on:\n  pull_request_target:\n    types: [opened]\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: y\n"
	w, err := Parse("ci.yml", []byte(src))
	require.NoError(t, err)
	require.Nil(t, w.On.PullRequest, "pull_request_target must not be mistaken for pull_request")
	require.Equal(t, []string{"opened"}, w.On.Other["pull_request_target"].Types)
}
