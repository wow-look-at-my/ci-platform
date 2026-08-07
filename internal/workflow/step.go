package workflow

import (
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

var stepKeys = []string{
	"name", "id", "if", "uses", "run", "with", "env", "shell",
	"working-directory", "continue-on-error", "timeout-minutes", "retry",
}

// supportedShells are the interpreters the runner knows how to invoke.
// unsupportedShells are real GHA shells this platform does not implement; they
// are named separately so the error says "not implemented" rather than
// "unknown", which is a different thing for the person reading it.
var (
	supportedShells   = []string{"bash", "sh", "python", "node"}
	unsupportedShells = []string{"cmd", "pwsh", "powershell"}
)

func (p *parser) step(n *yaml.Node, where string, number int) (*model.StepIR, error) {
	s := &model.StepIR{Number: number}
	var sawRun, sawUses bool
	err := p.each(n, where, stepKeys, func(key string, kn, vn *yaml.Node) error {
		at := where + "." + key
		var err error
		switch key {
		case "name":
			s.Name, err = p.expr(vn, at)
		case "id":
			s.ID, err = p.nonEmpty(vn, at)
		case "if":
			s.If, err = p.condition(vn, at)
		case "uses":
			sawUses = true
			s.Uses, err = p.nonEmpty(vn, at)
			if err == nil {
				err = p.usesRef(vn, at, s.Uses)
			}
		case "run":
			sawRun = true
			s.Run, err = p.expr(vn, at)
		case "with":
			s.With, err = p.exprMap(vn, at)
		case "env":
			s.Env, err = p.exprMap(vn, at)
		case "shell":
			s.Shell, err = p.nonEmpty(vn, at)
			if err == nil {
				err = p.checkShell(vn, at, s.Shell)
			}
		case "working-directory":
			s.WorkingDirectory, err = p.expr(vn, at)
		case "continue-on-error":
			s.ContinueOnError, err = p.expr(vn, at)
		case "timeout-minutes":
			s.TimeoutMinutes, err = p.expr(vn, at)
		case "retry":
			s.Retry, err = p.retry(vn, at)
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	// workflow-v1.0.json splits steps into `run-step` and `regular-step`: each
	// side's keys are invalid on the other, and a step must be exactly one.
	switch {
	case sawRun && sawUses:
		return nil, p.errf(n, "%s sets both `run:` and `uses:`; a step is one or the other", where)
	case !sawRun && !sawUses:
		return nil, p.errf(n, "%s must set either `run:` or `uses:`", where)
	case sawRun && s.With != nil:
		return nil, p.errf(n, "%s sets `with:` on a `run:` step; `with:` passes inputs to an action", where)
	case sawUses && s.Shell != "":
		return nil, p.errf(n, "%s sets `shell:` on a `uses:` step; the action chooses its own runtime", where)
	case sawUses && !s.WorkingDirectory.Empty():
		return nil, p.errf(n, "%s sets `working-directory:` on a `uses:` step", where)
	case sawRun && strings.TrimSpace(s.Run.Raw) == "":
		return nil, p.errf(n, "%s has an empty `run:`", where)
	}
	return s, nil
}

func (p *parser) checkShell(n *yaml.Node, where, shell string) error {
	if containsStr(supportedShells, shell) {
		return nil
	}
	if containsStr(unsupportedShells, shell) {
		return p.errf(n, "unsupported: %s %q is not implemented", where, shell)
	}
	// A custom shell template ("/bin/bash -e {0}") is a real GHA feature that
	// this platform does not run.
	if strings.ContainsAny(shell, " {") {
		return p.errf(n, "unsupported: %s %q is a custom shell template, which is not implemented", where, shell)
	}
	return p.errf(n, "%s %q is not a known shell (known shells: %s)", where, shell,
		strings.Join(append(append([]string{}, supportedShells...), unsupportedShells...), ", "))
}
