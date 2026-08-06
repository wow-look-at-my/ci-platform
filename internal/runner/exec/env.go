package exec

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/commands"
)

// DefaultPath is what PATH is rebuilt from when a step has added to
// $GITHUB_PATH. Set Config.BasePath when the sandbox image differs.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// baseEnv is the environment every step sees. Secrets are deliberately absent:
// the control plane resolves the ones a workflow references into Env, and the
// rest exist only to be masked.
func (e *Executor) baseEnv() map[string]string {
	a := e.cfg.Assignment
	env := map[string]string{
		"CI":                      "true",
		"GITHUB_ACTIONS":          "true",
		"GITHUB_WORKSPACE":        e.cfg.WorkspaceDir,
		"GITHUB_REPOSITORY":       a.RepoOwner + "/" + a.RepoName,
		"GITHUB_REPOSITORY_OWNER": a.RepoOwner,
		"GITHUB_SHA":              a.HeadSHA,
		"GITHUB_REF":              a.HeadRef,
		"GITHUB_REF_NAME":         strings.TrimPrefix(strings.TrimPrefix(a.HeadRef, "refs/heads/"), "refs/tags/"),
		"GITHUB_RUN_ID":           strconv.FormatInt(a.RunID, 10),
		"GITHUB_RUN_ATTEMPT":      strconv.Itoa(a.Attempt),
		"GITHUB_JOB":              a.JobKey,
		"GITHUB_SERVER_URL":       a.ServerURL,
		"RUNNER_TEMP":             e.cfg.TempDir,
		"RUNNER_WORKSPACE":        e.cfg.WorkspaceDir,
		"RUNNER_NAME":             e.cfg.RunnerName,
		"RUNNER_OS":               osName(e.cfg.RunnerOS),
		"RUNNER_ARCH":             archName(e.cfg.RunnerArch),
	}
	if e.cfg.RuntimeToken != "" {
		env["ACTIONS_RUNTIME_TOKEN"] = e.cfg.RuntimeToken
	}
	if e.cfg.ResultsURL != "" {
		env["ACTIONS_RESULTS_URL"] = e.cfg.ResultsURL
	}
	if e.cfg.IDTokenRequestURL != "" {
		// The OIDC client appends "&audience=", so the URL must already carry
		// a query string.
		url := e.cfg.IDTokenRequestURL
		if !strings.Contains(url, "?") {
			url += "?"
		}
		env["ACTIONS_ID_TOKEN_REQUEST_URL"] = url
		env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = e.cfg.RuntimeToken
	}
	return env
}

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "":
		return "Linux"
	default:
		return strings.ToUpper(goos[:1]) + goos[1:]
	}
}

func archName(goarch string) string {
	switch goarch {
	case "amd64":
		return "X64"
	case "386":
		return "X86"
	case "arm64":
		return "ARM64"
	case "arm":
		return "ARM"
	case "":
		return "X64"
	default:
		return strings.ToUpper(goarch)
	}
}

// stepEnv merges the layers a step sees, lowest precedence first: platform
// base, workflow env, job env (including what earlier steps wrote to
// $GITHUB_ENV), the enclosing action's env, then the step's own env.
func (e *Executor) stepEnv(scopeEnv, stepOwn map[string]string) map[string]string {
	env := e.baseEnv()
	for k, v := range e.cfg.WorkflowEnv {
		env[k] = v
	}
	for k, v := range e.jobEnv {
		env[k] = v
	}
	for k, v := range scopeEnv {
		env[k] = v
	}
	for k, v := range stepOwn {
		env[k] = v
	}
	if len(e.extraPath) > 0 {
		base := e.cfg.BasePath
		if base == "" {
			base = DefaultPath
		}
		if existing, ok := env["PATH"]; ok && existing != "" {
			base = existing
		}
		env["PATH"] = strings.Join(e.extraPath, ":") + ":" + base
	}
	return env
}

// buildStepEnv adds the step's own env (expression-expanded where it references
// something only the runner knows) and the per-step env file paths.
func (e *Executor) buildStepEnv(ctx context.Context, spec protocol.StepSpec, s *scope, files commands.EnvFiles) (map[string]string, error) {
	own := map[string]string{}
	for k, v := range spec.Env {
		if strings.Contains(v, "${{") {
			ev, err := e.evaluator(ctx, s.extraContexts())
			if err != nil {
				return nil, err
			}
			expanded, err := ev.EvalString(v)
			if err != nil {
				return nil, fmt.Errorf("expression error: evaluating env %q of step %q: %w", k, stepName(spec), err)
			}
			v = expanded
		}
		own[k] = v
	}
	var scopeEnv map[string]string
	if s != nil {
		scopeEnv = s.env
	}
	env := e.stepEnv(scopeEnv, own)
	for k, v := range files.EnvMap() {
		env[k] = v
	}
	if s != nil && s.actionDir != "" {
		env["GITHUB_ACTION_PATH"] = s.actionDir
	}
	return env, nil
}
