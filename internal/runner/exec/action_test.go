package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/actions"
)

// writeAction lays out an action on the host, the way the resolver would.
func writeAction(t *testing.T, yaml string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte(yaml), 0o644))
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return dir
}

func withResolver(r *fakeResolver) func(*Config) {
	return func(c *Config) { c.Actions = r }
}

func TestDockerActionIsUnsupportedNotSkipped(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "docker://alpine:3"}},
		withResolver(&fakeResolver{}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.Equal(t, model.ClassConfig, res.Class)
	assert.True(t, h.log.contains("unsupported: container actions"))
}

func TestDockerRunsUsingIsUnsupported(t *testing.T) {
	dir := writeAction(t, "name: d\nruns:\n  using: docker\n  image: Dockerfile\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/p@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/p@v1": dir}}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("unsupported:"))
	assert.True(t, h.log.contains("Dockerfile"))
}

func TestUnknownRunsUsingIsUnsupported(t *testing.T) {
	dir := writeAction(t, "name: d\nruns:\n  using: node12\n  main: i.js\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/p@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/p@v1": dir}}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains(`runs.using "node12"`))
}

func TestUnresolvableActionIsConfigErrorNamingTheRef(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "octo/missing@v9"}},
		withResolver(&fakeResolver{dirs: map[string]string{}}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("octo/missing@v9"))
	assert.True(t, h.log.contains("unable to resolve action"))
}

func TestNoResolverConfigured(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/p@v1"}}, nil)
	res := h.ex.Run(context.Background())
	assert.True(t, res.Conclusion.IsFailure())
	assert.True(t, h.log.contains("no action resolver configured"))
}

const nodeActionYAML = `
name: Greet
description: greets
inputs:
  who-to-greet:
    description: whom
    default: World
  token:
    description: t
    required: true
runs:
  using: node20
  main: dist/index.js
  pre: dist/pre.js
  post: dist/post.js
`

func TestJavaScriptActionRunsMainPreAndPost(t *testing.T) {
	dir := writeAction(t, nodeActionYAML, map[string]string{
		"dist/index.js": "//main", "dist/pre.js": "//pre", "dist/post.js": "//post",
	})
	steps := []protocol.StepSpec{{
		Number: 1, Name: "greet", Uses: "o/greet@v1",
		With: map[string]string{"token": "abc"},
	}}
	h := newHarness(t, steps, withResolver(&fakeResolver{dirs: map[string]string{"o/greet@v1": dir}}))
	h.sb.binaries["node20"] = "/usr/bin/node20"

	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	require.Len(t, h.sb.runs, 3, "pre, main, post")

	assert.True(t, strings.HasSuffix(h.sb.runs[0].Argv[1], "dist/pre.js"))
	assert.True(t, strings.HasSuffix(h.sb.runs[1].Argv[1], "dist/index.js"))
	assert.True(t, strings.HasSuffix(h.sb.runs[2].Argv[1], "dist/post.js"))
	assert.Equal(t, "/usr/bin/node20", h.sb.runs[1].Argv[0])

	env := h.sb.runs[1].Env
	assert.Equal(t, "abc", env["INPUT_TOKEN"])
	assert.Equal(t, "World", env["INPUT_WHO-TO-GREET"], "the declared default is applied")
	assert.NotEmpty(t, env["GITHUB_ACTION_PATH"])
	assert.NotEmpty(t, env["GITHUB_OUTPUT"], "env files are wired for actions too")

	// The action was placed in the sandbox rather than bind-mounted.
	assert.NotEmpty(t, h.sb.copies)
}

func TestJavaScriptPostRunsEvenWhenTheJobFails(t *testing.T) {
	dir := writeAction(t, nodeActionYAML, map[string]string{
		"dist/index.js": "//main", "dist/pre.js": "//pre", "dist/post.js": "//post",
	})
	steps := []protocol.StepSpec{
		{Number: 1, Uses: "o/greet@v1", With: map[string]string{"token": "abc"}},
		{Number: 2, Name: "fail", Run: "false"},
	}
	h := newHarness(t, steps, withResolver(&fakeResolver{dirs: map[string]string{"o/greet@v1": dir}}))
	h.sb.binaries["node"] = "/usr/bin/node"

	var postRan bool
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		if strings.HasSuffix(req.Argv[len(req.Argv)-1], "post.js") {
			postRan = true
		}
		if req.Argv[0] == "bash" {
			return RunResult{ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionFailure, res.Conclusion)
	assert.True(t, postRan, "post must run even after a failure, or actions cannot clean up")
}

func TestJavaScriptActionWithoutNodeIsInfra(t *testing.T) {
	dir := writeAction(t, "name: n\nruns:\n  using: node24\n  main: i.js\n", map[string]string{"i.js": "//x"})
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/p@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/p@v1": dir}}))
	// No node in the sandbox image.
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionInfraFailure, res.Conclusion, "a missing runtime is the image's fault, not the workflow's")
	assert.Equal(t, model.ClassInfra, res.Class)
	assert.True(t, h.log.contains("node"))
}

func TestJavaScriptActionMissingMain(t *testing.T) {
	dir := writeAction(t, "name: n\nruns:\n  using: node20\n  post: p.js\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/p@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/p@v1": dir}}))
	h.sb.binaries["node"] = "/usr/bin/node"
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("no runs.main"))
}

func TestRequiredInputWithoutValueIsConfigError(t *testing.T) {
	dir := writeAction(t, nodeActionYAML, map[string]string{"dist/index.js": "//x"})
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/greet@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/greet@v1": dir}}))
	h.sb.binaries["node"] = "/usr/bin/node"
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains(`required input "token"`))
}

