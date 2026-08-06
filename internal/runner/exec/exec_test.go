package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

type harness struct {
	sb  *fakeSandbox
	log *recordingLog
	rep *recordingReporter
	ex  *Executor
	asg *protocol.Assignment
	msk *mask.Masker
}

func newHarness(t *testing.T, steps []protocol.StepSpec, tweak func(*Config)) *harness {
	t.Helper()
	h := &harness{
		sb:  newFakeSandbox(),
		log: &recordingLog{},
		rep: &recordingReporter{},
		msk: mask.New(),
		asg: &protocol.Assignment{
			RunID: 7, JobID: 42, Attempt: 1,
			IdempotencyKey: "7/42/1",
			JobName:        "build", JobKey: "build",
			RepoOwner: "octo", RepoName: "demo",
			HeadSHA: "abc", HeadRef: "refs/heads/main",
			Steps:     steps,
			Env:       map[string]string{"JOB_LEVEL": "job"},
			ServerURL: "https://ci.example.localhost",
			JobToken:  "job-token-value",
			Retry:     model.RetryPolicy{Attempts: 1},
		},
	}
	cfg := Config{
		Assignment:   h.asg,
		Sandbox:      h.sb,
		Log:          h.log,
		Reporter:     h.rep,
		Masker:       h.msk,
		NewEvaluator: newFakeEvaluatorFactory(nil),
		WorkspaceDir: "/workspace",
		TempDir:      "/tmp/_temp",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	ex, err := New(cfg)
	require.NoError(t, err)
	h.ex = ex
	return h
}

func TestNewValidatesConfig(t *testing.T) {
	_, err := New(Config{})
	require.ErrorContains(t, err, "Assignment is required")

	_, err = New(Config{Assignment: &protocol.Assignment{}})
	require.ErrorContains(t, err, "Sandbox is required")

	_, err = New(Config{Assignment: &protocol.Assignment{}, Sandbox: newFakeSandbox()})
	require.ErrorContains(t, err, "Log is required")
}

func TestRunSimpleBashStep(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Name: "greet", Run: "echo hi"}}, nil)
	var script string
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		// Read it here: the executor removes the script once the step ends.
		script = sb.script(req.Argv[len(req.Argv)-1])
		fmt.Fprintln(req.Stdout, "hi")
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionSuccess, res.Conclusion)
	require.Len(t, res.Steps, 1)
	assert.Equal(t, model.ConclusionSuccess, res.Steps[0].Conclusion)
	assert.True(t, h.log.contains("hi"))

	require.Len(t, h.sb.runs, 1)
	run := h.sb.runs[0]
	assert.Equal(t, "bash", run.Argv[0])
	assert.Equal(t, []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail"}, run.Argv[:6])
	assert.True(t, strings.HasSuffix(run.Argv[6], ".sh"))
	assert.Equal(t, "echo hi", script)
	assert.Equal(t, "/workspace", run.WorkingDir)

	assert.Equal(t, "octo/demo", run.Env["GITHUB_REPOSITORY"])
	assert.Equal(t, "true", run.Env["CI"])
	assert.Equal(t, "job", run.Env["JOB_LEVEL"])
	assert.Equal(t, "7", run.Env["GITHUB_RUN_ID"])
	assert.NotEmpty(t, run.Env["GITHUB_OUTPUT"])

	require.Len(t, h.rep.started, 1)
	require.Len(t, h.rep.ended, 1)
}

func TestFailingStepIsClassifiedAsUserAndRecorded(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Name: "test", Run: "false"}}, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		fmt.Fprintln(req.Stdout, "FAIL TestThing")
		return RunResult{ExitCode: 1}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionFailure, res.Conclusion)
	assert.Equal(t, model.ClassUser, res.Class)
	assert.Contains(t, res.Explanation, "exit code 1")
	require.Len(t, res.ClassificationLog, 1)
	assert.Contains(t, res.ClassificationLog[0], "classified user")
	// The decision must also be visible in the job log, not only in the API.
	assert.True(t, h.log.contains("classified user"))
	assert.Contains(t, h.rep.ended[0].ClassReason, "classified user")
}

