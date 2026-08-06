package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

func (p *parser) strategy(n *yaml.Node, where string) (*model.Strategy, error) {
	s := &model.Strategy{}
	err := p.each(n, where, []string{"matrix", "fail-fast", "max-parallel"}, func(key string, kn, vn *yaml.Node) error {
		at := where + "." + key
		switch key {
		case "matrix":
			m, err := p.matrix(vn, at)
			s.Matrix = m
			return err
		case "fail-fast":
			b, err := p.boolean(vn, at)
			if err != nil {
				return err
			}
			s.FailFast = &b
			return nil
		case "max-parallel":
			mp, err := p.expr(vn, at)
			if err != nil {
				return err
			}
			// A literal must still be a positive number; only an expression can
			// wait until plan time to be checked.
			if mp.IsLiteral() {
				v, err := p.integer(vn, at)
				if err != nil {
					return err
				}
				if v < 1 {
					return p.errf(vn, "%s must be at least 1, found %d", at, v)
				}
			}
			s.MaxParallel = mp
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.Matrix == nil && s.MaxParallel.Empty() && s.FailFast == nil {
		return nil, p.errf(n, "%s is empty", where)
	}
	return s, nil
}

// matrix accepts the whole block as a single ${{ fromJSON(...) }} expression,
// or a mapping of dimensions plus include/exclude.
//
// A dimension's values are stored as plain data. A string value that contains
// ${{ }} is kept raw and evaluated at plan time, once `needs` outputs exist.
func (p *parser) matrix(n *yaml.Node, where string) (*model.Matrix, error) {
	if n.Kind == yaml.ScalarNode {
		e, err := p.expr(n, where)
		if err != nil {
			return nil, err
		}
		if e.IsLiteral() {
			return nil, p.errf(n, "%s must be a mapping or a ${{ }} expression, found the literal %q", where, e.Raw)
		}
		return &model.Matrix{FromExpr: e}, nil
	}

	m := &model.Matrix{Dimensions: map[string][]any{}}
	var excludeNodes []*yaml.Node
	err := p.each(n, where, nil, func(key string, kn, vn *yaml.Node) error {
		at := where + "." + key
		switch key {
		case "include", "exclude":
			rows, nodes, err := p.matrixRows(vn, at)
			if err != nil {
				return err
			}
			if key == "include" {
				m.Include = rows
			} else {
				m.Exclude, excludeNodes = rows, nodes
			}
			return nil
		}
		vals, err := p.matrixDimension(vn, at)
		if err != nil {
			return err
		}
		m.Dimensions[key] = vals
		m.Order = append(m.Order, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(m.Dimensions) == 0 && len(m.Include) == 0 {
		return nil, p.errf(n, "%s has no dimensions and no `include:`", where)
	}
	if err := p.checkExcludes(m, excludeNodes, where); err != nil {
		return nil, err
	}
	return m, nil
}

func (p *parser) matrixDimension(n *yaml.Node, where string) ([]any, error) {
	if n.Kind == yaml.ScalarNode {
		v, err := p.value(n, where)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, p.errf(n, "%s has no value", where)
		}
		return []any{v}, nil
	}
	var out []any
	err := p.seq(n, where, func(i int, item *yaml.Node) error {
		v, err := p.value(item, fmt.Sprintf("%s[%d]", where, i))
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, p.errf(n, "%s is an empty list, which would expand to zero legs", where)
	}
	return out, nil
}

// matrixRows parses include/exclude: a sequence of flat mappings. A nested
// mapping or sequence inside a row is rejected because leg matching compares
// values for equality.
func (p *parser) matrixRows(n *yaml.Node, where string) ([]map[string]any, []*yaml.Node, error) {
	var out []map[string]any
	var nodes []*yaml.Node
	err := p.seq(n, where, func(i int, item *yaml.Node) error {
		at := fmt.Sprintf("%s[%d]", where, i)
		if item.Kind != yaml.MappingNode {
			return p.errf(item, "%s must be a mapping of matrix keys to values, found %s", at, kindName(item))
		}
		row := map[string]any{}
		err := p.each(item, at, nil, func(key string, kn, vn *yaml.Node) error {
			if vn.Kind != yaml.ScalarNode {
				return p.errf(vn, "%s.%s must be a scalar, found %s", at, key, kindName(vn))
			}
			v, err := p.value(vn, at+"."+key)
			if err != nil {
				return err
			}
			row[key] = v
			return nil
		})
		if err != nil {
			return err
		}
		if len(row) == 0 {
			return p.errf(item, "%s is empty", at)
		}
		out = append(out, row)
		nodes = append(nodes, item)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, p.errf(n, "%s is an empty list", where)
	}
	return out, nodes, nil
}

// checkExcludes rejects an `exclude:` naming a key the matrix does not have,
// which silently excludes nothing on GHA.
func (p *parser) checkExcludes(m *model.Matrix, nodes []*yaml.Node, where string) error {
	if m == nil || !m.FromExpr.Empty() {
		return nil
	}
	known := map[string]bool{}
	for k := range m.Dimensions {
		known[k] = true
	}
	for _, row := range m.Include {
		for k := range row {
			known[k] = true
		}
	}
	for i, row := range m.Exclude {
		for k := range row {
			if !known[k] {
				keys := make([]string, 0, len(known))
				for k := range known {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return p.errf(nodes[i], "%s.exclude[%d] names %q, which is not a matrix key (known keys: %s)",
					where, i, k, strings.Join(keys, ", "))
			}
		}
	}
	return nil
}
