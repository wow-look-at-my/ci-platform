package exec

import (
	"fmt"
	"strings"
)

// shellSpec is how one shell turns a script file into a command line.
type shellSpec struct {
	command string
	// argFormat contains {0}, replaced by the script path.
	argFormat string
	ext       string
}

// builtinShells mirrors the Actions runner's own table
// (Runner.Worker/Handlers/ScriptHandlerHelpers.cs). bash gets pipefail, sh does
// not, because sh is the POSIX fallback and does not have the option.
var builtinShells = map[string]shellSpec{
	"bash":   {command: "bash", argFormat: "--noprofile --norc -e -o pipefail {0}", ext: ".sh"},
	"sh":     {command: "sh", argFormat: "-e {0}", ext: ".sh"},
	"python": {command: "python", argFormat: "{0}", ext: ".py"},
	// node is not a GitHub Actions built-in shell; it is ours, so a workflow
	// can run a JS snippet without wrapping it in an action.
	"node": {command: "node", argFormat: "{0}", ext: ".js"},
}

// unsupportedShells are real GitHub Actions shells this platform does not
// implement. They fail as config errors naming the shell rather than being
// skipped or silently rewritten to bash.
var unsupportedShells = map[string]string{
	"pwsh":       "PowerShell Core is not installed in the Linux sandbox image",
	"powershell": "Windows PowerShell is not available on a Linux runner",
	"cmd":        "cmd.exe is not available on a Linux runner",
}

// shellCommand builds the argv for a step's script and the path the script
// must be written to. The extension is chosen here, so the file written and
// the file named in argv can never disagree.
//
// A custom `shell:` is COMMAND [..ARGS] where ARGS must contain {0}; that is
// the Actions runner's rule, and without it there is no way to know where the
// script path goes.
func shellCommand(shell, scriptBase string) (argv []string, scriptPath string, err error) {
	shell = strings.TrimSpace(shell)
	if shell == "" {
		shell = "bash"
	}
	name := shell
	if i := strings.IndexByte(shell, ' '); i >= 0 {
		name = shell[:i]
	}
	if why, bad := unsupportedShells[strings.ToLower(name)]; bad {
		return nil, "", fmt.Errorf("unsupported: shell %q is not supported (%s)", name, why)
	}

	if spec, builtin := builtinShells[strings.ToLower(shell)]; builtin {
		return build(spec, scriptBase)
	}

	command, args, _ := strings.Cut(shell, " ")
	args = strings.TrimSpace(args)
	if args == "" {
		if spec, ok := builtinShells[strings.ToLower(command)]; ok {
			return build(spec, scriptBase)
		}
	}
	if !strings.Contains(args, "{0}") {
		return nil, "", fmt.Errorf("unsupported: shell %q must be a built-in (bash, sh, python, node) or a command whose arguments contain {0}", shell)
	}
	return append([]string{command}, formatArgs(args, scriptBase)...), scriptBase, nil
}

func build(spec shellSpec, scriptBase string) ([]string, string, error) {
	p := scriptBase + spec.ext
	return append([]string{spec.command}, formatArgs(spec.argFormat, p)...), p, nil
}

// formatArgs splits the argument template and substitutes the script path.
// Splitting on spaces before substituting keeps a path with spaces in one
// argument.
func formatArgs(format, scriptPath string) []string {
	fields := strings.Fields(format)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ReplaceAll(f, "{0}", scriptPath))
	}
	return out
}
