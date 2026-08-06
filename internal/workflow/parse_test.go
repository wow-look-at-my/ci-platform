package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func load(t *testing.T, name string) *model.Workflow {
	t.Helper()
	path := filepath.Join("testdata", name)
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	w, err := Parse(path, src)
	require.NoError(t, err)
	return w
}

// TestValidCorpusParses is the floor: every file under testdata/valid parses,
// names its jobs in declaration order, and gives every step a number.
func TestValidCorpusParses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "valid", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			require.NoError(t, err)
			w, err := Parse(f, src)
			require.NoError(t, err)

			require.Equal(t, f, w.Path)
			require.NotEmpty(t, w.Name)
			require.Len(t, w.JobOrder, len(w.Jobs), "JobOrder must name every job exactly once")
			seen := map[string]bool{}
			for _, key := range w.JobOrder {
				require.NotNil(t, w.Jobs[key], "JobOrder names %q but Jobs does not have it", key)
				require.False(t, seen[key], "JobOrder repeats %q", key)
				seen[key] = true
				require.Equal(t, key, w.Jobs[key].Key)
				for i, s := range w.Jobs[key].Steps {
					require.Equal(t, i+1, s.Number)
					require.True(t, s.Run.Empty() != (s.Uses == ""),
						"step %d of %s must be exactly one of run/uses", i+1, key)
				}
			}
		})
	}
}

func TestGoToolchainCI(t *testing.T) {
	w := load(t, "valid/go-toolchain-ci.yml")

	require.Equal(t, "CI", w.Name)
	require.NotNil(t, w.On.Push)
	require.Empty(t, w.On.Push.Branches, "a bare `push:` filters nothing")
	require.Nil(t, w.On.PullRequest)
	require.Equal(t, []string{"test"}, w.JobOrder)

	require.NotNil(t, w.Permissions)
	require.Equal(t, map[string]string{
		"id-token": "write", "contents": "write", "actions": "read", "checks": "read",
	}, w.Permissions.Scopes)

	j := w.Jobs["test"]
	require.Equal(t, []model.Expr{model.NewExpr("ubuntu-latest")}, j.RunsOn.Labels)
	require.Len(t, j.Steps, 2)
	require.Equal(t, "actions/checkout@v4", j.Steps[0].Uses)
	require.Equal(t, "wow-look-at-my/go-toolchain@v1", j.Steps[1].Uses)
	require.Empty(t, Unsupported(w))
}

func TestMatrix(t *testing.T) {
	w := load(t, "valid/matrix.yml")
	s := w.Jobs["build"].Strategy
	require.NotNil(t, s)

	require.NotNil(t, s.FailFast)
	require.False(t, *s.FailFast)
	require.Equal(t, "4", s.MaxParallel.Raw)

	m := s.Matrix
	require.Equal(t, []string{"os", "go", "arch"}, m.Order, "dimension order must follow the file")
	require.Equal(t, []any{"ubuntu-latest", "macos-latest", "windows-latest"}, m.Dimensions["os"])
	require.Equal(t, []any{"1.23", "1.24"}, m.Dimensions["go"])

	require.Len(t, m.Include, 2)
	require.Equal(t, map[string]any{
		"os": "ubuntu-latest", "go": "1.24", "arch": "amd64", "coverage": true,
	}, m.Include[0])
	require.Equal(t, map[string]any{"os": "ubuntu-latest", "experimental": true}, m.Include[1])

	require.Len(t, m.Exclude, 2)
	require.Equal(t, map[string]any{"os": "windows-latest", "arch": "arm64"}, m.Exclude[0])

	// A matrix reference survives into the IR unevaluated.
	require.Equal(t, "${{ matrix.os }}", w.Jobs["build"].RunsOn.Labels[0].Raw)
	require.Equal(t, "matrix.coverage", w.Jobs["build"].Steps[2].If.Raw)
	require.Equal(t, "30", w.Jobs["build"].TimeoutMinutes.Raw)
}

func TestNeedsDAG(t *testing.T) {
	w := load(t, "valid/dag.yml")
	require.Equal(t, []string{"setup", "lint", "test", "report"}, w.JobOrder)

	require.Equal(t, []string{"setup"}, w.Jobs["lint"].Needs, "a scalar `needs:` becomes a one-element list")
	require.Equal(t, []string{"setup"}, w.Jobs["test"].Needs)
	require.Equal(t, []string{"lint", "test", "setup"}, w.Jobs["report"].Needs)
	require.Equal(t, "always()", w.Jobs["report"].If.Raw)

	require.Equal(t, "${{ steps.meta.outputs.tag }}", w.Jobs["setup"].Outputs["tag"].Raw)

	// A whole-matrix expression is deferred, not expanded.
	m := w.Jobs["test"].Strategy.Matrix
	require.NotNil(t, m)
	require.Equal(t, "${{ fromJSON(needs.setup.outputs.legs) }}", m.Dimensions["shard"][0])
}

