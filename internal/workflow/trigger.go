package workflow

import (
	"fmt"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Event is what actually happened, as far as trigger matching is concerned.
type Event struct {
	// Name is the webhook event: "push", "pull_request", "workflow_dispatch",
	// "schedule", or any other event name.
	Name string
	// Ref is the full ref, e.g. "refs/heads/main" or "refs/tags/v1.2.3". For a
	// pull_request it is the BASE ref, which is what branches: filters against.
	Ref string
	// Action is the webhook's activity type, e.g. "opened" or "synchronize".
	Action string
	// ChangedPaths are the repo-relative paths the event touched. An empty
	// slice with a paths filter present means the filter cannot be satisfied.
	ChangedPaths []string
}

// Decision is why a workflow did or did not trigger. The reason is recorded
// rather than inferred, so "why didn't my workflow run?" has an answer.
type Decision struct {
	Match bool
	// Reason is a complete sentence naming the filter that decided it.
	Reason string
}

// Matches reports whether a workflow triggers for an event, and why.
//
// A filter that cannot be compiled is an error rather than a pattern that
// quietly matches nothing: a typo in `branches:` must not silently stop a
// workflow from ever running again.
func Matches(w *model.Workflow, e Event) (Decision, error) {
	if w == nil {
		return Decision{}, fmt.Errorf("workflow is nil")
	}
	switch e.Name {
	case "push":
		if w.On.Push == nil {
			return Decision{Reason: "the workflow does not listen for push events"}, nil
		}
		return matchBranchFilter(w.On.Push, e, "push")

	case "pull_request":
		if w.On.PullRequest == nil {
			return Decision{Reason: "the workflow does not listen for pull_request events"}, nil
		}
		if len(w.On.PullRequest.Types) > 0 && e.Action != "" &&
			!containsString(w.On.PullRequest.Types, e.Action) {
			return Decision{Reason: fmt.Sprintf(
				"pull_request activity type %q is not in the workflow's types filter", e.Action)}, nil
		}
		return matchBranchFilter(&w.On.PullRequest.BranchFilter, e, "pull_request")

	case "workflow_dispatch":
		if w.On.WorkflowDispatch == nil {
			return Decision{Reason: "the workflow is not manually dispatchable"}, nil
		}
		return Decision{Match: true, Reason: "manually dispatched"}, nil

	case "schedule":
		if len(w.On.Schedule) == 0 {
			return Decision{Reason: "the workflow has no schedule"}, nil
		}
		return Decision{Match: true, Reason: "a schedule entry is due"}, nil

	default:
		ev, ok := w.On.Other[e.Name]
		if !ok {
			return Decision{Reason: fmt.Sprintf("the workflow does not listen for %s events", e.Name)}, nil
		}
		if len(ev.Types) > 0 && e.Action != "" && !containsString(ev.Types, e.Action) {
			return Decision{Reason: fmt.Sprintf(
				"%s activity type %q is not in the workflow's types filter", e.Name, e.Action)}, nil
		}
		return Decision{Match: true, Reason: fmt.Sprintf("the workflow listens for %s events", e.Name)}, nil
	}
}

func matchBranchFilter(f *model.BranchFilter, e Event, event string) (Decision, error) {
	isTag := strings.HasPrefix(e.Ref, "refs/tags/")
	short := shortRef(e.Ref)

	// A ref filter that names only the other kind of ref excludes this one
	// outright: a workflow filtered to tags does not run on a branch push.
	if isTag {
		if len(f.Branches) > 0 || len(f.BranchesIgnore) > 0 {
			if len(f.Tags) == 0 && len(f.TagsIgnore) == 0 {
				return Decision{Reason: fmt.Sprintf(
					"%s filters on branches, and %s is a tag", event, e.Ref)}, nil
			}
		}
		if d, err := applyRefFilter(f.Tags, f.TagsIgnore, short, "tags", e.Ref); err != nil || !d.Match {
			return d, err
		}
	} else {
		if len(f.Tags) > 0 || len(f.TagsIgnore) > 0 {
			if len(f.Branches) == 0 && len(f.BranchesIgnore) == 0 {
				return Decision{Reason: fmt.Sprintf(
					"%s filters on tags, and %s is a branch", event, e.Ref)}, nil
			}
		}
		if d, err := applyRefFilter(f.Branches, f.BranchesIgnore, short, "branches", e.Ref); err != nil || !d.Match {
			return d, err
		}
	}

	return applyPathFilter(f, e, event)
}

func applyRefFilter(include, ignore []string, short, kind, ref string) (Decision, error) {
	if len(include) > 0 {
		set, err := CompileGlobs(include)
		if err != nil {
			return Decision{}, fmt.Errorf("on.%s: %w", kind, err)
		}
		if !set.Matches(short) {
			return Decision{Reason: fmt.Sprintf("%s does not match the %s filter", ref, kind)}, nil
		}
		return Decision{Match: true, Reason: fmt.Sprintf("%s matches the %s filter", ref, kind)}, nil
	}
	if len(ignore) > 0 {
		set, err := CompileGlobs(ignore)
		if err != nil {
			return Decision{}, fmt.Errorf("on.%s-ignore: %w", kind, err)
		}
		// Every pattern here is an exclusion, whether or not it carries a `!`.
		for _, g := range set {
			if g.Match(short) != g.Negated() {
				return Decision{Reason: fmt.Sprintf("%s is excluded by the %s-ignore filter", ref, kind)}, nil
			}
		}
	}
	return Decision{Match: true, Reason: fmt.Sprintf("%s is not excluded", ref)}, nil
}

func applyPathFilter(f *model.BranchFilter, e Event, event string) (Decision, error) {
	if len(f.Paths) > 0 {
		set, err := CompileGlobs(f.Paths)
		if err != nil {
			return Decision{}, fmt.Errorf("on.%s.paths: %w", event, err)
		}
		if !set.MatchesAny(e.ChangedPaths) {
			return Decision{Reason: "no changed file matches the paths filter"}, nil
		}
		return Decision{Match: true, Reason: "a changed file matches the paths filter"}, nil
	}
	if len(f.PathsIgnore) > 0 {
		set, err := CompileGlobs(f.PathsIgnore)
		if err != nil {
			return Decision{}, fmt.Errorf("on.%s.paths-ignore: %w", event, err)
		}
		// The run happens when at least one changed file is NOT ignored.
		for _, p := range e.ChangedPaths {
			ignored := false
			for _, g := range set {
				if g.Match(p) != g.Negated() {
					ignored = true
					break
				}
			}
			if !ignored {
				return Decision{Match: true, Reason: fmt.Sprintf(
					"%s is outside the paths-ignore filter", p)}, nil
			}
		}
		return Decision{Reason: "every changed file is excluded by the paths-ignore filter"}, nil
	}
	return Decision{Match: true, Reason: "no path filter applies"}, nil
}

// shortRef strips refs/heads/ or refs/tags/, because filters are written
// against the branch or tag name rather than the full ref.
func shortRef(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
	}
	return ref
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
