// Package workflow parses GitHub Actions workflow YAML into the platform's IR.
//
// Its central rule: a key this package does not implement fails the parse with
// an `unsupported:` error naming the exact YAML path and line. Silently
// dropping a key the author wrote -- an `if:`, a `continue-on-error:` -- would
// run something other than the workflow in front of us, which is worse than
// refusing to run at all.
package workflow

import (
	"errors"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

var workflowKeys = []string{
	"name", "description", "run-name", "on", "env", "defaults", "concurrency",
	"permissions", "jobs",
}

// Parse parses one workflow file into the IR. path is repo-relative.
func Parse(path string, src []byte) (*model.Workflow, error) {
	p := &parser{
		path:       path,
		jobNodes:   map[string]*yaml.Node{},
		needsNodes: map[string]*yaml.Node{},
		w: &model.Workflow{
			Path: path,
			Name: path,
			Jobs: map[string]*model.JobIR{},
		},
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, parseYAMLError(path, err)
	}
	if len(doc.Content) == 0 {
		return nil, p.errf(nil, "the workflow file is empty")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, p.errf(root, "a workflow file must be a mapping, found %s", kindName(root))
	}

	normalizeOnKey(root)

	var sawOn, sawJobs bool
	err := p.each(root, "", workflowKeys, func(key string, kn, vn *yaml.Node) error {
		var err error
		switch key {
		case "name":
			p.w.Name, err = p.nonEmpty(vn, "name")
		case "description":
			p.w.Description, err = p.scalar(vn, "description")
		case "run-name":
			p.w.RunName, err = p.expr(vn, "run-name")
		case "on":
			sawOn = true
			p.w.On, err = p.triggers(vn)
		case "env":
			p.w.Env, err = p.exprMap(vn, "env")
		case "defaults":
			p.w.Defaults, err = p.defaults(vn, "defaults")
		case "concurrency":
			p.w.Concurrency, err = p.concurrency(vn, "concurrency")
		case "permissions":
			p.w.Permissions, err = p.permissions(vn, "permissions")
		case "jobs":
			sawJobs = true
			err = p.jobs(vn)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if !sawOn {
		return nil, p.errf(root, "the workflow declares no `on:` trigger, so nothing could ever start it")
	}
	if !sawJobs {
		return nil, p.errf(root, "the workflow declares no `jobs:`")
	}
	if len(p.w.JobOrder) == 0 {
		return nil, p.errf(root, "`jobs:` is empty")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if strings.Contains(string(src), "${{") {
		p.deviate("${{ }}",
			`renders an object or array in an interpolated string as the literal text "Object" or "Array"`,
			"renders it as JSON",
			`GitHub's own rendering discards the value, which makes a mis-typed expression impossible to debug from its output`)
	}
	return p.w, nil
}

// normalizeOnKey undoes YAML 1.1's `on: -> true`. yaml.v3 resolves a bare `on`
// key to the string "on", but a loader that follows YAML 1.1 does not, and a
// file written for one must not parse differently here.
func normalizeOnKey(root *yaml.Node) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		kn := root.Content[i]
		if kn.Kind == yaml.ScalarNode && kn.Tag == "!!bool" && strings.EqualFold(kn.Value, "true") {
			kn.Tag = "!!str"
			kn.Value = "on"
		}
	}
}

// parseYAMLError converts yaml.v3's own error, which carries the line only
// inside its message text, into a ParseError.
func parseYAMLError(path string, err error) error {
	msg := err.Error()
	var te *yaml.TypeError
	if errors.As(err, &te) {
		msg = strings.Join(te.Errors, "; ")
	}
	line := 0
	// yaml.v3 formats syntax errors as "yaml: line N: message".
	if i := strings.Index(msg, "line "); i >= 0 {
		for _, c := range msg[i+len("line "):] {
			if c < '0' || c > '9' {
				break
			}
			line = line*10 + int(c-'0')
		}
	}
	return &ParseError{Path: path, Line: line, Msg: "invalid workflow file: " + msg}
}