func TestInfraFailureIsRetriedAndEveryAttemptIsLogged(t *testing.T) {
	steps := []protocol.StepSpec{{
		Number: 1, Name: "pull", Run: "docker pull x",
		Retry: &model.RetryPolicy{
			Attempts: 3, On: []model.FailureClass{model.ClassInfra},
			Backoff: model.BackoffNone,
		},
	}}
	h := newHarness(t, steps, nil)

	calls := 0
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		calls++
		if calls < 3 {
			fmt.Fprintf(req.Stderr, "attempt %d: TLS handshake timeout\n", calls)
			return RunResult{ExitCode: 1}, nil
		}
		fmt.Fprintln(req.Stdout, "pulled")
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.Equal(t, 3, res.Steps[0].Attempts)
	assert.Equal(t, 3, calls)

	assert.True(t, h.log.contains("Attempt 1 of 3"))
	assert.True(t, h.log.contains("Attempt 2 of 3"))
	assert.True(t, h.log.contains("Attempt 3 of 3"))
	// No attempt's output is overwritten by a later one.
	assert.True(t, h.log.contains("attempt 1: TLS handshake timeout"))
	assert.True(t, h.log.contains("attempt 2: TLS handshake timeout"))
	assert.Equal(t, 2, h.log.count("classified infra"))
}

func TestUserFailureIsNotRetried(t *testing.T) {
	steps := []protocol.StepSpec{{
		Number: 1, Run: "false",
		Retry: &model.RetryPolicy{Attempts: 3, On: []model.FailureClass{model.ClassInfra}, Backoff: model.BackoffNone},
	}}
	h := newHarness(t, steps, nil)
	calls := 0
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		calls++
		return RunResult{ExitCode: 2}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, 1, calls, "a user failure must never be retried")
	assert.Equal(t, model.ClassUser, res.Class)
	assert.Equal(t, 2, res.Steps[0].ExitCode)
}

func TestContinueOnError(t *testing.T) {
	steps := []protocol.StepSpec{
		{Number: 1, Name: "flaky", Run: "false", ContinueOnError: true},
		{Number: 2, Name: "after", Run: "echo ok"},
	}
	h := newHarness(t, steps, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		if strings.Contains(sb.script(req.Argv[len(req.Argv)-1]), "false") {
			return RunResult{ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionSuccess, res.Conclusion, "the job survives a continue-on-error step")
	assert.Equal(t, model.ConclusionFailure, res.Steps[0].Outcome, "the step still reports what happened")
	assert.Equal(t, model.ConclusionSuccess, res.Steps[0].Conclusion)
	assert.Equal(t, model.ConclusionSuccess, res.Steps[1].Conclusion, "the next step still runs")
}

func TestStepsAfterFailureAreSkipped(t *testing.T) {
	steps := []protocol.StepSpec{
		{Number: 1, Name: "boom", Run: "false"},
		{Number: 2, Name: "never", Run: "echo no"},
		{Number: 3, Name: "always", Run: "echo cleanup", IfExpr: "always()"},
	}
	h := newHarness(t, steps, nil)
	ran := map[string]bool{}
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		script := sb.script(req.Argv[len(req.Argv)-1])
		ran[script] = true
		if script == "false" {
			return RunResult{ExitCode: 1}, nil
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionFailure, res.Conclusion)
	assert.Equal(t, model.ConclusionSkipped, res.Steps[1].Conclusion)
	assert.Equal(t, model.ConclusionSuccess, res.Steps[2].Conclusion)
	assert.False(t, ran["echo no"])
	assert.True(t, ran["echo cleanup"])
	assert.True(t, h.log.contains("an earlier step failed"))
}

func TestIfExpressionFalseSkips(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "echo x", IfExpr: "false"}}, nil)
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionSkipped, res.Steps[0].Conclusion)
	assert.Empty(t, h.sb.runs)
	assert.True(t, h.log.contains(`if: "false" evaluated false`))
}

func TestIfExpressionErrorIsConfigFailure(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "echo x", IfExpr: "bogus()"}}, func(c *Config) {
		c.NewEvaluator = newFakeEvaluatorFactory(map[string]bool{"bogus()": true})
	})
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.Equal(t, model.ClassConfig, res.Class)
	assert.True(t, h.log.contains("classified config"))
}

func TestMissingEvaluatorFailsLoudly(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "echo x", IfExpr: "success()"}}, func(c *Config) {
		c.NewEvaluator = nil
	})
	res := h.ex.Run(context.Background())
	assert.True(t, res.Conclusion.IsFailure(), "a missing evaluator must fail, never be treated as true")
	assert.True(t, h.log.contains("no expression evaluator configured"))
}

