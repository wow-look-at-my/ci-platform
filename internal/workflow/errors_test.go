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
	return fx("errors_25.yml") + indent(body, "    ")
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
		{"no on", fx("errors_43.yml"), "declares no `on:` trigger"},
		{"no jobs", fx("errors_44.yml"), "declares no `jobs:`"},
		{"empty jobs", fx("errors_45.yml"), "`jobs:` is empty"},
		{"unknown root key", fx("errors_46.yml"), "unsupported: runners is not a known key"},
		{"duplicate root key", fx("errors_47.yml"), "on is declared twice"},
		{"empty name", fx("errors_48.yml"), "name must not be empty"},
		{"jobs not a mapping", fx("errors_49.yml"), "jobs must be a mapping, found a sequence"},
		{"non-scalar key", fx("errors_50.yml"), "where a key name was expected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestTriggerErrors(t *testing.T) {
	tail := fx("errors_58.yml")
	cases := []struct{ name, src, want string }{
		{"bare schedule", fx("errors_60.yml") + tail, "on.schedule requires at least one cron entry"},
		{"unknown bare event", fx("errors_61.yml") + tail, "unsupported: on.deploy is not a known GitHub event"},
		{"cron descriptor", fx("errors_62.yml") + tail, "uses a descriptor"},
		{"cron missing", fx("errors_63.yml") + tail, "must set `cron:`"},
		{"schedule not a sequence", fx("errors_64.yml") + tail, "on.schedule must be a sequence, found a mapping"},
		{"push types", fx("errors_65.yml") + tail, "unsupported: on.push.types is not a known key"},
		{"pr tags", fx("errors_66.yml") + tail, "unsupported: on.pull_request.tags is not a known key"},
		{"other event extra key", fx("errors_67.yml") + tail, "unsupported: on.issues.branches is not a known key"},
		{"bad input type", fx("errors_68.yml") + tail, "type must be one of"},
		{"choice without options", fx("errors_69.yml") + tail, "must list `options:`"},
		{"options without choice", fx("errors_70.yml") + tail, "but its type is"},
		{"call choice", fx("errors_71.yml") + tail, "cannot be `choice`"},
		{"call options", fx("errors_72.yml") + tail, "only valid on a workflow_dispatch input"},
		{"call output without value", fx("errors_73.yml") + tail, "must set `value:`"},
		{"branch filter not a scalar", fx("errors_74.yml") + tail, "on.push.branches[0] must be a scalar value, found a sequence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestOnKeyParsedAsBoolean(t *testing.T) {
	w, err := Parse("ci.yml", []byte(fx("errors_82.yml")))
	require.NoError(t, err)
	require.NotNil(t, w.On.Push)
}

func TestJobErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"job not a mapping", fx("errors_89.yml"), "jobs.a must be a mapping, found a scalar"},
		{"no steps or uses", job(fx("errors_90.yml")), "must declare either `steps:` or `uses:`"},
		{"uses and steps", job(fx("errors_91.yml")), "declares both `uses:` and `steps:`"},
		{"with but no uses", job(fx("errors_92.yml")), "unsupported: jobs.build.with is not a known key"},
		{"empty steps", job(fx("errors_93.yml")), "is empty; a job with no steps does no work"},
		{"steps not a sequence", job(fx("errors_94.yml")), "jobs.build.steps must be a sequence, found a mapping"},
		{"duplicate step id", job(fx("errors_95.yml")), "is already used by an earlier step"},
		{"needs itself", job(fx("errors_96.yml")), "needs contains itself"},
		{"needs twice", fx("errors_97.yml"), `needs lists "a" twice`},
		{"runs-on empty mapping", job(fx("errors_98.yml")), "must set `group:` or `labels:`"},
		{"runs-on unknown key", job(fx("errors_99.yml")), "unsupported: jobs.build.runs-on.gruop is not a known key"},
		{"permissions scalar", job(fx("errors_100.yml")), "must be `read-all`, `write-all`, or a mapping"},
		{"permissions unknown scope", job(fx("errors_101.yml")), "unsupported: jobs.build.permissions.everything is not a known key"},
		{"id-token read", job(fx("errors_102.yml")), "id-token accepts only write or none"},
		{"concurrency no group", job(fx("errors_103.yml")), "must set `group:`"},
		{"environment no name", job(fx("errors_104.yml")), "must set `name:`"},
		{"container no image", job(fx("errors_105.yml")), "must set `image:`"},
		{"service entrypoint", job(fx("errors_106.yml")), "unsupported: jobs.build.services.db.entrypoint is not implemented"},
		{"container entrypoint not allowed", job(fx("errors_107.yml")), "unsupported: jobs.build.container.entrypoint is not a known key"},
		{"secrets scalar", job(fx("errors_108.yml")), "must be `inherit` or a mapping"},
		{"defaults unknown", job(fx("errors_109.yml")), "unsupported: jobs.build.defaults.shell is not a known key"},
		{"defaults bad shell", job(fx("errors_110.yml")), `unsupported: jobs.build.defaults.run.shell "pwsh" is not implemented`},
		{"env non-scalar", job(fx("errors_111.yml")), "jobs.build.env.A must be a scalar value, found a sequence"},
		{"bad continue-on-error expression", job(fx("errors_112.yml")), "unexpected end of expression"},
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
			mustFail(t, job(fx("errors_131.yml")+c.ref+"\n"), c.want)
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
			w, err := Parse("ci.yml", []byte(job(fx("errors_146.yml")+ref+"\n")))
			require.NoError(t, err)
			require.Equal(t, ref, w.Jobs["build"].Steps[0].Uses)
		})
	}
}

func TestStepErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty run", job(fx("errors_155.yml")), "has an empty `run:`"},
		{"shell on uses", job(fx("errors_156.yml")), "sets `shell:` on a `uses:` step"},
		{"working-directory on uses", job(fx("errors_157.yml")), "sets `working-directory:` on a `uses:` step"},
		{"custom shell template", job(fx("errors_158.yml")), "is a custom shell template"},
		{"empty step id", job(fx("errors_159.yml")), "jobs.build.steps[0].id must not be empty"},
		{"step not a mapping", job(fx("errors_160.yml")), "jobs.build.steps[0] must be a mapping, found a scalar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestStrategyErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"empty strategy", job(fx("errors_169.yml")), "jobs.build.strategy is empty"},
		{"empty matrix", job(fx("errors_170.yml")), "has no dimensions and no `include:`"},
		{"literal matrix scalar", job(fx("errors_171.yml")), "must be a mapping or a ${{ }} expression"},
		{"empty dimension", job(fx("errors_172.yml")), "would expand to zero legs"},
		{"null dimension", job(fx("errors_173.yml")), "jobs.build.strategy.matrix.os has no value"},
		{"include not a sequence", job(fx("errors_174.yml")), "must be a sequence, found a mapping"},
		{"include row not a mapping", job(fx("errors_175.yml")), "must be a mapping of matrix keys to values"},
		{"include nested value", job(fx("errors_176.yml")), "must be a scalar, found a sequence"},
		{"empty include row", job(fx("errors_177.yml")), "strategy.matrix.include[0] is empty"},
		{"empty include list", job(fx("errors_178.yml")), "strategy.matrix.include is an empty list"},
		{"fail-fast not a bool", job(fx("errors_179.yml")), "must be true or false"},
		{"max-parallel zero", job(fx("errors_180.yml")), "must be at least 1"},
		{"max-parallel not a number", job(fx("errors_181.yml")), "must be a whole number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestMaxParallelExpressionIsDeferred(t *testing.T) {
	w, err := Parse("ci.yml", []byte(job(fx("errors_189.yml"))))
	require.NoError(t, err)
	require.Equal(t, "${{ vars.LIMIT }}", w.Jobs["build"].Strategy.MaxParallel.Raw)
}

func TestRetryErrors(t *testing.T) {
	body := func(retry string) string {
		return job(fx("errors_196_2.yml") + indent(retry, "  ") + fx("errors_196.yml"))
	}
	cases := []struct{ name, src, want string }{
		{"no attempts", body(fx("errors_199.yml")), "must set `attempts:`"},
		{"zero attempts", body(fx("errors_200.yml")), "must be at least 1"},
		{"attempts not a number", body(fx("errors_201.yml")), "must be a whole number"},
		{"bad backoff", body(fx("errors_202.yml")), "must be none, fixed, linear or exponential"},
		{"unitless duration", body(fx("errors_203.yml")), "must be a duration with a unit"},
		{"negative duration", body(fx("errors_204.yml")), "must not be negative"},
		{"initial beyond max", body(fx("errors_205.yml")), "is longer than"},
		{"unknown retry key", body(fx("errors_206.yml")), "unsupported: jobs.build.retry.ceiling is not a known key"},
		{"jitter not a bool", body(fx("errors_207.yml")), "must be true or false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.src, c.want) })
	}
}

func TestRetryBackoffNoneAcceptsZeroDurations(t *testing.T) {
	w, err := Parse("ci.yml", []byte(job(fx("errors_215.yml"))))
	require.NoError(t, err)
	require.Equal(t, 0, int(w.Jobs["build"].Retry.Initial))
}

func TestSelfReferentialCycleReportsItsPath(t *testing.T) {
	src := fx("errors_221.yml") +
		fx("errors_222.yml") +
		fx("errors_223.yml")
	_, err := Parse("ci.yml", []byte(src))
	require.ErrorContains(t, err, "forms a cycle: a -> b -> a")
}

func TestDeviationsAreRecorded(t *testing.T) {
	src := fx("errors_229.yml")
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
	src := fx("errors_244.yml")
	w, err := Parse("ci.yml", []byte(src))
	require.NoError(t, err)
	require.Nil(t, w.On.PullRequest, "pull_request_target must not be mistaken for pull_request")
	require.Equal(t, []string{"opened"}, w.On.Other["pull_request_target"].Types)
}
