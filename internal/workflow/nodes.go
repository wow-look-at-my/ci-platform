package workflow

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
	"gopkg.in/yaml.v3"
)

// ParseError carries the YAML line/column so the UI can point at the mistake.
type ParseError struct {
	Path      string
	Line, Col int
	Msg       string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.Path, e.Line, e.Col, e.Msg)
}

type parser struct {
	path string
	w    *model.Workflow
	// jobNodes and needsNodes let the cross-job validation pass report a line
	// for a cycle or a dangling `needs:`, long after the job itself was parsed.
	jobNodes   map[string]*yaml.Node
	needsNodes map[string]*yaml.Node
}

func (p *parser) errf(n *yaml.Node, format string, a ...any) error {
	e := &ParseError{Path: p.path, Msg: fmt.Sprintf(format, a...)}
	if n != nil {
		e.Line, e.Col = n.Line, n.Column
	}
	return e
}

func (p *parser) deviate(path, gha, ours, why string) {
	p.w.Deviations = append(p.w.Deviations, model.Deviation{
		Path: path, GHABehavior: gha, OurBehavior: ours, Rationale: why,
	})
}

// join builds a YAML path, tolerating the empty top-level prefix.
func join(where, key string) string {
	if where == "" {
		return key
	}
	return where + "." + key
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return "null"
		}
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	}
	return "an unknown node"
}

func orRoot(where string) string {
	if where == "" {
		return "the workflow"
	}
	return where
}

func isNull(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null"
}

// check rejects YAML features GitHub Actions itself does not accept, so a file
// that would behave differently there fails here rather than silently working.
func (p *parser) check(n *yaml.Node, where string) error {
	if n.Kind == yaml.AliasNode {
		return p.errf(n, "unsupported: %s uses a YAML alias, which GitHub Actions does not support", where)
	}
	return nil
}

// each iterates a mapping, rejecting duplicate keys and any key outside the
// allow-list. The allow-list is the whole point: a key we do not implement must
// fail the run, never be dropped.
func (p *parser) each(n *yaml.Node, where string, allowed []string, fn func(key string, kn, vn *yaml.Node) error) error {
	if err := p.check(n, where); err != nil {
		return err
	}
	if n.Kind != yaml.MappingNode {
		return p.errf(n, "%s must be a mapping, found %s", orRoot(where), kindName(n))
	}
	seen := make(map[string]bool, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		if kn.Kind != yaml.ScalarNode {
			return p.errf(kn, "%s has a %s where a key name was expected", where, kindName(kn))
		}
		key := kn.Value
		if seen[key] {
			return p.errf(kn, "%s is declared twice; GitHub Actions would silently keep the last one", join(where, key))
		}
		seen[key] = true
		// The alias check comes first so a merge key (`<<: *anchor`) is
		// reported as the alias it is rather than as an unknown key.
		if err := p.check(vn, join(where, key)); err != nil {
			return err
		}
		if allowed != nil && !slices.Contains(allowed, key) {
			return p.errf(kn, "unsupported: %s is not a known key (known keys: %s)", join(where, key), strings.Join(allowed, ", "))
		}
		if err := fn(key, kn, vn); err != nil {
			return err
		}
	}
	return nil
}

// seq iterates a sequence.
func (p *parser) seq(n *yaml.Node, where string, fn func(i int, item *yaml.Node) error) error {
	if err := p.check(n, where); err != nil {
		return err
	}
	if n.Kind != yaml.SequenceNode {
		return p.errf(n, "%s must be a sequence, found %s", where, kindName(n))
	}
	for i, item := range n.Content {
		if err := p.check(item, fmt.Sprintf("%s[%d]", where, i)); err != nil {
			return err
		}
		if err := fn(i, item); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) scalar(n *yaml.Node, where string) (string, error) {
	if err := p.check(n, where); err != nil {
		return "", err
	}
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return "", p.errf(n, "%s must be a scalar value, found %s", where, kindName(n))
	}
	return n.Value, nil
}

func (p *parser) nonEmpty(n *yaml.Node, where string) (string, error) {
	s, err := p.scalar(n, where)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", p.errf(n, "%s must not be empty", where)
	}
	return s, nil
}

// expr keeps the text unevaluated but checks the SYNTAX of every ${{ }} body,
// so a malformed expression is a config error now rather than a failure
// halfway through a run.
func (p *parser) expr(n *yaml.Node, where string) (model.Expr, error) {
	s, err := p.scalar(n, where)
	if err != nil {
		return model.Expr{}, err
	}
	if err := expr.Validate(s); err != nil {
		return model.Expr{}, p.errf(n, "%s: %v", where, err)
	}
	return model.NewExpr(s), nil
}

// condition is `if:`, where a bare value is an expression body rather than a
// template.
func (p *parser) condition(n *yaml.Node, where string) (model.Expr, error) {
	s, err := p.scalar(n, where)
	if err != nil {
		return model.Expr{}, err
	}
	if err := expr.ValidateCondition(s); err != nil {
		return model.Expr{}, p.errf(n, "%s: %v", where, err)
	}
	return model.NewExpr(s), nil
}

// stringList accepts both the scalar and the sequence shorthand, which GHA does
// in every place it takes a list.
func (p *parser) stringList(n *yaml.Node, where string) ([]string, error) {
	if n.Kind == yaml.ScalarNode {
		s, err := p.nonEmpty(n, where)
		if err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
	var out []string
	err := p.seq(n, where, func(i int, item *yaml.Node) error {
		s, err := p.nonEmpty(item, fmt.Sprintf("%s[%d]", where, i))
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

func (p *parser) exprMap(n *yaml.Node, where string) (map[string]model.Expr, error) {
	out := map[string]model.Expr{}
	err := p.each(n, where, nil, func(key string, kn, vn *yaml.Node) error {
		v, err := p.expr(vn, join(where, key))
		if err != nil {
			return err
		}
		out[key] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) boolean(n *yaml.Node, where string) (bool, error) {
	s, err := p.scalar(n, where)
	if err != nil {
		return false, err
	}
	// A typo like `ture` must fail rather than quietly reading as false.
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, p.errf(n, "%s must be true or false, found %q", where, s)
	}
	return b, nil
}

func (p *parser) integer(n *yaml.Node, where string) (int, error) {
	s, err := p.scalar(n, where)
	if err != nil {
		return 0, err
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, p.errf(n, "%s must be a whole number, found %q", where, s)
	}
	return i, nil
}

// value decodes a scalar/sequence/mapping into plain Go data for the places the
// IR stores `any` (matrix dimensions and include/exclude entries).
func (p *parser) value(n *yaml.Node, where string) (any, error) {
	if err := p.check(n, where); err != nil {
		return nil, err
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, p.errf(n, "%s could not be read: %v", where, err)
	}
	return v, nil
}
