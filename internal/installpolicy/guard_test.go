package installpolicy

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/ghaccounts"
	"github.com/wow-look-at-my/ci-platform/internal/github/webhook"
)

// countingSink records that a delivery got past the guard. Anything reaching it
// from a stranger is a run this instance would have executed.
type countingSink struct{ calls []string }

func (s *countingSink) Push(context.Context, *webhook.PushEvent) error {
	s.calls = append(s.calls, "push")
	return nil
}

func (s *countingSink) PullRequest(context.Context, *webhook.PullRequestEvent) error {
	s.calls = append(s.calls, "pull_request")
	return nil
}

func (s *countingSink) WorkflowDispatch(context.Context, *webhook.WorkflowDispatchEvent) error {
	s.calls = append(s.calls, "workflow_dispatch")
	return nil
}

func (s *countingSink) CheckRunRerequested(context.Context, *webhook.CheckRunEvent) error {
	s.calls = append(s.calls, "check_run")
	return nil
}

func (s *countingSink) CheckSuiteRerequested(context.Context, *webhook.CheckSuiteEvent) error {
	s.calls = append(s.calls, "check_suite")
	return nil
}

func (s *countingSink) RequestedAction(context.Context, *webhook.CheckRunEvent) error {
	s.calls = append(s.calls, "requested_action")
	return nil
}

func (s *countingSink) Installation(context.Context, *webhook.InstallationEvent) error {
	s.calls = append(s.calls, "installation")
	return nil
}

func guardFor(t *testing.T, owners ...string) (*Guard, *countingSink) {
	t.Helper()
	set, err := ghaccounts.New("CIPLATFORM_ALLOWED_OWNERS", owners)
	require.NoError(t, err)
	next := &countingSink{}
	return NewGuard(next, set, slog.New(slog.NewTextHandler(io.Discard, nil))), next
}

func metaFor(owner string) webhook.Meta {
	return webhook.Meta{
		Event: "push",
		Repo: webhook.Repository{
			ID: 42, Name: "loot", FullName: owner + "/loot",
			Owner: webhook.User{Login: owner},
		},
		InstallationID: 7,
		Sender:         webhook.User{Login: owner},
	}
}

// Every sink method is a way to start work, so every one of them has to be shut
// for a stranger. A single unguarded method is arbitrary code on the runners.
func TestGuard_RefusesEveryEventFromAnAccountNotServed(t *testing.T) {
	g, next := guardFor(t, "PazerOP")
	ctx := context.Background()
	m := metaFor("a-total-stranger")

	require.NoError(t, g.Push(ctx, &webhook.PushEvent{Meta: m}))
	require.NoError(t, g.PullRequest(ctx, &webhook.PullRequestEvent{Meta: m}))
	require.NoError(t, g.WorkflowDispatch(ctx, &webhook.WorkflowDispatchEvent{Meta: m}))
	require.NoError(t, g.CheckRunRerequested(ctx, &webhook.CheckRunEvent{Meta: m}))
	require.NoError(t, g.CheckSuiteRerequested(ctx, &webhook.CheckSuiteEvent{Meta: m}))
	require.NoError(t, g.RequestedAction(ctx, &webhook.CheckRunEvent{Meta: m}))
	require.NoError(t, g.Installation(ctx, &webhook.InstallationEvent{
		Meta:         m,
		Installation: webhook.Installation{ID: 7, Account: webhook.InstallationAccount{Login: "a-total-stranger"}},
		Repositories: []webhook.Repository{m.Repo},
	}))

	assert.Empty(t, next.calls, "a stranger's delivery reached the platform")
}

func TestGuard_PassesEveryEventFromAServedAccount(t *testing.T) {
	g, next := guardFor(t, "PazerOP")
	ctx := context.Background()
	// The login GitHub sends is not necessarily the case the operator typed.
	m := metaFor("pazerop")

	require.NoError(t, g.Push(ctx, &webhook.PushEvent{Meta: m}))
	require.NoError(t, g.PullRequest(ctx, &webhook.PullRequestEvent{Meta: m}))
	require.NoError(t, g.WorkflowDispatch(ctx, &webhook.WorkflowDispatchEvent{Meta: m}))
	require.NoError(t, g.CheckRunRerequested(ctx, &webhook.CheckRunEvent{Meta: m}))
	require.NoError(t, g.CheckSuiteRerequested(ctx, &webhook.CheckSuiteEvent{Meta: m}))
	require.NoError(t, g.RequestedAction(ctx, &webhook.CheckRunEvent{Meta: m}))
	require.NoError(t, g.Installation(ctx, &webhook.InstallationEvent{
		Meta:         m,
		Installation: webhook.Installation{ID: 7, Account: webhook.InstallationAccount{Login: "PazerOP"}},
	}))

	assert.Len(t, next.calls, 7)
}

// A payload can carry the full name without the owner object.
func TestGuard_FallsBackToTheFullName(t *testing.T) {
	g, next := guardFor(t, "PazerOP")
	m := webhook.Meta{Event: "push", Repo: webhook.Repository{FullName: "PazerOP/ci-platform"}}

	require.NoError(t, g.Push(context.Background(), &webhook.PushEvent{Meta: m}))
	assert.Equal(t, []string{"push"}, next.calls)
}

// A pull request from a fork carries the fork in head.repo and the base repo in
// Meta. The base repo owns the runners, so it is the one that decides.
func TestGuard_ForkPullRequestIsJudgedByItsBase(t *testing.T) {
	g, next := guardFor(t, "PazerOP")
	e := &webhook.PullRequestEvent{Meta: metaFor("PazerOP")}
	e.PullRequest.Head.Repo = webhook.Repository{
		FullName: "a-total-stranger/fork", Owner: webhook.User{Login: "a-total-stranger"},
	}

	require.NoError(t, g.PullRequest(context.Background(), e))
	assert.Equal(t, []string{"pull_request"}, next.calls, "fork approval governs this, not the account list")
}

func TestGuard_AnyoneServesEverybody(t *testing.T) {
	g, next := guardFor(t, ghaccounts.Anyone)
	require.NoError(t, g.Push(context.Background(), &webhook.PushEvent{Meta: metaFor("a-total-stranger")}))
	assert.Equal(t, []string{"push"}, next.calls)
}

// An installation event carries no repository in Meta, so the account has to
// come off the installation itself or every install would be judged as "".
func TestGuard_InstallationIsJudgedByItsAccount(t *testing.T) {
	g, next := guardFor(t, "PazerOP")
	e := &webhook.InstallationEvent{
		Meta:         webhook.Meta{Event: "installation", Action: "created"},
		Installation: webhook.Installation{ID: 7, Account: webhook.InstallationAccount{Login: "PazerOP"}},
	}

	require.NoError(t, g.Installation(context.Background(), e))
	assert.Equal(t, []string{"installation"}, next.calls)
}