func TestStepOutputsFlowToLaterSteps(t *testing.T) {
	steps := []protocol.StepSpec{
		{Number: 1, ID: "meta", Run: "compute"},
		{Number: 2, Name: "use", Run: "echo", Env: map[string]string{"SHA": "${{ steps.meta.outputs.sha }}"}},
	}
	h := newHarness(t, steps, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		if out := req.Env["GITHUB_OUTPUT"]; strings.Contains(out, "_1_") {
			sb.write(out, "sha=deadbeef\nmulti<<EOF\nline1\nline2\nEOF\n")
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.Equal(t, "deadbeef", res.Steps[0].Outputs["sha"])
	assert.Equal(t, "line1\nline2", res.Steps[0].Outputs["multi"])
	assert.Equal(t, "deadbeef", h.sb.runs[1].Env["SHA"])
}

func TestGithubEnvAndPathAffectLaterSteps(t *testing.T) {
	steps := []protocol.StepSpec{
		{Number: 1, Run: "setup"},
		{Number: 2, Run: "use"},
	}
	h := newHarness(t, steps, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		if strings.Contains(req.Env["GITHUB_ENV"], "_1_") {
			sb.write(req.Env["GITHUB_ENV"], "TOOL_HOME=/opt/tool\n")
			sb.write(req.Env["GITHUB_PATH"], "/opt/tool/bin\n")
			sb.write(req.Env["GITHUB_STEP_SUMMARY"], "# Results\n")
		}
		return RunResult{}, nil
	}

	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	second := h.sb.runs[1].Env
	assert.Equal(t, "/opt/tool", second["TOOL_HOME"])
	assert.True(t, strings.HasPrefix(second["PATH"], "/opt/tool/bin:"), "PATH was %q", second["PATH"])
	assert.True(t, h.log.contains("step summary"))
}

func TestMalformedEnvFileFailsTheStep(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		sb.write(req.Env["GITHUB_OUTPUT"], "this is not a key value line\n")
		return RunResult{}, nil
	}
	res := h.ex.Run(context.Background())
	assert.True(t, res.Conclusion.IsFailure())
	assert.True(t, h.log.contains("GITHUB_OUTPUT"))
}

func TestEnvPrecedenceWorkflowJobStep(t *testing.T) {
	steps := []protocol.StepSpec{{Number: 1, Run: "x", Env: map[string]string{"LEVEL": "step"}}}
	h := newHarness(t, steps, func(c *Config) {
		c.WorkflowEnv = map[string]string{"LEVEL": "workflow", "ONLY_WF": "1"}
		c.Assignment.Env = map[string]string{"LEVEL": "job", "ONLY_JOB": "1"}
	})
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	env := h.sb.runs[0].Env
	assert.Equal(t, "step", env["LEVEL"])
	assert.Equal(t, "1", env["ONLY_WF"])
	assert.Equal(t, "1", env["ONLY_JOB"])
}

func TestWorkingDirectoryLayers(t *testing.T) {
	steps := []protocol.StepSpec{
		{Number: 1, Run: "a"},
		{Number: 2, Run: "b", WorkingDirectory: "sub"},
		{Number: 3, Run: "c", WorkingDirectory: "/abs"},
	}
	h := newHarness(t, steps, func(c *Config) { c.Assignment.WorkingDirectory = "app" })
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.Equal(t, "/workspace/app", h.sb.runs[0].WorkingDir)
	assert.Equal(t, "/workspace/app/sub", h.sb.runs[1].WorkingDir)
	assert.Equal(t, "/abs", h.sb.runs[2].WorkingDir)
}

func TestSecretsNeverReachTheLog(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "leak"}}, nil)
	h.asg.Secrets = map[string]string{"API_KEY": "sup3r-s3cret-value"}
	h.msk.AddAll(h.asg.Secrets)

	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		fmt.Fprintln(req.Stdout, "key is sup3r-s3cret-value")
		fmt.Fprintln(req.Stdout, "b64 is c3VwM3ItczNjcmV0LXZhbHVl")
		fmt.Fprintln(req.Stderr, "url is sup3r-s3cret-value")
		fmt.Fprintln(req.Stdout, "::add-mask::dynamic-secret-1")
		fmt.Fprintln(req.Stdout, "later dynamic-secret-1 appears")
		return RunResult{}, nil
	}

	h.ex.Run(context.Background())
	text := h.log.text()
	assert.NotContains(t, text, "sup3r-s3cret-value")
	assert.NotContains(t, text, "c3VwM3ItczNjcmV0LXZhbHVl")
	assert.NotContains(t, text, "dynamic-secret-1")
	assert.Contains(t, text, "***")
}

