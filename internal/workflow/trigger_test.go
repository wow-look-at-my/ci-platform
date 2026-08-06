package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// GitHub's `?` and `+` quantify the PRECEDING character rather than acting as
// wildcards, which is the part of this dialect that gets guessed wrong.
func TestCompileGlob_QuantifiersApplyToThePrecedingCharacter(t *testing.T) {
	g, err := CompileGlob("releases/v1.?")
	require.NoError(t, err)
	assert.True(t, g.Match("releases/v1."), "zero of the preceding character")
	assert.False(t, g.Match("releases/v1.xy"), "? is not a single-character wildcard")

	plus, err := CompileGlob("v1.0+")
	require.NoError(t, err)
	assert.True(t, plus.Match("v1.0"))
	assert.True(t, plus.Match("v1.000"))
	assert.False(t, plus.Match("v1."))
}

func TestCompileGlob_StarDoesNotCrossASlashButDoubleStarDoes(t *testing.T) {
	single, err := CompileGlob("feature/*")
	require.NoError(t, err)
	assert.True(t, single.Match("feature/login"))
	assert.False(t, single.Match("feature/login/step2"), "* must not match /")

	double, err := CompileGlob("feature/**")
	require.NoError(t, err)
	assert.True(t, double.Match("feature/login/step2"))
}

