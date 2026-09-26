// Package installpolicy decides which GitHub accounts this instance does work
// for.
//
// A published GitHub App can be installed by anybody. Installation is the
// installer's decision, not the operator's, and a workflow run is arbitrary
// code on a privileged Docker-in-Docker runner. Without this gate, publishing
// the App hands every stranger who clicks Install a shell on the runner fleet.
//
// So an install by an account the operator did not name is inert: no run, no
// job token, no check run, and no GitHub API call made on its behalf.
//
// see docs/security.md
package installpolicy

import (
	"context"
	"log/slog"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/ghaccounts"
	"github.com/wow-look-at-my/ci-platform/internal/github/webhook"
)

// Guard wraps a webhook.Sink and drops every delivery from an account the policy does not allow.
// It sits at the delivery boundary, not inside ingest, because re-run buttons reach the scheduler directly too.
type Guard struct {
	next   webhook.Sink
	owners *ghaccounts.Set
	log    *slog.Logger
}

// NewGuard wraps next. A refused delivery is answered 200 (nothing for GitHub to redeliver) and logged with its account.
func NewGuard(next webhook.Sink, owners *ghaccounts.Set, log *slog.Logger) *Guard {
	if log == nil {
		log = slog.Default()
	}
	return &Guard{next: next, owners: owners, log: log}
}

func (g *Guard) allowed(m webhook.Meta) bool {
	if g.owners.Contains(ownerOf(m.Repo)) {
		return true
	}
	g.log.Warn("refused a webhook delivery from an account this instance does not serve",
		"owner", ownerOf(m.Repo), "repo", m.Repo.FullName, "event", m.Event,
		"action", m.Action, "installation", m.InstallationID, "sender", m.Sender.Login,
		"serving", g.owners.String())
	return false
}

// Push forwards a push only from an allowed account.
func (g *Guard) Push(ctx context.Context, e *webhook.PushEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.Push(ctx, e)
}

// PullRequest gates on the base repository, whose runners and credentials the run would use; a fork head is governed by fork approval instead.
func (g *Guard) PullRequest(ctx context.Context, e *webhook.PullRequestEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.PullRequest(ctx, e)
}

// WorkflowDispatch forwards a dispatch only from an allowed account.
func (g *Guard) WorkflowDispatch(ctx context.Context, e *webhook.WorkflowDispatchEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.WorkflowDispatch(ctx, e)
}

// CheckRunRerequested forwards a re-run only from an allowed account.
func (g *Guard) CheckRunRerequested(ctx context.Context, e *webhook.CheckRunEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.CheckRunRerequested(ctx, e)
}

// CheckSuiteRerequested forwards a suite re-run only from an allowed account.
func (g *Guard) CheckSuiteRerequested(ctx context.Context, e *webhook.CheckSuiteEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.CheckSuiteRerequested(ctx, e)
}

// RequestedAction forwards a check-run button only from an allowed account.
func (g *Guard) RequestedAction(ctx context.Context, e *webhook.CheckRunEvent) error {
	if !g.allowed(e.Meta) {
		return nil
	}
	return g.next.RequestedAction(ctx, e)
}

// Installation records repositories only for an allowed account, so a stranger's
// install leaves no row behind for a later delivery to match on.
//
// The account is taken from the installation itself: an installation event
// carries no repository in Meta, and its repository list is what would be
// written.
func (g *Guard) Installation(ctx context.Context, e *webhook.InstallationEvent) error {
	account := e.Installation.Account.Login
	if account == "" {
		account = ownerOf(e.Meta.Repo)
	}
	if !g.owners.Contains(account) {
		g.log.Warn("ignored an App installation by an account this instance does not serve",
			"account", account, "installation", e.Installation.ID, "action", e.Meta.Action,
			"repositories", len(e.Repositories), "serving", g.owners.String())
		return nil
	}
	return g.next.Installation(ctx, e)
}

// ownerOf prefers the owner object and falls back to the full name, because a
// trimmed-down payload can carry one without the other.
func ownerOf(r webhook.Repository) string {
	if r.Owner.Login != "" {
		return r.Owner.Login
	}
	if owner, _, found := strings.Cut(r.FullName, "/"); found {
		return owner
	}
	return ""
}