const compositeYAML = `
name: Composite
description: c
inputs:
  greeting:
    default: hello
outputs:
  said:
    description: what was said
    value: ${{ steps.say.outputs.text }}
runs:
  using: composite
  steps:
    - id: say
      shell: bash
      run: echo ${{ inputs.greeting }}
    - shell: sh
      run: echo second
`

func TestCompositeActionRunsStepsWithInputScope(t *testing.T) {
	dir := writeAction(t, compositeYAML, nil)
	steps := []protocol.StepSpec{
		{Number: 1, Name: "composite", Uses: "o/c@v1", With: map[string]string{"greeting": "bonjour"}},
		{Number: 2, Name: "after", Run: "echo", Env: map[string]string{"SAID": "${{ steps.composite.outputs.said }}"}},
	}
	steps[0].ID = "composite"
	h := newHarness(t, steps, withResolver(&fakeResolver{dirs: map[string]string{"o/c@v1": dir}}))

	scripts := map[string]string{}
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		p := req.Argv[len(req.Argv)-1]
		scripts[p] = sb.script(p)
		if strings.Contains(sb.script(p), "bonjour") {
			sb.write(req.Env["GITHUB_OUTPUT"], "text=bonjour world\n")
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	require.Len(t, h.sb.runs, 3, "two composite steps plus the following job step")

	var sawExpanded bool
	for _, body := range scripts {
		if body == "echo bonjour" {
			sawExpanded = true
		}
	}
	assert.True(t, sawExpanded, "${{ inputs.greeting }} is expanded in the composite scope: %v", scripts)
	assert.Equal(t, "sh", h.sb.runs[1].Argv[0], "each composite step uses its own shell")
	assert.Equal(t, "bonjour world", res.Steps[0].Outputs["said"], "declared composite outputs are evaluated")
	assert.Equal(t, "bonjour world", h.sb.runs[2].Env["SAID"])
}

func TestCompositeStepWithoutShellIsConfigError(t *testing.T) {
	dir := writeAction(t, "name: c\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/c@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/c@v1": dir}}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("without the required shell:"))
}

func TestCompositeStepWithNeitherRunNorUses(t *testing.T) {
	dir := writeAction(t, "name: c\nruns:\n  using: composite\n  steps:\n    - name: nothing\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/c@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/c@v1": dir}}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("neither run: nor uses:"))
}

func TestCompositeFailingStepFailsTheAction(t *testing.T) {
	dir := writeAction(t, compositeYAML, nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/c@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/c@v1": dir}}))
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		return RunResult{ExitCode: 3}, nil
	}
	res := h.ex.Run(context.Background())
	assert.True(t, res.Conclusion.IsFailure())
	assert.True(t, h.log.contains("had a failing step"))
	assert.Equal(t, 1, len(h.sb.runs), "a failed composite step stops the rest of the action")
}

func TestNestedCompositeRecursionIsBounded(t *testing.T) {
	// An action that uses itself: without a depth bound this never terminates.
	dir := writeAction(t, "name: loop\nruns:\n  using: composite\n  steps:\n    - uses: o/loop@v1\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/loop@v1"}},
		func(c *Config) {
			c.Actions = &fakeResolver{dirs: map[string]string{"o/loop@v1": dir}}
			c.MaxCompositeDepth = 3
		})
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("depth limit of 3"))
}

func TestNestedCompositeUsesWorks(t *testing.T) {
	inner := writeAction(t, "name: inner\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo inner\n", nil)
	outer := writeAction(t, "name: outer\nruns:\n  using: composite\n  steps:\n    - uses: o/inner@v1\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/outer@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{
			"o/outer@v1": outer, "o/inner@v1": inner,
		}}))
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	require.Len(t, h.sb.runs, 1)
	assert.Equal(t, "bash", h.sb.runs[0].Argv[0])
}

func TestLocalActionIsReadFromTheSandbox(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "./.github/actions/build"}},
		withResolver(&fakeResolver{}))
	h.sb.write("/workspace/.github/actions/build/action.yml",
		"name: local\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo local\n")

	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	require.Len(t, h.sb.runs, 1)
	assert.Empty(t, h.sb.copies, "a local action is already in the workspace and is not copied")
}

func TestLocalActionMissingMetadata(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "./missing"}}, withResolver(&fakeResolver{}))
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("no action.yml or action.yaml"))
}

func TestActionWithExpressionInWithFromComposite(t *testing.T) {
	inner := writeAction(t, `
name: inner
inputs:
  value:
    required: true
runs:
  using: composite
  steps:
    - shell: bash
      run: echo ${{ inputs.value }}
`, nil)
	outer := writeAction(t, `
name: outer
inputs:
  passed:
    default: from-outer
runs:
  using: composite
  steps:
    - uses: o/inner@v1
      with:
        value: ${{ inputs.passed }}
`, nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/outer@v1"}},
		withResolver(&fakeResolver{dirs: map[string]string{"o/outer@v1": outer, "o/inner@v1": inner}}))

	var body string
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		body = sb.script(req.Argv[len(req.Argv)-1])
		return RunResult{}, nil
	}
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.Equal(t, "echo from-outer", body, "a nested with: is evaluated in the enclosing action's scope")
}

func TestCachedActionIsReported(t *testing.T) {
	dir := writeAction(t, "name: c\nruns:\n  using: composite\n  steps: []\n", nil)
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Uses: "o/c@v1"}}, func(c *Config) {
		c.Actions = &cachedResolver{inner: &fakeResolver{dirs: map[string]string{"o/c@v1": dir}}}
	})
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.True(t, h.log.contains("action cache"))
}

type cachedResolver struct{ inner *fakeResolver }

func (c *cachedResolver) Resolve(ctx context.Context, uses string) (actions.Resolved, error) {
	r, err := c.inner.Resolve(ctx, uses)
	r.Cached = true
	return r, err
}
