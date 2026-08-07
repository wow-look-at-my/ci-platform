package workflow

import (
	"fmt"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

// Job key allow-lists, from workflow-v1.0.json's `job-factory` and
// `workflow-job` definitions, plus this platform's own `retry`.
var (
	stepsJobKeys = []string{
		"name", "needs", "if", "runs-on", "environment", "concurrency", "outputs",
		"env", "defaults", "steps", "timeout-minutes", "strategy",
		"continue-on-error", "container", "services", "permissions", "retry",
	}
	callJobKeys = []string{
		"name", "uses", "with", "secrets", "needs", "if", "permissions",
		"concurrency", "strategy", "retry",
	}
	// permissionScopes is workflow-v1.0.json's `permissions-mapping`.
	permissionScopes = []string{
		"actions", "artifact-metadata", "attestations", "checks", "contents",
		"deployments", "discussions", "id-token", "issues", "models", "packages",
		"pages", "pull-requests", "repository-projects", "security-events",
		"statuses", "vulnerability-alerts",
	}
	permissionLevels = []string{"read", "write", "none"}
)

func (p *parser) jobs(n *yaml.Node) error {
	p.w.Jobs = map[string]*model.JobIR{}
	return p.each(n, "jobs", nil, func(key string, kn, vn *yaml.Node) error {
		if key == "" {
			return p.errf(kn, "jobs has an empty job id")
		}
		job, err := p.job(key, vn)
		if err != nil {
			return err
		}
		p.w.Jobs[key] = job
		p.w.JobOrder = append(p.w.JobOrder, key)
		p.jobNodes[key] = vn
		return nil
	})
}

func (p *parser) job(key string, n *yaml.Node) (*model.JobIR, error) {
	where := "jobs." + key
	if err := p.check(n, where); err != nil {
		return nil, err
	}
	if n.Kind != yaml.MappingNode {
		return nil, p.errf(n, "%s must be a mapping, found %s", where, kindName(n))
	}

	// A job is either a reusable-workflow call or a list of steps, and each
	// shape has its own key set (workflow-v1.0.json splits them into
	// `workflow-job` and `job-factory`). Catching the mixture here gives a
	// better message than "steps is not a known key" would.
	allowed := stepsJobKeys
	if hasKey(n, "uses") {
		if hasKey(n, "steps") {
			return nil, p.errf(n, "%s declares both `uses:` and `steps:`; a reusable-workflow call has no steps of its own", where)
		}
		allowed = callJobKeys
	}

	j := &model.JobIR{Key: key}
	var sawRunsOn, sawSteps bool
	err := p.each(n, where, allowed, func(k string, kn, vn *yaml.Node) error {
		at := where + "." + k
		var err error
		switch k {
		case "name":
			j.Name, err = p.expr(vn, at)
		case "if":
			j.If, err = p.condition(vn, at)
		case "needs":
			j.Needs, err = p.stringList(vn, at)
			p.needsNodes[key] = vn
		case "runs-on":
			sawRunsOn = true
			j.RunsOn, err = p.runsOn(vn, at)
		case "env":
			j.Env, err = p.exprMap(vn, at)
		case "outputs":
			j.Outputs, err = p.exprMap(vn, at)
		case "defaults":
			j.Defaults, err = p.defaults(vn, at)
		case "timeout-minutes":
			j.TimeoutMinutes, err = p.expr(vn, at)
		case "continue-on-error":
			j.ContinueOnError, err = p.expr(vn, at)
		case "permissions":
			j.Permissions, err = p.permissions(vn, at)
		case "concurrency":
			j.Concurrency, err = p.concurrency(vn, at)
		case "strategy":
			j.Strategy, err = p.strategy(vn, at)
		case "environment":
			j.Environment, err = p.environment(vn, at)
		case "container":
			j.Container, err = p.container(vn, at, false)
		case "services":
			j.Services = map[string]*model.ContainerSpec{}
			err = p.each(vn, at, nil, func(name string, kn, sn *yaml.Node) error {
				c, err := p.container(sn, at+"."+name, true)
				j.Services[name] = c
				return err
			})
		case "steps":
			sawSteps = true
			j.Steps, err = p.steps(vn, at)
		case "uses":
			j.Uses, err = p.nonEmpty(vn, at)
			if err == nil {
				err = p.usesRef(vn, at, j.Uses)
			}
		case "with":
			j.With, err = p.exprMap(vn, at)
		case "secrets":
			j.Secrets, err = p.jobSecrets(vn, at)
		case "retry":
			j.Retry, err = p.retry(vn, at)
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	switch {
	case j.Uses == "" && !sawSteps:
		return nil, p.errf(n, "%s must declare either `steps:` or `uses:`", where)
	case j.Uses == "" && !sawRunsOn:
		return nil, p.errf(n, "%s must declare `runs-on:`", where)
	}
	return j, nil
}

func hasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

// runsOn accepts a label, a list of labels, or a {group, labels} mapping.
func (p *parser) runsOn(n *yaml.Node, where string) (model.RunsOn, error) {
	var r model.RunsOn
	switch n.Kind {
	case yaml.ScalarNode, yaml.SequenceNode:
		labels, err := p.stringList(n, where)
		if err != nil {
			return r, err
		}
		for _, l := range labels {
			r.Labels = append(r.Labels, model.NewExpr(l))
		}
		return r, nil
	}
	err := p.each(n, where, []string{"group", "labels"}, func(key string, kn, vn *yaml.Node) error {
		if key == "group" {
			g, err := p.expr(vn, where+".group")
			r.Group = g
			return err
		}
		labels, err := p.stringList(vn, where+".labels")
		if err != nil {
			return err
		}
		for _, l := range labels {
			r.Labels = append(r.Labels, model.NewExpr(l))
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if r.Group.Empty() && len(r.Labels) == 0 {
		return r, p.errf(n, "%s must set `group:` or `labels:`", where)
	}
	return r, nil
}

func (p *parser) defaults(n *yaml.Node, where string) (model.Defaults, error) {
	var d model.Defaults
	err := p.each(n, where, []string{"run"}, func(key string, kn, vn *yaml.Node) error {
		return p.each(vn, where+".run", []string{"shell", "working-directory"}, func(k string, kn, v *yaml.Node) error {
			var err error
			switch k {
			case "shell":
				d.Shell, err = p.nonEmpty(v, where+".run.shell")
				if err == nil {
					err = p.checkShell(v, where+".run.shell", d.Shell)
				}
			case "working-directory":
				d.WorkingDirectory, err = p.nonEmpty(v, where+".run.working-directory")
			}
			return err
		})
	})
	return d, err
}

func (p *parser) permissions(n *yaml.Node, where string) (*model.Permissions, error) {
	if n.Kind == yaml.ScalarNode {
		s, err := p.nonEmpty(n, where)
		if err != nil {
			return nil, err
		}
		if s != "read-all" && s != "write-all" {
			return nil, p.errf(n, "%s must be `read-all`, `write-all`, or a mapping of scopes, found %q", where, s)
		}
		return &model.Permissions{All: s}, nil
	}
	perms := &model.Permissions{Scopes: map[string]string{}}
	err := p.each(n, where, permissionScopes, func(key string, kn, vn *yaml.Node) error {
		level, err := p.nonEmpty(vn, where+"."+key)
		if err != nil {
			return err
		}
		if !containsStr(permissionLevels, level) {
			return p.errf(vn, "%s.%s must be read, write or none, found %q", where, key, level)
		}
		if key == "id-token" && level == "read" {
			return p.errf(vn, "%s.id-token accepts only write or none", where)
		}
		perms.Scopes[key] = level
		return nil
	})
	return perms, err
}

func (p *parser) concurrency(n *yaml.Node, where string) (*model.Concurrency, error) {
	if n.Kind == yaml.ScalarNode {
		g, err := p.expr(n, where)
		if err != nil {
			return nil, err
		}
		return &model.Concurrency{Group: g}, nil
	}
	c := &model.Concurrency{}
	err := p.each(n, where, []string{"group", "cancel-in-progress"}, func(key string, kn, vn *yaml.Node) error {
		var err error
		if key == "group" {
			c.Group, err = p.expr(vn, where+".group")
		} else {
			c.CancelInProgress, err = p.expr(vn, where+".cancel-in-progress")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if c.Group.Empty() {
		return nil, p.errf(n, "%s must set `group:`", where)
	}
	return c, nil
}

func (p *parser) environment(n *yaml.Node, where string) (*model.Environment, error) {
	if n.Kind == yaml.ScalarNode {
		name, err := p.expr(n, where)
		if err != nil {
			return nil, err
		}
		return &model.Environment{Name: name}, nil
	}
	e := &model.Environment{}
	err := p.each(n, where, []string{"name", "url"}, func(key string, kn, vn *yaml.Node) error {
		var err error
		if key == "name" {
			e.Name, err = p.expr(vn, where+".name")
		} else {
			e.URL, err = p.expr(vn, where+".url")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if e.Name.Empty() {
		return nil, p.errf(n, "%s must set `name:`", where)
	}
	return e, nil
}

func (p *parser) container(n *yaml.Node, where string, service bool) (*model.ContainerSpec, error) {
	if n.Kind == yaml.ScalarNode {
		img, err := p.expr(n, where)
		if err != nil {
			return nil, err
		}
		return &model.ContainerSpec{Image: img}, nil
	}
	allowed := []string{"image", "options", "env", "ports", "volumes", "credentials"}
	if service {
		// `entrypoint` and `command` exist only on service containers, and the
		// IR has nowhere to put them.
		allowed = append(allowed, "entrypoint", "command")
	}
	c := &model.ContainerSpec{}
	err := p.each(n, where, allowed, func(key string, kn, vn *yaml.Node) error {
		at := where + "." + key
		var err error
		switch key {
		case "image":
			c.Image, err = p.expr(vn, at)
		case "options":
			c.Options, err = p.expr(vn, at)
		case "env":
			c.Env, err = p.exprMap(vn, at)
		case "credentials":
			c.Credentials, err = p.exprMap(vn, at)
		case "ports":
			c.Ports, err = p.exprList(vn, at)
		case "volumes":
			c.Volumes, err = p.exprList(vn, at)
		case "entrypoint", "command":
			return p.errf(vn, "unsupported: %s is not implemented", at)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if c.Image.Empty() {
		return nil, p.errf(n, "%s must set `image:`", where)
	}
	return c, nil
}

func (p *parser) exprList(n *yaml.Node, where string) ([]model.Expr, error) {
	list, err := p.stringList(n, where)
	if err != nil {
		return nil, err
	}
	out := make([]model.Expr, len(list))
	for i, s := range list {
		out[i] = model.NewExpr(s)
	}
	return out, nil
}

func (p *parser) jobSecrets(n *yaml.Node, where string) (*model.JobSecrets, error) {
	if n.Kind == yaml.ScalarNode {
		s, err := p.nonEmpty(n, where)
		if err != nil {
			return nil, err
		}
		if s != "inherit" {
			return nil, p.errf(n, "%s must be `inherit` or a mapping, found %q", where, s)
		}
		return &model.JobSecrets{Inherit: true}, nil
	}
	vals, err := p.exprMap(n, where)
	if err != nil {
		return nil, err
	}
	return &model.JobSecrets{Values: vals}, nil
}

// usesRef validates an action or reusable-workflow reference. An unpinned
// `owner/repo` is rejected: it resolves to whatever the default branch happens
// to be at dispatch time.
func (p *parser) usesRef(n *yaml.Node, where, ref string) error {
	switch {
	case strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../"):
		if strings.Contains(ref, "@") {
			return p.errf(n, "%s %q is a local path and must not carry an @ref", where, ref)
		}
		return nil
	case strings.HasPrefix(ref, "docker://"):
		if strings.TrimPrefix(ref, "docker://") == "" {
			return p.errf(n, "%s %q names no image", where, ref)
		}
		return nil
	}
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return p.errf(n, "%s %q must be pinned to a ref (owner/repo@ref)", where, ref)
	}
	repo, rev := ref[:at], ref[at+1:]
	if rev == "" {
		return p.errf(n, "%s %q has an empty ref after '@'", where, ref)
	}
	parts := strings.Split(repo, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return p.errf(n, "%s %q must be owner/repo@ref, owner/repo/path@ref, ./local/path, or docker://image", where, ref)
	}
	for _, seg := range parts[2:] {
		if seg == "" {
			return p.errf(n, "%s %q has an empty path segment", where, ref)
		}
	}
	return nil
}

func (p *parser) steps(n *yaml.Node, where string) ([]*model.StepIR, error) {
	var out []*model.StepIR
	ids := map[string]bool{}
	err := p.seq(n, where, func(i int, item *yaml.Node) error {
		s, err := p.step(item, fmt.Sprintf("%s[%d]", where, i), i+1)
		if err != nil {
			return err
		}
		if s.ID != "" {
			if ids[s.ID] {
				return p.errf(item, "%s[%d].id %q is already used by an earlier step", where, i, s.ID)
			}
			ids[s.ID] = true
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, p.errf(n, "%s is empty; a job with no steps does no work", where)
	}
	return out, nil
}
