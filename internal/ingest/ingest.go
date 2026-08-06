// Package ingest turns a webhook event into runs: it discovers the repository's
// workflow files at the event's SHA, parses each to IR, decides which ones the
// event triggers, and starts the ones that do.
//
// The rule that shapes this package: a workflow that cannot be parsed, or that
// uses a feature this platform does not implement, produces a FAILED run with
// the reason on it. It is never skipped quietly. A workflow silently absent
// from a commit's checks is indistinguishable from one that passed.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/github/webhook"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/store"
	"github.com/wow-look-at-my/ci-platform/internal/workflow"
)

// Files reads a repository's workflow files at a ref.
type Files interface {
	ListWorkflowFiles(ctx context.Context, repo gh.Repo, ref string) ([]gh.WorkflowFile, error)
	GetFileContents(ctx context.Context, repo gh.Repo, path, ref string) (*gh.FileContent, error)
}

// Starter starts a planned run.
type Starter interface {
	StartRun(ctx context.Context, run *model.Run, p *plan.Plan) error
}

// Options configures the ingester.
type Options struct {
	Store   store.Store
	Files   Files
	Starter Starter
	NewEval plan.EvaluatorFactory

	// ServerURL is this control plane's public base URL, used to build the
	// github context's server URLs.
	ServerURL string
	Logger    *slog.Logger
	Now       func() time.Time
}

// Ingester implements webhook.Sink.
type Ingester struct {
	opts Options
	log  *slog.Logger
}

// New builds an ingester.
func New(opts Options) (*Ingester, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("ingest: Store is required")
	case opts.Files == nil:
		return nil, errors.New("ingest: Files is required")
	case opts.Starter == nil:
		return nil, errors.New("ingest: Starter is required")
	case opts.NewEval == nil:
		return nil, errors.New("ingest: NewEval is required")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Ingester{opts: opts, log: opts.Logger}, nil
}

// Trigger is one event reduced to what workflow matching and run creation need.
type Trigger struct {
	Event        string
	Action       string
	Repo         webhook.Repository
	Ref          string
	HeadSHA      string
	HeadBranch   string
	BaseBranch   string
	Actor        string
	IsForkPR     bool
	ChangedPaths []string
	Inputs       map[string]any
	// WorkflowPath, when set, restricts ingest to one workflow file, which is
	// what workflow_dispatch names.
	WorkflowPath string
	Raw          json.RawMessage
}

// Push handles a push event.
func (i *Ingester) Push(ctx context.Context, e *webhook.PushEvent) error {
	if e.Deleted {
		i.log.Info("ignoring push that deleted a ref", "ref", e.Ref, "repo", e.Repo.FullName)
		return nil
	}
	return i.Handle(ctx, Trigger{
		Event: "push", Repo: e.Repo, Ref: e.Ref, HeadSHA: e.After,
		HeadBranch: strings.TrimPrefix(e.Ref, "refs/heads/"),
		Actor:      e.Sender.Login, ChangedPaths: changedPaths(e), Raw: e.Raw,
	})
}

// PullRequest handles a pull_request event.
func (i *Ingester) PullRequest(ctx context.Context, e *webhook.PullRequestEvent) error {
	return i.Handle(ctx, Trigger{
		Event: "pull_request", Action: e.Action, Repo: e.Repo,
		// branches: on a pull_request filters the BASE ref, not the head.
		Ref:        "refs/heads/" + e.PullRequest.Base.Ref,
		HeadSHA:    e.PullRequest.Head.SHA,
		HeadBranch: e.PullRequest.Head.Ref,
		BaseBranch: e.PullRequest.Base.Ref,
		Actor:      e.Sender.Login,
		IsForkPR:   e.IsFork(),
		Raw:        e.Raw,
	})
}

// WorkflowDispatch handles a manual dispatch.
func (i *Ingester) WorkflowDispatch(ctx context.Context, e *webhook.WorkflowDispatchEvent) error {
	return i.Handle(ctx, Trigger{
		Event: "workflow_dispatch", Repo: e.Repo, Ref: e.Ref,
		HeadBranch: strings.TrimPrefix(e.Ref, "refs/heads/"),
		Actor:      e.Sender.Login, Inputs: e.Inputs,
		WorkflowPath: e.Workflow, Raw: e.Raw,
	})
}

// Installation records the repositories an installation covers.
func (i *Ingester) Installation(ctx context.Context, e *webhook.InstallationEvent) error {
	for _, r := range e.Repositories {
		repo := &model.Repo{
			ID: r.ID, Owner: ownerOf(r), Name: r.Name,
			InstallationID: e.Meta.InstallationID,
			DefaultBranch:  r.DefaultBranch, Private: r.Private,
		}
		if err := i.opts.Store.UpsertRepo(ctx, repo); err != nil {
			return fmt.Errorf("ingest: record repo %s: %w", r.FullName, err)
		}
	}
	return nil
}