func TestWorkflowCommandsProduceAnnotationsAndGroups(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		fmt.Fprintln(req.Stdout, "::group::Compile")
		fmt.Fprintln(req.Stdout, "compiling")
		fmt.Fprintln(req.Stdout, "::error file=main.go,line=4,title=vet::undefined: x")
		fmt.Fprintln(req.Stdout, "::endgroup::")
		return RunResult{}, nil
	}

	h.ex.Run(context.Background())
	require.Len(t, h.rep.annots, 1)
	assert.Equal(t, "main.go", h.rep.annots[0].Path)
	assert.Equal(t, 4, h.rep.annots[0].StartLine)

	var grouped bool
	for _, l := range h.log.lines {
		if l.text == "compiling" {
			grouped = l.group == "Compile"
		}
	}
	assert.True(t, grouped, "output inside a ::group:: carries the group title")
}

func TestPartialFinalLineIsNotLost(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		fmt.Fprint(req.Stdout, "no trailing newline")
		return RunResult{}, nil
	}
	h.ex.Run(context.Background())
	assert.True(t, h.log.contains("no trailing newline"))
}

func TestStepTimeout(t *testing.T) {
	steps := []protocol.StepSpec{{Number: 1, Run: "sleep", TimeoutMinutes: 1}}
	h := newHarness(t, steps, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		// Simulate the sandbox observing its context deadline.
		return RunResult{ExitCode: 124}, nil
	}
	// A one-minute timeout is not waited out; the executor's own deadline
	// bookkeeping is what is under test, via a context already past due.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	res := h.ex.Run(ctx)
	assert.Equal(t, model.ConclusionCancelled, res.Conclusion)
}

func TestUnsupportedShellsAreConfigErrors(t *testing.T) {
	for _, shell := range []string{"pwsh", "powershell", "cmd"} {
		t.Run(shell, func(t *testing.T) {
			h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x", Shell: shell}}, nil)
			res := h.ex.Run(context.Background())
			assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
			assert.Equal(t, model.ClassConfig, res.Class)
			assert.True(t, h.log.contains("unsupported: shell"))
			assert.Empty(t, h.sb.runs, "an unsupported shell must never silently skip to a different one")
		})
	}
}

func TestStepWithNeitherRunNorUses(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Name: "empty"}}, nil)
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionConfigError, res.Conclusion)
	assert.True(t, h.log.contains("neither run: nor uses:"))
}

func TestSandboxErrorIsInfra(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, nil)
	h.sb.handle = func(sb *fakeSandbox, req RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("cannot connect to the Docker daemon")
	}
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ClassInfra, res.Class)
	assert.Equal(t, model.ConclusionInfraFailure, res.Conclusion)
}

func TestDefaultShellFromAssignment(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, func(c *Config) {
		c.Assignment.DefaultShell = "sh"
	})
	res := h.ex.Run(context.Background())
	require.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.Equal(t, []string{"sh", "-e"}, h.sb.runs[0].Argv[:2])
}

func TestReporterErrorsAreLoggedNotFatal(t *testing.T) {
	h := newHarness(t, []protocol.StepSpec{{Number: 1, Run: "x"}}, nil)
	h.rep.err = errors.New("control plane down")
	res := h.ex.Run(context.Background())
	assert.Equal(t, model.ConclusionSuccess, res.Conclusion)
	assert.True(t, h.log.contains("control plane down"))
}

func TestServerURLWarningForArtifactClients(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.asg.ServerURL = "https://ci.internal.example.com"
	h.ex.Run(context.Background())
	assert.True(t, h.log.contains(".ghe.com"), "the operator is told why artifact actions will refuse to run")
}

func TestServerURLAcceptedHosts(t *testing.T) {
	for _, u := range []string{"https://ci.example.localhost", "https://acme.ghe.com", "http://localhost:8080", ""} {
		h := newHarness(t, nil, nil)
		h.asg.ServerURL = u
		h.ex.Run(context.Background())
		assert.False(t, h.log.contains(".ghe.com"), "unexpected warning for %q", u)
	}
}