func TestTriggers(t *testing.T) {
	w := load(t, "valid/triggers.yml")
	on := w.On

	require.NotNil(t, on.Push)
	require.Equal(t, []string{"main"}, on.Push.Branches)
	require.Equal(t, []string{"v*"}, on.Push.TagsIgnore)
	require.Equal(t, []string{"docs/**"}, on.Push.PathsIgnore)

	require.NotNil(t, on.PullRequest)
	require.Equal(t, []string{"opened", "synchronize", "reopened"}, on.PullRequest.Types)
	require.Equal(t, []string{"wip"}, on.PullRequest.BranchesIgnore)

	require.Equal(t, []model.ScheduleTrigger{{Cron: "0 3 * * *"}, {Cron: "*/15 * * * 1-5"}}, on.Schedule)

	d := on.WorkflowDispatch
	require.NotNil(t, d)
	require.Equal(t, []string{"environment", "verbose", "count"}, d.Order)
	require.Equal(t, "choice", d.Inputs["environment"].Type)
	require.True(t, d.Inputs["environment"].Required)
	require.Equal(t, []string{"staging", "production"}, d.Inputs["environment"].Options)
	require.Equal(t, "1", d.Inputs["count"].Default)

	require.Equal(t, model.RawEvents{Types: []string{"opened"}}, on.Other["issues"])
	// An event we accept by name only is surfaced as a deviation, not dropped.
	require.True(t, hasDeviation(w, "on.issues"), "deviations: %+v", w.Deviations)
}

func TestShorthandForms(t *testing.T) {
	w := load(t, "valid/shorthands.yml")

	require.NotNil(t, w.On.Push)
	require.NotNil(t, w.On.PullRequest)
	require.NotNil(t, w.On.WorkflowDispatch)

	require.Equal(t, "1", w.Env["GLOBAL"].Raw)
	require.Equal(t, "3", w.Env["NUMERIC"].Raw, "a numeric env value keeps its literal text")
	require.Equal(t, model.Defaults{Shell: "bash", WorkingDirectory: "./src"}, w.Defaults)
	require.Equal(t, "ci-${{ github.ref }}", w.Concurrency.Group.Raw)
	require.Equal(t, "read-all", w.Permissions.All)
	require.Empty(t, w.Permissions.Scopes)

	scalar := w.Jobs["scalar-runner"]
	require.Equal(t, "true", scalar.ContinueOnError.Raw)
	require.Equal(t, "scalar-${{ github.ref }}", scalar.Concurrency.Group.Raw)
	require.Equal(t, "true", scalar.Concurrency.CancelInProgress.Raw)

	list := w.Jobs["list-runner"]
	require.Equal(t, []model.Expr{
		model.NewExpr("self-hosted"), model.NewExpr("linux"), model.NewExpr("x64"),
	}, list.RunsOn.Labels)
	require.Equal(t, map[string]string{"contents": "read", "id-token": "write"}, list.Permissions.Scopes)
	require.Equal(t, "/tmp", list.Steps[0].WorkingDirectory.Raw)
	require.Equal(t, "5", list.Steps[0].TimeoutMinutes.Raw)

	group := w.Jobs["group-runner"]
	require.Equal(t, "beefy", group.RunsOn.Group.Raw)
	require.Equal(t, []model.Expr{model.NewExpr("gpu")}, group.RunsOn.Labels)
	require.Equal(t, "python", group.Defaults.Shell)
	require.Equal(t, "s1", group.Steps[0].ID)
	require.Equal(t, "node", group.Steps[1].Shell)
}

func TestRetryPolicy(t *testing.T) {
	w := load(t, "valid/retry.yml")
	j := w.Jobs["flaky"]

	require.NotNil(t, j.Retry)
	require.Equal(t, model.DefaultRetryPolicy(), *j.Retry, "the file spells out the default policy")

	s := j.Steps[0].Retry
	require.NotNil(t, s)
	require.Equal(t, 5, s.Attempts)
	require.Equal(t, []model.FailureClass{model.ClassInfra, model.ClassConfig}, s.On)
	require.Equal(t, model.BackoffLinear, s.Backoff)
	require.Equal(t, "1s", s.Initial.String())
	require.Equal(t, "30s", s.Max.String())
	require.False(t, s.Jitter)

	// Everything unset inherits the default policy.
	partial := j.Steps[1].Retry
	require.NotNil(t, partial)
	want := model.DefaultRetryPolicy()
	want.Attempts = 2
	require.Equal(t, want, *partial)

	// A step with no `retry:` at all leaves it nil, so the caller applies
	// model.DefaultRetryPolicy() rather than the parser inventing one.
	require.Nil(t, load(t, "valid/matrix.yml").Jobs["build"].Steps[0].Retry)
}

