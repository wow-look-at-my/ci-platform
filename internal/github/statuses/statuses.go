// Package statuses writes legacy commit statuses alongside check runs, so
// branch protection rules written against the old API keep matching.
//
// One context per job, named exactly like the check run. The context named
// all-builds belongs to an org-level app that aggregates builds itself; a
// context posted under that name shadows the real status in the UI and makes a
// red gate look green, so posting it is refused here rather than reviewed for.
package statuses

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// MaxDescription is the commit status description limit GitHub enforces.
const MaxDescription = 140

// MaxContext is the commit status context limit.
const MaxContext = 255

// State is a legacy commit status state.
type State string

const (
	// StateError is "something went wrong that is not your build" -- the legacy
	// API's only slot for an infrastructure or configuration failure.
	StateError   State = "error"
	StateFailure State = "failure"
	StatePending State = "pending"
	StateSuccess State = "success"
)

// AllBuildsContext is the org-owned aggregate context this package refuses to
// post.
const AllBuildsContext = "all-builds"

// ErrForbiddenContext is returned when a caller tries to post a reserved
// context.
var ErrForbiddenContext = errors.New("statuses: forbidden context")

// StateFor maps a platform status and conclusion onto the legacy states.
//
// infra_failure and config_error map to "error", not "failure": failure is the
// legacy API's "your build is broken", and neither of these is that.
func StateFor(s model.Status, c model.Conclusion) State {
	if s != model.StatusCompleted {
		return StatePending
	}
	switch c {
	case model.ConclusionSuccess:
		return StateSuccess
	case model.ConclusionFailure, model.ConclusionTimedOut:
		return StateFailure
	case model.ConclusionInfraFailure, model.ConclusionConfigError, model.ConclusionActionRequired:
		return StateError
	case model.ConclusionCancelled:
		// A cancelled job did not pass. Reporting success here would let a
		// cancellation satisfy a required check.
		return StateError
	case model.ConclusionSkipped, model.ConclusionNeutral, model.ConclusionStale:
		return StateSuccess
	}
	return StateError
}

// Status is one commit status write.
type Status struct {
	Repo        gh.Repo
	SHA         string
	Context     string
	State       State
	Description string
	TargetURL   string
}

// Options configures a Reporter.
type Options struct {
	// ForbiddenContexts are refused in addition to all-builds, which can never
	// be removed from the set.
	ForbiddenContexts []string
	Logger            *slog.Logger
}

// Reporter posts legacy commit statuses.
type Reporter struct {
	cli       *gh.Client
	forbidden map[string]struct{}
	log       *slog.Logger
}

// NewReporter builds a Reporter. all-builds is always forbidden.
func NewReporter(cli *gh.Client, opts Options) *Reporter {
	f := map[string]struct{}{normalizeContext(AllBuildsContext): {}}
	for _, c := range opts.ForbiddenContexts {
		if n := normalizeContext(c); n != "" {
			f[n] = struct{}{}
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Reporter{cli: cli, forbidden: f, log: log}
}

func normalizeContext(c string) string { return strings.ToLower(strings.TrimSpace(c)) }

// Forbidden reports whether a context is refused.
func (r *Reporter) Forbidden(context string) bool {
	_, bad := r.forbidden[normalizeContext(context)]
	return bad
}

type statusPayload struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// CommitStatus is the created status.
type CommitStatus struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// Post writes one commit status.
func (r *Reporter) Post(ctx context.Context, s Status) (*CommitStatus, error) {
	if r.cli == nil {
		return nil, errors.New("statuses: reporter has no GitHub client")
	}
	if !s.Repo.Valid() {
		return nil, fmt.Errorf("statuses: status for %q has no repo owner/name", s.Context)
	}
	if s.SHA == "" {
		return nil, fmt.Errorf("statuses: status %q on %s has no SHA", s.Context, s.Repo)
	}
	if s.Context == "" {
		return nil, fmt.Errorf("statuses: status on %s@%s has no context", s.Repo, s.SHA)
	}
	if len(s.Context) > MaxContext {
		return nil, fmt.Errorf("statuses: context %q is %d chars, limit %d", s.Context, len(s.Context), MaxContext)
	}
	if !s.State.valid() {
		return nil, fmt.Errorf("statuses: state %q is not error/failure/pending/success", s.State)
	}
	if r.Forbidden(s.Context) {
		return nil, fmt.Errorf("%w: %q is owned by another app and must never be posted by this platform", ErrForbiddenContext, s.Context)
	}

	body := statusPayload{
		State:       string(s.State),
		TargetURL:   s.TargetURL,
		Description: TruncateDescription(s.Description),
		Context:     s.Context,
	}
	var out CommitStatus
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s",
		url.PathEscape(s.Repo.Owner), url.PathEscape(s.Repo.Name), url.PathEscape(s.SHA))
	if _, err := r.cli.Post(ctx, path, body, &out); err != nil {
		return nil, fmt.Errorf("statuses: posting %q to %s@%s: %w", s.Context, s.Repo, s.SHA, err)
	}
	return &out, nil
}

// PostJob posts the status for one job, using the check run's own name as the
// context so existing protection rules keep matching.
func (r *Reporter) PostJob(ctx context.Context, repo gh.Repo, sha string, job *model.Job, description, targetURL string) (*CommitStatus, error) {
	if job == nil {
		return nil, errors.New("statuses: PostJob got a nil job")
	}
	return r.Post(ctx, Status{
		Repo:        repo,
		SHA:         sha,
		Context:     job.Name,
		State:       StateFor(job.Status, job.Conclusion),
		Description: description,
		TargetURL:   targetURL,
	})
}

func (s State) valid() bool {
	switch s {
	case StateError, StateFailure, StatePending, StateSuccess:
		return true
	}
	return false
}

// TruncateDescription cuts a description to GitHub's 140-char limit, marking
// the cut so a reader knows the sentence is not the whole story.
func TruncateDescription(s string) string {
	if len(s) <= MaxDescription {
		return s
	}
	const ellipsis = "..."
	cut := MaxDescription - len(ellipsis)
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + ellipsis
}
