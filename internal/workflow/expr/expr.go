// Package expr implements the GitHub Actions expression language: a lexer, a
// precedence-climbing parser, and an evaluator over a set of named contexts.
//
// Two behaviours here are load-bearing and easy to get wrong. Comparisons are
// LOOSE (null, false, 0 and "" all compare equal, strings compare
// case-insensitively) and `&&`/`||` return one of their OPERANDS rather than a
// boolean, so `github.head_ref || github.ref_name` yields a string.
package expr

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Context holds the named-value contexts: "github", "env", "vars", "secrets",
// "job", "jobs", "steps", "runner", "needs", "strategy", "matrix", "inputs".
// A name absent from the map is an error, never null: a workflow referring to a
// context that does not exist here is a config error.
type Context map[string]any

// Status governs success()/failure()/always()/cancelled().
type Status struct {
	Success   bool // no previous step/job failed
	Failure   bool
	Cancelled bool
}

// Evaluator evaluates expressions against one Context. It is immutable: the
// With* methods return a copy.
type Evaluator struct {
	ctx    Context
	status Status
	fsys   fs.FS
	root   string
}

// New returns an Evaluator whose status is "nothing has failed yet", so
// success() is true and failure()/cancelled() are false.
func New(c Context) *Evaluator {
	return &Evaluator{ctx: c, status: Status{Success: true}}
}

// WithStatus returns a copy whose status functions report s.
func (e *Evaluator) WithStatus(s Status) *Evaluator {
	c := *e
	c.status = s
	return &c
}

// WithFS returns a copy that can evaluate hashFiles(). root is the workspace
// directory within fsys that patterns are matched relative to; "" means the
// root of fsys itself. Without this, hashFiles() is an error rather than a
// fabricated digest.
func (e *Evaluator) WithFS(fsys fs.FS, root string) *Evaluator {
	c := *e
	c.fsys = fsys
	c.root = root
	return &c
}

// Eval evaluates ONE expression body, without the ${{ }} wrapper.
func (e *Evaluator) Eval(raw string) (any, error) {
	n, err := parse(raw)
	if err != nil {
		return nil, err
	}
	return e.eval(n)
}

// EvalString interpolates every ${{ }} in a template and returns the rest
// verbatim.
func (e *Evaluator) EvalString(raw string) (string, error) {
	var b strings.Builder
	rest := raw
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:i])
		body, after, ok := splitExpr(rest[i+3:])
		if !ok {
			return "", fmt.Errorf("unterminated expression: %q", rest[i:])
		}
		v, err := e.Eval(body)
		if err != nil {
			return "", err
		}
		b.WriteString(stringify(v))
		rest = after
	}
}

// EvalBool evaluates raw as a condition. A bare `if: foo` is treated as
// `${{ foo }}`, matching GitHub Actions.
func (e *Evaluator) EvalBool(raw string) (bool, error) {
	src := strings.TrimSpace(raw)
	if src == "" {
		return false, nil
	}
	if strings.HasPrefix(src, "${{") {
		body, after, ok := splitExpr(src[3:])
		if ok && strings.TrimSpace(after) == "" {
			src = body
		}
	}
	v, err := e.Eval(src)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// EvalExpr is EvalString over an IR expression.
func (e *Evaluator) EvalExpr(x model.Expr) (string, error) { return e.EvalString(x.Raw) }

// splitExpr finds the "}}" that closes an expression body, ignoring braces
// inside single-quoted strings so that format('}}') survives. s starts just
// after the opening "${{".
func splitExpr(s string) (body, rest string, ok bool) {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			// '' inside a string is an escaped quote, and skipping the second
			// one here keeps inStr correct.
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
		case '}':
			if !inStr && i+1 < len(s) && s[i+1] == '}' {
				return s[:i], s[i+2:], true
			}
		}
	}
	return "", "", false
}
