package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/github/webhook"
	"github.com/wow-look-at-my/ci-platform/internal/ingest"
	"github.com/wow-look-at-my/ci-platform/internal/scheduler"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// sink routes webhook events: run-creating events to the ingester, and the
// check-run buttons to the scheduler's re-run entry points.
type sink struct {
	ing   *ingest.Ingester
	sched *scheduler.Scheduler
	store store.Store
	log   *slog.Logger
}

func newWebhookHandler(secret string, ing *ingest.Ingester, sched *scheduler.Scheduler, st store.Store, log *slog.Logger) (http.Handler, error) {
	return webhook.NewHandler(secret, &sink{ing: ing, sched: sched, store: st, log: log}, webhook.WithLogger(log))
}

func (s *sink) Push(ctx context.Context, e *webhook.PushEvent) error {
	return s.ing.Push(ctx, e)
}

func (s *sink) PullRequest(ctx context.Context, e *webhook.PullRequestEvent) error {
	return s.ing.PullRequest(ctx, e)
}

func (s *sink) WorkflowDispatch(ctx context.Context, e *webhook.WorkflowDispatchEvent) error {
	return s.ing.WorkflowDispatch(ctx, e)
}

func (s *sink) Installation(ctx context.Context, e *webhook.InstallationEvent) error {
	return s.ing.Installation(ctx, e)
}

// CheckRunRerequested handles GitHub's own "Re-run" on a single check.
func (s *sink) CheckRunRerequested(ctx context.Context, e *webhook.CheckRunEvent) error {
	jobID, err := jobIDFrom(e)
	if err != nil {
		return err
	}
	return s.sched.RerunJob(ctx, jobID, actorOf(e))
}

// CheckSuiteRerequested handles "Re-run all checks".
func (s *sink) CheckSuiteRerequested(ctx context.Context, e *webhook.CheckSuiteEvent) error {
	runIDs, err := s.runIDsForSuite(ctx, e)
	if err != nil {
		return err
	}
	for _, id := range runIDs {
		if err := s.sched.Rerun(ctx, id, actorOfSuite(e)); err != nil {
			return err
		}
	}
	return nil
}

// RequestedAction handles the buttons the check run offers.
func (s *sink) RequestedAction(ctx context.Context, e *webhook.CheckRunEvent) error {
	jobID, err := jobIDFrom(e)
	if err != nil {
		return err
	}
	if e.RequestedAction == nil {
		return fmt.Errorf("check run %d sent a requested_action event with no action", e.CheckRun.ID)
	}
	switch e.RequestedAction.Identifier {
	case "rerun":
		return s.sched.RerunJob(ctx, jobID, actorOf(e))
	case "rerun-failed":
		runID, err := runIDFrom(e)
		if err != nil {
			return err
		}
		return s.sched.RerunFailed(ctx, runID, actorOf(e))
	default:
		// An unknown identifier is a bug in whoever posted the button, not
		// something to swallow: the user pressed something and nothing
		// happened, which is exactly the class of silence this platform avoids.
		return fmt.Errorf("check run %d requested unknown action %q",
			e.CheckRun.ID, e.RequestedAction.Identifier)
	}
}

// externalID is "<run_id>/<job_id>/<attempt>", set when the check run was
// created, so a button press maps back to a job without a lookup table.
func jobIDFrom(e *webhook.CheckRunEvent) (int64, error) {
	_, jobID, _, err := parseExternalID(e.CheckRun.ExternalID)
	return jobID, err
}

func runIDFrom(e *webhook.CheckRunEvent) (int64, error) {
	runID, _, _, err := parseExternalID(e.CheckRun.ExternalID)
	return runID, err
}

func parseExternalID(id string) (runID, jobID int64, attempt int, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("check run external id %q is not <run>/<job>/<attempt>", id)
	}
	if runID, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("check run external id %q: bad run id: %w", id, err)
	}
	if jobID, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("check run external id %q: bad job id: %w", id, err)
	}
	if attempt, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, fmt.Errorf("check run external id %q: bad attempt: %w", id, err)
	}
	return runID, jobID, attempt, nil
}

func (s *sink) runIDsForSuite(ctx context.Context, e *webhook.CheckSuiteEvent) ([]int64, error) {
	runs, err := s.store.ListRunsForSHA(ctx, e.Repo.ID, e.CheckSuite.HeadSHA)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

func actorOf(e *webhook.CheckRunEvent) string {
	if e.Sender.Login != "" {
		return e.Sender.Login
	}
	return "github"
}

func actorOfSuite(e *webhook.CheckSuiteEvent) string {
	if e.Sender.Login != "" {
		return e.Sender.Login
	}
	return "github"
}
