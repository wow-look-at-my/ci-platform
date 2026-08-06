package workflow

import (
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// validate runs the checks that need the whole file: the needs DAG, and the
// matrix cross-references.
func (p *parser) validate() error {
	for _, key := range p.w.JobOrder {
		j := p.w.Jobs[key]
		for _, need := range j.Needs {
			if _, ok := p.w.Jobs[need]; !ok {
				return p.errf(p.needsNodes[key], "jobs.%s.needs refers to %q, which is not a job in this workflow (jobs: %s)",
					key, need, strings.Join(p.w.JobOrder, ", "))
			}
			if need == key {
				return p.errf(p.needsNodes[key], "jobs.%s.needs contains itself", key)
			}
		}
		if seen := duplicates(j.Needs); seen != "" {
			return p.errf(p.needsNodes[key], "jobs.%s.needs lists %q twice", key, seen)
		}
	}
	if cycle := findCycle(p.w); len(cycle) > 0 {
		head := cycle[0]
		return p.errf(p.needsNodes[head], "jobs.%s.needs forms a cycle: %s", head, strings.Join(cycle, " -> "))
	}
	return nil
}

func duplicates(ss []string) string {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return s
		}
		seen[s] = true
	}
	return ""
}

// findCycle returns the jobs on one cycle in the needs DAG, closed (the first
// element repeated at the end) so the message reads a -> b -> a. Jobs are
// visited in declaration order, so the reported cycle is stable.
func findCycle(w *model.Workflow) []string {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(w.Jobs))
	var stack []string

	var walk func(key string) []string
	walk = func(key string) []string {
		state[key] = onStack
		stack = append(stack, key)
		for _, need := range w.Jobs[key].Needs {
			next, ok := w.Jobs[need]
			if !ok {
				continue
			}
			switch state[need] {
			case onStack:
				at := 0
				for i, k := range stack {
					if k == need {
						at = i
						break
					}
				}
				return append(append([]string{}, stack[at:]...), next.Key)
			case unvisited:
				if c := walk(need); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = done
		return nil
	}

	for _, key := range w.JobOrder {
		if state[key] == unvisited {
			if c := walk(key); c != nil {
				return c
			}
		}
	}
	return nil
}

// Unsupport is one feature present in the file that this platform parses but
// does not execute.
type Unsupport struct{ Path, Feature, Detail string }

func (u Unsupport) String() string {
	return "unsupported: " + u.Path + " " + u.Feature + " is not implemented (" + u.Detail + ")"
}

// Unsupported lists features present in the file that this platform does not
// yet execute. A non-empty result must fail the run, not be ignored.
func Unsupported(w *model.Workflow) []Unsupport {
	if w == nil {
		return nil
	}
	var out []Unsupport
	if w.On.WorkflowCall != nil {
		out = append(out, Unsupport{
			Path:    "on.workflow_call",
			Feature: "reusable workflow definitions",
			Detail:  "this workflow declares a callable interface, and nothing here can call it",
		})
	}
	for _, key := range w.JobOrder {
		j := w.Jobs[key]
		if j == nil {
			continue
		}
		at := "jobs." + key
		if j.Uses != "" {
			out = append(out, Unsupport{
				Path:    at + ".uses",
				Feature: "reusable workflow calls",
				Detail:  j.Uses,
			})
		}
		if j.Container != nil {
			out = append(out, Unsupport{
				Path:    at + ".container",
				Feature: "job containers",
				Detail:  "steps would have to run inside " + j.Container.Image.Raw,
			})
		}
		for _, name := range sortedNames(j.Services) {
			out = append(out, Unsupport{
				Path:    at + ".services." + name,
				Feature: "service containers",
				Detail:  j.Services[name].Image.Raw,
			})
		}
		if j.Environment != nil {
			out = append(out, Unsupport{
				Path:    at + ".environment",
				Feature: "deployment environments",
				Detail:  "protection rules and environment secrets for " + j.Environment.Name.Raw,
			})
		}
	}
	return out
}

func sortedNames(m map[string]*model.ContainerSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
