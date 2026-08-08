// Package demoseed fills a store with the runs the demo site shows.
//
// The data is chosen to answer the question the platform exists to answer:
// what happened, and whose fault was it. Every one of the five incidents in
// docs/incidents.md is visible somewhere in this seed -- an infra failure that
// retried and is not coloured like a build failure, a job whose runner vanished
// and was requeued rather than failed, a cancellation that names its actor and
// says why, a job whose setup time is measured rather than inferred, and a
// green run whose green means work actually ran.
//
// see docs/demo.md
package demoseed

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Now is the demo's fixed clock. A fixed instant keeps a recapture of an
// unchanged seed byte-identical, which is what lets -check mean anything.
var Now = time.Date(2026, 8, 6, 15, 4, 5, 0, time.UTC)

func ago(d time.Duration) time.Time   { return Now.Add(-d) }
func agop(d time.Duration) *time.Time { t := ago(d); return &t }

// repoID is GitHub's id for the demo repository.
const repoID int64 = 4210

const (
	owner = "acme"
	repo  = "widget"
)

// Logs is the log source the seeder writes into.
type Logs interface {
	Append(ctx context.Context, jobID int64, attempt int, lines []model.LogLine) (int64, error)
}

// Seeded is what was created, and the API paths that describe it.
type Seeded struct {
	RunIDs []int64
	JobIDs []int64
}

// Paths lists every URL the demo client will ask for, in a stable order.
func (s Seeded) Paths() []string {
	out := []string{
		"/api/v1/runs",
		"/api/v1/runners",
		"/api/v1/queue",
		"/api/v1/queue/history?since=" + ago(6*time.Hour).UTC().Format(time.RFC3339),
		fmt.Sprintf("/api/v1/repos/%s/%s/cache", owner, repo),
		"/healthz",
	}
	for _, id := range s.RunIDs {
		out = append(out,
			fmt.Sprintf("/api/v1/runs/%d", id),
			fmt.Sprintf("/api/v1/runs/%d/jobs", id),
			fmt.Sprintf("/api/v1/runs/%d/artifacts", id),
		)
	}
	for _, id := range s.JobIDs {
		out = append(out,
			fmt.Sprintf("/api/v1/jobs/%d", id),
			fmt.Sprintf("/api/v1/jobs/%d/logs", id),
		)
	}
	return out
}

// Seed writes the demo's repository, runs, jobs, steps, logs, runners, queue
// history and cache activity.
func Seed(ctx context.Context, st store.Store, logs Logs) (*Seeded, error) {
	if err := st.UpsertRepo(ctx, &model.Repo{
		ID: repoID, Owner: owner, Name: repo, DefaultBranch: "main", InstallationID: 991,
	}); err != nil {
		return nil, err
	}

	s := &Seeded{}
	for _, build := range []func(context.Context, store.Store, Logs, *Seeded) error{
		seedGreenRun,
		seedInfraRun,
		seedUserFailureRun,
		seedRequeuedRun,
		seedCancelledRun,
	} {
		if err := build(ctx, st, logs, s); err != nil {
			return nil, err
		}
	}
	if err := seedFleet(ctx, st); err != nil {
		return nil, err
	}
	if err := seedCache(ctx, st); err != nil {
		return nil, err
	}
	return s, nil
}

// createRun stores a run and remembers it for the path list.
func createRun(ctx context.Context, st store.Store, s *Seeded, r *model.Run) error {
	r.RepoID = repoID
	r.RepoFull = owner + "/" + repo
	if err := st.CreateRun(ctx, r); err != nil {
		return err
	}
	s.RunIDs = append(s.RunIDs, r.ID)
	return nil
}

func createJob(ctx context.Context, st store.Store, s *Seeded, j *model.Job) error {
	if err := st.CreateJob(ctx, j); err != nil {
		return err
	}
	s.JobIDs = append(s.JobIDs, j.ID)
	return nil
}

type stepSpec struct {
	name       string
	conclusion model.Conclusion
	exitCode   int
	took       time.Duration
	logStart   int64
	logEnd     int64
}

// steps writes a job's steps back to back from start.
func steps(ctx context.Context, st store.Store, jobID int64, specs []stepSpec, start time.Time) error {
	at := start
	for i, sp := range specs {
		began, ended := at, at.Add(sp.took)
		at = ended
		class := model.FailureClass("")
		switch sp.conclusion {
		case model.ConclusionFailure:
			class = model.ClassUser
		case model.ConclusionInfraFailure:
			class = model.ClassInfra
		}
		if err := st.UpsertStep(ctx, &model.Step{
			JobID: jobID, Number: i + 1, Name: sp.name, Attempt: attemptOf(jobID),
			Status: model.StatusCompleted, Conclusion: sp.conclusion, Class: class,
			ExitCode: sp.exitCode, StartedAt: &began, CompletedAt: &ended,
			LogStart: sp.logStart, LogEnd: sp.logEnd,
		}); err != nil {
			return err
		}
	}
	return nil
}

// attemptOf is 2 for the job that retried and 1 for everything else. The demo
// has exactly one retried job, so a lookup table would be more machinery than
// the fact deserves.
func attemptOf(jobID int64) int {
	if jobID == retriedJobID {
		return 2
	}
	return 1
}

// retriedJobID is the publish job in the Release run: the fourth job created,
// and the only one on attempt 2.
const retriedJobID int64 = 4

func appendLog(ctx context.Context, logs Logs, jobID int64, attempt int, text []logLine) error {
	lines := make([]model.LogLine, 0, len(text))
	for i, l := range text {
		lines = append(lines, model.LogLine{
			Seq: int64(i + 1), Timestamp: ago(20 * time.Minute).Add(time.Duration(i) * 700 * time.Millisecond),
			StepNumber: l.step, Stream: l.stream, Text: l.text, Group: l.group,
		})
	}
	_, err := logs.Append(ctx, jobID, attempt, lines)
	return err
}