// CheckRunRerequested, CheckSuiteRerequested, and RequestedAction are wired by
// the caller to the scheduler's re-run entry points; ingest only creates runs
// from events, so these are not its job.
func (i *Ingester) CheckRunRerequested(context.Context, *webhook.CheckRunEvent) error { return nil }

// CheckSuiteRerequested is a no-op here for the same reason.
func (i *Ingester) CheckSuiteRerequested(context.Context, *webhook.CheckSuiteEvent) error {
	return nil
}

// RequestedAction is a no-op here for the same reason.
func (i *Ingester) RequestedAction(context.Context, *webhook.CheckRunEvent) error { return nil }

// Handle discovers, parses, matches, and starts the workflows for one trigger.
func (i *Ingester) Handle(ctx context.Context, t Trigger) error {
	if t.HeadSHA == "" {
		return fmt.Errorf("ingest: %s event for %s carries no head SHA", t.Event, t.Repo.FullName)
	}
	repo, err := i.repo(ctx, t)
	if err != nil {
		return err
	}

	ghRepo := gh.Repo{Owner: repo.Owner, Name: repo.Name}
	files, err := i.opts.Files.ListWorkflowFiles(ctx, ghRepo, t.HeadSHA)
	if err != nil {
		return fmt.Errorf("ingest: list workflows for %s@%s: %w", repo.FullName(), t.HeadSHA, err)
	}

	var firstErr error
	for _, f := range files {
		if t.WorkflowPath != "" && f.Path != t.WorkflowPath && f.Name != t.WorkflowPath {
			continue
		}
		if err := i.handleFile(ctx, repo, ghRepo, t, f); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (i *Ingester) handleFile(ctx context.Context, repo *model.Repo, ghRepo gh.Repo, t Trigger, f gh.WorkflowFile) error {
	content, err := i.opts.Files.GetFileContents(ctx, ghRepo, f.Path, t.HeadSHA)
	if err != nil {
		return fmt.Errorf("ingest: read %s: %w", f.Path, err)
	}

	w, perr := workflow.Parse(f.Path, content.Content)
	if perr != nil {
		// A workflow that cannot be parsed produces a failed run carrying the
		// parse error. Skipping it would leave the commit looking clean.
		return i.failRun(ctx, repo, t, f.Path, f.Name, model.ClassConfig, perr.Error())
	}

	dec, err := workflow.Matches(w, workflow.Event{
		Name: t.Event, Ref: t.Ref, Action: t.Action, ChangedPaths: t.ChangedPaths,
	})
	if err != nil {
		// A filter that cannot be compiled is a config error, not a reason to
		// quietly not run.
		return i.failRun(ctx, repo, t, f.Path, w.Name, model.ClassConfig, err.Error())
	}
	if !dec.Match {
		i.log.Debug("workflow not triggered", "workflow", f.Path, "reason", dec.Reason)
		return nil
	}

	if un := workflow.Unsupported(w); len(un) > 0 {
		msgs := make([]string, 0, len(un))
		for _, u := range un {
			msgs = append(msgs, u.String())
		}
		return i.failRun(ctx, repo, t, f.Path, w.Name, model.ClassConfig, strings.Join(msgs, "\n"))
	}

	run, err := i.createRun(ctx, repo, t, f.Path, w.Name)
	if err != nil {
		return err
	}

	p, err := plan.Build(w, plan.Input{
		Run:      run,
		Contexts: i.contexts(repo, run, t),
		NewEval:  i.opts.NewEval,
	})
	if err != nil {
		return i.completeAsConfigError(ctx, run, err.Error())
	}
	if err := i.opts.Starter.StartRun(ctx, run, p); err != nil {
		return fmt.Errorf("ingest: start run %d: %w", run.ID, err)
	}
	i.log.Info("run started", "run", run.ID, "workflow", f.Path, "repo", repo.FullName(),
		"sha", t.HeadSHA, "jobs", len(p.Jobs))
	return nil
}

func (i *Ingester) repo(ctx context.Context, t Trigger) (*model.Repo, error) {
	repo, err := i.opts.Store.GetRepo(ctx, t.Repo.ID)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("ingest: look up repo %d: %w", t.Repo.ID, err)
	}
	owner, name := splitFullName(t.Repo.FullName, t.Repo.Name)
	repo = &model.Repo{
		ID: t.Repo.ID, Owner: owner, Name: name,
		DefaultBranch: t.Repo.DefaultBranch, Private: t.Repo.Private,
	}
	if err := i.opts.Store.UpsertRepo(ctx, repo); err != nil {
		return nil, fmt.Errorf("ingest: record repo %s: %w", t.Repo.FullName, err)
	}
	return repo, nil
}

func (i *Ingester) createRun(ctx context.Context, repo *model.Repo, t Trigger, path, name string) (*model.Run, error) {
	num, err := i.opts.Store.NextRunNumber(ctx, repo.ID, path)
	if err != nil {
		return nil, fmt.Errorf("ingest: allocate run number: %w", err)
	}
	if name == "" {
		name = path
	}
	run := &model.Run{
		RepoID: repo.ID, RepoFull: repo.FullName(),
		WorkflowName: name, WorkflowPath: path,
		RunNumber: num, Attempt: 1,
		Event: t.Event, HeadSHA: t.HeadSHA, HeadBranch: t.HeadBranch,
		BaseBranch: t.BaseBranch, Actor: t.Actor, IsForkPR: t.IsForkPR,
		Status: model.StatusQueued, EventPayload: t.Raw, Inputs: t.Inputs,
		CreatedAt: i.opts.Now(),
	}
	if err := i.opts.Store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("ingest: create run: %w", err)
	}
	return run, nil
}

