package workflow

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

// knownEvents is GitHub's own event list, taken from the `on-mapping-strict`
// definition in actions-runner's workflow-v1.0.json. An event outside it is a
// typo, not a feature we are missing.
var knownEvents = []string{
	"branch_protection_rule", "check_run", "check_suite", "create", "delete",
	"deployment", "deployment_status", "discussion", "discussion_comment",
	"fork", "gollum", "image_version", "issue_comment", "issues", "label",
	"merge_group", "milestone", "page_build", "project", "project_card",
	"project_column", "public", "pull_request", "pull_request_comment",
	"pull_request_review", "pull_request_review_comment", "pull_request_target",
	"push", "registry_package", "release", "repository_dispatch", "schedule",
	"status", "watch", "workflow_call", "workflow_dispatch", "workflow_run",
}

var (
	pushKeys        = []string{"branches", "branches-ignore", "tags", "tags-ignore", "paths", "paths-ignore"}
	pullRequestKeys = []string{"types", "branches", "branches-ignore", "paths", "paths-ignore"}
	dispatchKeys    = []string{"inputs"}
	callKeys        = []string{"inputs", "secrets", "outputs"}
	inputKeys       = []string{"description", "type", "required", "default", "options"}
	callSecretKeys  = []string{"description", "required"}
	callOutputKeys  = []string{"description", "value"}
	inputTypes      = []string{"string", "boolean", "number", "choice", "environment"}
)

// triggers parses `on:` in all three shapes: a bare event name, a list of
// names, or the full per-event mapping.
func (p *parser) triggers(n *yaml.Node) (model.Triggers, error) {
	var t model.Triggers
	switch n.Kind {
	case yaml.ScalarNode, yaml.SequenceNode:
		names, err := p.stringList(n, "on")
		if err != nil {
			return t, err
		}
		for _, name := range names {
			if err := p.bareEvent(&t, name, n); err != nil {
				return t, err
			}
		}
		return t, nil
	}
	err := p.each(n, "on", knownEvents, func(key string, kn, vn *yaml.Node) error {
		return p.event(&t, key, kn, vn)
	})
	return t, err
}

func (p *parser) bareEvent(t *model.Triggers, name string, n *yaml.Node) error {
	switch name {
	case "push":
		t.Push = &model.BranchFilter{}
	case "pull_request":
		t.PullRequest = &model.PullRequestFilter{}
	case "workflow_dispatch":
		t.WorkflowDispatch = &model.WorkflowDispatch{}
	case "workflow_call":
		t.WorkflowCall = &model.WorkflowCall{}
	case "schedule":
		return p.errf(n, "on.schedule requires at least one cron entry")
	default:
		if !isKnownEvent(name) {
			return p.errf(n, "unsupported: on.%s is not a known GitHub event", name)
		}
		p.otherEvent(t, name, nil)
	}
	return nil
}

func (p *parser) event(t *model.Triggers, name string, kn, vn *yaml.Node) error {
	where := "on." + name
	switch name {
	case "push":
		f, err := p.branchFilter(vn, where, pushKeys)
		if err != nil {
			return err
		}
		t.Push = f
		return nil
	case "pull_request", "pull_request_target":
		f := &model.PullRequestFilter{}
		if !isNull(vn) {
			err := p.each(vn, where, pullRequestKeys, func(key string, kn, vn *yaml.Node) error {
				if key == "types" {
					types, err := p.stringList(vn, where+".types")
					f.Types = types
					return err
				}
				return p.branchFilterKey(&f.BranchFilter, key, vn, where)
			})
			if err != nil {
				return err
			}
		}
		if name == "pull_request" {
			t.PullRequest = f
			return nil
		}
		// pull_request_target is accepted but not given PR-filter semantics.
		p.otherEvent(t, name, f.Types)
		return nil
	case "schedule":
		return p.schedule(t, vn, where)
	case "workflow_dispatch":
		return p.workflowDispatch(t, vn, where)
	case "workflow_call":
		return p.workflowCall(t, vn, where)
	}

	// Every other event is accepted by name and by activity type only.
	var types []string
	if !isNull(vn) {
		err := p.each(vn, where, []string{"types"}, func(key string, kn, vn *yaml.Node) error {
			var err error
			types, err = p.stringList(vn, where+".types")
			return err
		})
		if err != nil {
			return err
		}
	}
	p.otherEvent(t, name, types)
	return nil
}

func (p *parser) otherEvent(t *model.Triggers, name string, types []string) {
	if t.Other == nil {
		t.Other = map[string]model.RawEvents{}
	}
	t.Other[name] = model.RawEvents{Types: types}
	p.deviate("on."+name,
		"filters the event by its own branch, path and payload rules",
		"matches the event name and its `types:` only",
		"only push and pull_request carry filter semantics in this platform, so a narrower filter on this event would be ignored rather than applied")
}

func (p *parser) branchFilter(n *yaml.Node, where string, allowed []string) (*model.BranchFilter, error) {
	f := &model.BranchFilter{}
	if isNull(n) {
		return f, nil
	}
	err := p.each(n, where, allowed, func(key string, kn, vn *yaml.Node) error {
		return p.branchFilterKey(f, key, vn, where)
	})
	return f, err
}

func (p *parser) branchFilterKey(f *model.BranchFilter, key string, vn *yaml.Node, where string) error {
	list, err := p.stringList(vn, where+"."+key)
	if err != nil {
		return err
	}
	switch key {
	case "branches":
		f.Branches = list
	case "branches-ignore":
		f.BranchesIgnore = list
	case "tags":
		f.Tags = list
	case "tags-ignore":
		f.TagsIgnore = list
	case "paths":
		f.Paths = list
	case "paths-ignore":
		f.PathsIgnore = list
	default:
		return p.errf(vn, "unsupported: %s.%s is not a known filter", where, key)
	}
	return nil
}

