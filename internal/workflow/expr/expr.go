// Package expr implements the GitHub Actions expression language: a lexer, a
// precedence-climbing parser, and an evaluator over a set of named contexts.
//
// Behaviours here are load-bearing and easy to get wrong.
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
		body, after, ok := model.SplitExprBody(rest[i+3:])
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
		body, after, ok := model.SplitExprBody(src[3:])
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

// Validate reports a syntax error in any ${{ }} body of a template, without
// evaluating it. The parser uses this so a malformed expression is a config
// error at parse time rather than a surprise mid-run.
func Validate(raw string) error {
	rest := raw
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return nil
		}
		body, after, ok := model.SplitExprBody(rest[i+3:])
		if !ok {
			return fmt.Errorf("unterminated expression: %q", rest[i:])
		}
		if _, err := parse(body); err != nil {
			return err
		}
		rest = after
	}
}

// ValidateCondition validates an `if:` value, which may be a bare expression.
func ValidateCondition(raw string) error {
	src := strings.TrimSpace(raw)
	if src == "" {
		return nil
	}
	if strings.HasPrefix(src, "${{") {
		body, after, ok := model.SplitExprBody(src[3:])
		if !ok {
			return fmt.Errorf("unterminated expression: %q", src)
		}
		if strings.TrimSpace(after) == "" {
			src = body
		} else {
			return Validate(raw)
		}
	}
	_, err := parse(src)
	return err
}