func TestCompileGlob_ClassesEscapesAndErrors(t *testing.T) {
	class, err := CompileGlob("v[0-9]")
	require.NoError(t, err)
	assert.True(t, class.Match("v3"))
	assert.False(t, class.Match("vx"))

	esc, err := CompileGlob(`weird\*name`)
	require.NoError(t, err)
	assert.True(t, esc.Match("weird*name"))
	assert.False(t, esc.Match("weirdXname"))

	// A malformed pattern is an error, never a pattern that matches nothing:
	// a typo in branches: must not silently stop a workflow from running.
	for _, bad := range []string{"", "?abc", "+abc", "a*?", "[0-9", `trailing\`, "!"} {
		_, err := CompileGlob(bad)
		assert.Error(t, err, bad)
	}
}

func TestGlobSet_LastMatchWins(t *testing.T) {
	set, err := CompileGlobs([]string{"releases/**", "!releases/**-alpha"})
	require.NoError(t, err)
	assert.True(t, set.Matches("releases/1.0"))
	assert.False(t, set.Matches("releases/1.0-alpha"))

	// A later positive pattern puts part of the exclusion back.
	set2, err := CompileGlobs([]string{"releases/**", "!releases/**-alpha", "releases/final-alpha"})
	require.NoError(t, err)
	assert.True(t, set2.Matches("releases/final-alpha"))

	// A set of only negative patterns selects everything else.
	only, err := CompileGlobs([]string{"!docs/**"})
	require.NoError(t, err)
	assert.True(t, only.Matches("src/main.go"))
	assert.False(t, only.Matches("docs/readme.md"))

	assert.True(t, GlobSet(nil).Matches("anything"), "no filter means everything matches")
}

func pushWorkflow(f model.BranchFilter) *model.Workflow {
	return &model.Workflow{On: model.Triggers{Push: &f}}
}

// The hole this closes: with no filter evaluation, a workflow scoped to master
// ran on every branch.
func TestMatches_BranchFilterIsActuallyApplied(t *testing.T) {
	w := pushWorkflow(model.BranchFilter{Branches: []string{"master"}})

	on, err := Matches(w, Event{Name: "push", Ref: "refs/heads/master"})
	require.NoError(t, err)
	assert.True(t, on.Match)

	off, err := Matches(w, Event{Name: "push", Ref: "refs/heads/feature/x"})
	require.NoError(t, err)
	assert.False(t, off.Match)
	assert.Contains(t, off.Reason, "does not match the branches filter")
}

func TestMatches_BranchesIgnore(t *testing.T) {
	w := pushWorkflow(model.BranchFilter{BranchesIgnore: []string{"docs/**"}})

	on, err := Matches(w, Event{Name: "push", Ref: "refs/heads/main"})
	require.NoError(t, err)
	assert.True(t, on.Match)

	off, err := Matches(w, Event{Name: "push", Ref: "refs/heads/docs/typo"})
	require.NoError(t, err)
	assert.False(t, off.Match)
	assert.Contains(t, off.Reason, "branches-ignore")
}

func TestMatches_TagsAndBranchesAreDistinct(t *testing.T) {
	branchOnly := pushWorkflow(model.BranchFilter{Branches: []string{"**"}})
	d, err := Matches(branchOnly, Event{Name: "push", Ref: "refs/tags/v1.0.0"})
	require.NoError(t, err)
	assert.False(t, d.Match, "a branches-only filter must not fire on a tag")
	assert.Contains(t, d.Reason, "is a tag")

	tagOnly := pushWorkflow(model.BranchFilter{Tags: []string{"v*"}})
	hit, err := Matches(tagOnly, Event{Name: "push", Ref: "refs/tags/v1.0.0"})
	require.NoError(t, err)
	assert.True(t, hit.Match)

	miss, err := Matches(tagOnly, Event{Name: "push", Ref: "refs/heads/main"})
	require.NoError(t, err)
	assert.False(t, miss.Match)
	assert.Contains(t, miss.Reason, "is a branch")
}

func TestMatches_PathFilters(t *testing.T) {
	paths := pushWorkflow(model.BranchFilter{Paths: []string{"src/**"}})

	hit, err := Matches(paths, Event{Name: "push", Ref: "refs/heads/main", ChangedPaths: []string{"README.md", "src/a.go"}})
	require.NoError(t, err)
	assert.True(t, hit.Match)

	miss, err := Matches(paths, Event{Name: "push", Ref: "refs/heads/main", ChangedPaths: []string{"README.md"}})
	require.NoError(t, err)
	assert.False(t, miss.Match)
	assert.Contains(t, miss.Reason, "no changed file matches")

	ignore := pushWorkflow(model.BranchFilter{PathsIgnore: []string{"docs/**"}})

	// A run happens when at least one changed file is outside the ignore set.
	partial, err := Matches(ignore, Event{Name: "push", Ref: "refs/heads/main", ChangedPaths: []string{"docs/a.md", "src/b.go"}})
	require.NoError(t, err)
	assert.True(t, partial.Match)

	all, err := Matches(ignore, Event{Name: "push", Ref: "refs/heads/main", ChangedPaths: []string{"docs/a.md", "docs/b.md"}})
	require.NoError(t, err)
	assert.False(t, all.Match)
	assert.Contains(t, all.Reason, "every changed file is excluded")
}

func TestMatches_PullRequestTypes(t *testing.T) {
	w := &model.Workflow{On: model.Triggers{PullRequest: &model.PullRequestFilter{
		BranchFilter: model.BranchFilter{Branches: []string{"main"}},
		Types:        []string{"opened", "synchronize"},
	}}}

	ok, err := Matches(w, Event{Name: "pull_request", Ref: "refs/heads/main", Action: "opened"})
	require.NoError(t, err)
	assert.True(t, ok.Match)

	wrongType, err := Matches(w, Event{Name: "pull_request", Ref: "refs/heads/main", Action: "labeled"})
	require.NoError(t, err)
	assert.False(t, wrongType.Match)
	assert.Contains(t, wrongType.Reason, "activity type")

	wrongBase, err := Matches(w, Event{Name: "pull_request", Ref: "refs/heads/release", Action: "opened"})
	require.NoError(t, err)
	assert.False(t, wrongBase.Match)
}

func TestMatches_OtherEventKinds(t *testing.T) {
	dispatch := &model.Workflow{On: model.Triggers{WorkflowDispatch: &model.WorkflowDispatch{}}}
	d, err := Matches(dispatch, Event{Name: "workflow_dispatch"})
	require.NoError(t, err)
	assert.True(t, d.Match)

	sched := &model.Workflow{On: model.Triggers{Schedule: []model.ScheduleTrigger{{Cron: "0 * * * *"}}}}
	s, err := Matches(sched, Event{Name: "schedule"})
	require.NoError(t, err)
	assert.True(t, s.Match)

	other := &model.Workflow{On: model.Triggers{Other: map[string]model.RawEvents{
		"issues": {Types: []string{"opened"}},
	}}}
	hit, err := Matches(other, Event{Name: "issues", Action: "opened"})
	require.NoError(t, err)
	assert.True(t, hit.Match)

	miss, err := Matches(other, Event{Name: "issues", Action: "closed"})
	require.NoError(t, err)
	assert.False(t, miss.Match)

	none, err := Matches(&model.Workflow{}, Event{Name: "push"})
	require.NoError(t, err)
	assert.False(t, none.Match)
	assert.Contains(t, none.Reason, "does not listen for push")
}

// A bad pattern must fail loudly rather than silently matching nothing, which
// would look exactly like "my workflow mysteriously stopped running".
func TestMatches_BadPatternIsAnError(t *testing.T) {
	w := pushWorkflow(model.BranchFilter{Branches: []string{"[0-9"}})
	_, err := Matches(w, Event{Name: "push", Ref: "refs/heads/main"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on.branches")

	_, err = Matches(nil, Event{Name: "push"})
	assert.Error(t, err)
}

func TestShortRef(t *testing.T) {
	assert.Equal(t, "main", shortRef("refs/heads/main"))
	assert.Equal(t, "v1.0", shortRef("refs/tags/v1.0"))
	assert.Equal(t, "weird", shortRef("weird"))
}

func TestGlobAccessors(t *testing.T) {
	g, err := CompileGlob("!docs/**")
	require.NoError(t, err)
	assert.Equal(t, "!docs/**", g.Raw())
	assert.True(t, g.Negated())

	_, err = CompileGlobs([]string{"ok", "[bad"})
	assert.Error(t, err)
}