// failRun records a run that could not start, so the failure is visible on the
// commit rather than being a workflow that silently never ran.
func (i *Ingester) failRun(ctx context.Context, repo *model.Repo, t Trigger, path, name string, class model.FailureClass, msg string) error {
	run, err := i.createRun(ctx, repo, t, path, name)
	if err != nil {
		return err
	}
	i.log.Warn("workflow rejected", "workflow", path, "repo", repo.FullName(), "class", class, "reason", msg)
	return i.completeAsConfigError(ctx, run, msg)
}

func (i *Ingester) completeAsConfigError(ctx context.Context, run *model.Run, msg string) error {
	now := i.opts.Now()
	run.Status = model.StatusCompleted
	run.Conclusion = model.ConclusionConfigError
	run.StartedAt = &now
	run.CompletedAt = &now
	if err := i.opts.Store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("ingest: record rejected run: %w", err)
	}
	return i.opts.Store.RecordEvent(ctx, store.Event{
		RunID: run.ID, Kind: "config_error", Message: msg, At: now,
	})
}

// contexts builds the run-scoped expression contexts. Secrets are deliberately
// absent: a fork PR must never see them, and the scheduler injects them per job
// after that check.
func (i *Ingester) contexts(repo *model.Repo, run *model.Run, t Trigger) map[string]any {
	var event any
	if len(t.Raw) > 0 {
		_ = json.Unmarshal(t.Raw, &event)
	}
	inputs := t.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	refType := "branch"
	if strings.HasPrefix(t.Ref, "refs/tags/") {
		refType = "tag"
	}
	return map[string]any{
		"github": map[string]any{
			"repository":       repo.FullName(),
			"repository_owner": repo.Owner,
			"repository_id":    fmt.Sprint(repo.ID),
			"event_name":       t.Event,
			"event":            event,
			"sha":              run.HeadSHA,
			"ref":              t.Ref,
			"ref_name":         strings.TrimPrefix(strings.TrimPrefix(t.Ref, "refs/heads/"), "refs/tags/"),
			"ref_type":         refType,
			"head_ref":         run.HeadBranch,
			"base_ref":         run.BaseBranch,
			"actor":            run.Actor,
			"workflow":         run.WorkflowName,
			"run_id":           fmt.Sprint(run.ID),
			"run_number":       fmt.Sprint(run.RunNumber),
			"run_attempt":      fmt.Sprint(run.Attempt),
			"server_url":       i.opts.ServerURL,
			"api_url":          i.opts.ServerURL + "/api/v1",
			"workspace":        "/workspace",
		},
		"inputs": inputs,
	}
}

// changedPaths collects every file touched anywhere in the push. Using only
// the head commit would miss files changed by earlier commits in the same push,
// so a paths: filter would silently skip a workflow that should have run.
func changedPaths(e *webhook.PushEvent) []string {
	seen := map[string]bool{}
	var out []string
	add := func(c *webhook.Commit) {
		if c == nil {
			return
		}
		for _, group := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, p := range group {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	for i := range e.Commits {
		add(&e.Commits[i])
	}
	add(e.HeadCommit)
	return out
}

func splitFullName(full, fallbackName string) (string, string) {
	if owner, name, ok := strings.Cut(full, "/"); ok {
		return owner, name
	}
	return "", fallbackName
}

func ownerOf(r webhook.Repository) string {
	if r.Owner.Login != "" {
		return r.Owner.Login
	}
	owner, _ := splitFullName(r.FullName, r.Name)
	return owner
}