// cronParser is GitHub's dialect: five fields, no seconds, no @descriptors.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func (p *parser) schedule(t *model.Triggers, n *yaml.Node, where string) error {
	return p.seq(n, where, func(i int, item *yaml.Node) error {
		at := fmt.Sprintf("%s[%d]", where, i)
		var spec string
		err := p.each(item, at, []string{"cron"}, func(key string, kn, vn *yaml.Node) error {
			var err error
			spec, err = p.nonEmpty(vn, at+".cron")
			return err
		})
		if err != nil {
			return err
		}
		if spec == "" {
			return p.errf(item, "%s must set `cron:`", at)
		}
		if strings.HasPrefix(strings.TrimSpace(spec), "@") {
			return p.errf(item, "unsupported: %s.cron %q uses a descriptor; GitHub accepts only five-field POSIX cron", at, spec)
		}
		if _, err := cronParser.Parse(spec); err != nil {
			return p.errf(item, "%s.cron %q is not a valid five-field cron expression: %v", at, spec, err)
		}
		t.Schedule = append(t.Schedule, model.ScheduleTrigger{Cron: spec})
		return nil
	})
}

func (p *parser) workflowDispatch(t *model.Triggers, n *yaml.Node, where string) error {
	d := &model.WorkflowDispatch{}
	if isNull(n) {
		t.WorkflowDispatch = d
		return nil
	}
	err := p.each(n, where, dispatchKeys, func(key string, kn, vn *yaml.Node) error {
		inputs, order, err := p.inputs(vn, where+".inputs", true)
		if err != nil {
			return err
		}
		d.Inputs, d.Order = inputs, order
		return nil
	})
	if err != nil {
		return err
	}
	t.WorkflowDispatch = d
	return nil
}

func (p *parser) workflowCall(t *model.Triggers, n *yaml.Node, where string) error {
	c := &model.WorkflowCall{}
	if isNull(n) {
		t.WorkflowCall = c
		return nil
	}
	err := p.each(n, where, callKeys, func(key string, kn, vn *yaml.Node) error {
		switch key {
		case "inputs":
			inputs, _, err := p.inputs(vn, where+".inputs", false)
			c.Inputs = inputs
			return err
		case "secrets":
			c.Secrets = map[string]*model.CallSecret{}
			return p.each(vn, where+".secrets", nil, func(name string, kn, sn *yaml.Node) error {
				s := &model.CallSecret{}
				at := where + ".secrets." + name
				err := p.each(sn, at, callSecretKeys, func(k string, kn, v *yaml.Node) error {
					var err error
					switch k {
					case "description":
						s.Description, err = p.scalar(v, at+".description")
					case "required":
						s.Required, err = p.boolean(v, at+".required")
					}
					return err
				})
				c.Secrets[name] = s
				return err
			})
		case "outputs":
			c.Outputs = map[string]*model.CallOutput{}
			return p.each(vn, where+".outputs", nil, func(name string, kn, on *yaml.Node) error {
				o := &model.CallOutput{}
				at := where + ".outputs." + name
				err := p.each(on, at, callOutputKeys, func(k string, kn, v *yaml.Node) error {
					var err error
					switch k {
					case "description":
						o.Description, err = p.scalar(v, at+".description")
					case "value":
						o.Value, err = p.expr(v, at+".value")
					}
					return err
				})
				if err != nil {
					return err
				}
				if o.Value.Empty() {
					return p.errf(on, "%s must set `value:`", at)
				}
				c.Outputs[name] = o
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.WorkflowCall = c
	return nil
}

// inputs parses a workflow_dispatch or workflow_call input block. Only
// workflow_dispatch supports the `choice` type and its `options`.
func (p *parser) inputs(n *yaml.Node, where string, allowChoice bool) (map[string]*model.DispatchInput, []string, error) {
	out := map[string]*model.DispatchInput{}
	var order []string
	err := p.each(n, where, nil, func(name string, kn, vn *yaml.Node) error {
		in := &model.DispatchInput{}
		at := where + "." + name
		err := p.each(vn, at, inputKeys, func(key string, kn, v *yaml.Node) error {
			var err error
			switch key {
			case "description":
				in.Description, err = p.scalar(v, at+".description")
			case "required":
				in.Required, err = p.boolean(v, at+".required")
			case "default":
				in.Default, err = p.scalar(v, at+".default")
			case "type":
				in.Type, err = p.scalar(v, at+".type")
				if err == nil && !containsStr(inputTypes, in.Type) {
					return p.errf(v, "%s.type must be one of %s, found %q", at, strings.Join(inputTypes, ", "), in.Type)
				}
				if err == nil && in.Type == "choice" && !allowChoice {
					return p.errf(v, "%s.type cannot be `choice`; only workflow_dispatch inputs can", at)
				}
			case "options":
				if !allowChoice {
					return p.errf(v, "unsupported: %s.options is only valid on a workflow_dispatch input", at)
				}
				in.Options, err = p.stringList(v, at+".options")
			}
			return err
		})
		if err != nil {
			return err
		}
		if in.Type == "choice" && len(in.Options) == 0 {
			return p.errf(vn, "%s is a choice input and must list `options:`", at)
		}
		if in.Type != "" && in.Type != "choice" && len(in.Options) > 0 {
			return p.errf(vn, "%s sets `options:` but its type is %q, not choice", at, in.Type)
		}
		out[name] = in
		order = append(order, name)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, order, nil
}

func isKnownEvent(name string) bool { return containsStr(knownEvents, name) }

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