func TestPhase2ParsesButIsUnsupported(t *testing.T) {
	w := load(t, "valid/phase2.yml")

	// Everything is in the IR...
	require.NotNil(t, w.On.WorkflowCall)
	require.True(t, w.On.WorkflowCall.Inputs["target"].Required)
	require.True(t, w.On.WorkflowCall.Secrets["token"].Required)
	require.Equal(t, "${{ jobs.build.outputs.digest }}", w.On.WorkflowCall.Outputs["digest"].Value.Raw)

	b := w.Jobs["build"]
	require.Equal(t, "golang:1.24", b.Container.Image.Raw)
	require.Equal(t, "0", b.Container.Env["CGO_ENABLED"].Raw)
	require.Equal(t, "postgres:16", b.Services["postgres"].Image.Raw)
	require.Equal(t, "redis:7", b.Services["redis"].Image.Raw, "a scalar service is its image")
	require.Equal(t, "production", b.Environment.Name.Raw)

	c := w.Jobs["call"]
	require.Equal(t, "./.github/workflows/reusable.yml", c.Uses)
	require.True(t, c.Secrets.Inherit)
	require.Empty(t, c.Steps)

	// ...and every one of those is reported as unrunnable.
	got := map[string]string{}
	for _, u := range Unsupported(w) {
		got[u.Path] = u.Feature
	}
	require.Equal(t, map[string]string{
		"on.workflow_call":             "reusable workflow definitions",
		"jobs.build.container":         "job containers",
		"jobs.build.environment":       "deployment environments",
		"jobs.build.services.postgres": "service containers",
		"jobs.build.services.redis":    "service containers",
		"jobs.call.uses":               "reusable workflow calls",
	}, got)
	require.Contains(t, Unsupported(w)[0].String(), "unsupported: ")
}

func TestUnsupportedIsEmptyForPhase1(t *testing.T) {
	for _, name := range []string{"valid/matrix.yml", "valid/dag.yml", "valid/shorthands.yml", "valid/retry.yml"} {
		require.Empty(t, Unsupported(load(t, name)), name)
	}
	require.Nil(t, Unsupported(nil))
}

// TestInvalidCorpus pins both the message and the line, because a config error
// that cannot point at the mistake is barely better than no error.
func TestInvalidCorpus(t *testing.T) {
	cases := []struct {
		file string
		line int
		msg  string
	}{
		{"needs-cycle.yml", 7, "forms a cycle: a -> c -> b -> a"},
		{"unknown-key.yml", 8, `unsupported: jobs.build.contineu-on-error is not a known key`},
		{"unknown-step-key.yml", 10, "unsupported: jobs.build.steps[0].allow-failure is not a known key"},
		{"step-run-and-uses.yml", 10, "sets both `run:` and `uses:`"},
		{"step-neither.yml", 9, "must set either `run:` or `uses:`"},
		{"bad-expression.yml", 10, "unexpected end of expression"},
		{"bad-expression-template.yml", 10, "expected a property name after '.'"},
		{"unsupported-shell.yml", 10, `unsupported: jobs.build.steps[0].shell "pwsh" is not implemented`},
		{"unknown-shell.yml", 10, `"fish" is not a known shell`},
		{"unknown-needs.yml", 8, `refers to "setup", which is not a job in this workflow`},
		{"duplicate-job.yml", 10, "jobs.build is declared twice"},
		{"bad-uses.yml", 9, `must be pinned to a ref`},
		{"bad-cron.yml", 5, "is not a valid five-field cron expression"},
		{"no-runs-on.yml", 7, "must declare `runs-on:`"},
		{"unknown-event.yml", 4, "unsupported: on.pushh is not a known key"},
		{"bad-retry-class.yml", 12, "must list only user, infra or config"},
		{"bad-permission.yml", 6, "must be read, write or none"},
		{"exclude-unknown-key.yml", 12, `names "platform", which is not a matrix key`},
		{"yaml-anchor.yml", 7, "uses a YAML alias"},
		{"with-on-run-step.yml", 9, "sets `with:` on a `run:` step"},
		{"broken-yaml.yml", 6, "invalid workflow file"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join("testdata", "invalid", c.file)
			src, err := os.ReadFile(path)
			require.NoError(t, err)

			w, err := Parse(path, src)
			require.Error(t, err, "this file must not parse")
			require.Nil(t, w)

			var pe *ParseError
			require.ErrorAs(t, err, &pe)
			require.Equal(t, path, pe.Path)
			require.Equal(t, c.line, pe.Line, "wrong line in %q", err)
			require.Contains(t, err.Error(), c.msg)
			require.True(t, strings.HasPrefix(err.Error(), path+":"), "the error must lead with the file location: %q", err)
		})
	}
}

// TestEveryInvalidFileIsCovered stops a new testdata/invalid file from sitting
// there unasserted.
func TestEveryInvalidFileIsCovered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "invalid", "*.yml"))
	require.NoError(t, err)
	for _, f := range files {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = Parse(f, src)
		require.Error(t, err, "%s is in testdata/invalid but parses cleanly", f)
	}
}

func hasDeviation(w *model.Workflow, path string) bool {
	for _, d := range w.Deviations {
		if d.Path == path {
			return true
		}
	}
	return false
}
