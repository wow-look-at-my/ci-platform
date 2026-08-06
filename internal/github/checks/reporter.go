// Package checks writes GitHub check runs, which is the only status surface a
// third-party App can drive well enough for branch protection to work.
//
// Two properties are load-bearing. Updates are coalesced, so a chatty step
// executor cannot burn the API budget; and the completion update is never
// coalesced away, because a check run stuck in_progress blocks a merge forever.
package checks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	gh "github.com/wow-look-at-my/ci-platform/internal/github"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// DefaultMinInterval is the floor between two API calls for one check run.
const DefaultMinInterval = 2 * time.Second

// ReporterOptions configures a Reporter.
type ReporterOptions struct {
	// MinInterval bounds how often one check run is written. Default 2s.
	MinInterval time.Duration
	// FinalAttempts is how many times a completion update is retried before it
	// is reported as lost. Default 3.
	FinalAttempts int
	// FinalBackoff is the first retry delay for a completion update.
	FinalBackoff time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	// OnCheckRunID is called after a check run is created so the caller can
	// persist Job.CheckRunID. Errors here are the caller's problem, not ours.
	OnCheckRunID func(jobID, checkRunID int64)
	// Actions are attached to a completed check run when the Update does not
	// set its own. A non-nil empty slice on the Update means "no buttons".
	Actions []Action
	// DisableTicker turns off background flushing; pending updates then move
	// only on Report, Flush, or Close. Used by tests.
	DisableTicker bool
}

// Reporter queues and writes check run updates.
type Reporter struct {
	cli  *gh.Client
	opts ReporterOptions
	log  *slog.Logger
	now  func() time.Time

	mu    sync.Mutex
	state map[int64]*jobState

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

type jobState struct {
	// sendMu serializes writes for one check run. Held across HTTP.
	sendMu sync.Mutex

	// Guarded by Reporter.mu.
	pending    *Update
	lastSent   time.Time
	checkRunID int64
	annSent    int
	completed  bool
}

// NewReporter starts a Reporter. Close must be called to flush and stop it.
func NewReporter(cli *gh.Client, opts ReporterOptions) *Reporter {
	if opts.MinInterval <= 0 {
		opts.MinInterval = DefaultMinInterval
	}
	if opts.FinalAttempts <= 0 {
		opts.FinalAttempts = 3
	}
	if opts.FinalBackoff <= 0 {
		opts.FinalBackoff = 250 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Actions == nil {
		opts.Actions = DefaultActions()
	}
	r := &Reporter{
		cli:   cli,
		opts:  opts,
		log:   opts.Logger,
		now:   opts.Now,
		state: map[int64]*jobState{},
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if opts.DisableTicker {
		close(r.done)
	} else {
		go r.loop()
	}
	return r
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Reporter) loop() {
	defer close(r.done)
	t := time.NewTicker(r.opts.MinInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.flushDue(context.Background())
		}
	}
}

// Report queues a check-run update. It is safe to call on every step
// transition: non-terminal updates coalesce to at most one API call per
// MinInterval, and a terminal update is written synchronously with retries.
func (r *Reporter) Report(ctx context.Context, u Update) error {
	if r.cli == nil {
		return errors.New("checks: reporter has no GitHub client; the App installation token source was never wired")
	}
	if err := u.Validate(); err != nil {
		return err
	}
	if u.Terminal() && u.Actions == nil {
		u.Actions = r.opts.Actions
	}

	r.mu.Lock()
	st := r.state[u.JobID]
	if st == nil {
		st = &jobState{}
		r.state[u.JobID] = st
	}
	if u.CheckRunID != 0 && st.checkRunID == 0 {
		st.checkRunID = u.CheckRunID
	}
	prev := st.pending
	if prev != nil {
		// A newer update supersedes the queued one, but annotations queued and
		// not yet delivered must not be lost with it.
		u.Annotations = mergeAnnotations(prev.Annotations, u.Annotations)
	}
	st.pending = &u
	first := st.lastSent.IsZero()
	due := first || !r.now().Before(st.lastSent.Add(r.opts.MinInterval))
	r.mu.Unlock()

	if u.Terminal() {
		return r.flushJob(ctx, u.JobID, true)
	}
	if due {
		return r.flushJob(ctx, u.JobID, false)
	}
	return nil
}

// mergeAnnotations keeps the longer list, which is the caller's cumulative one.
func mergeAnnotations(prev, next []model.Annotation) []model.Annotation {
	if len(next) >= len(prev) {
		return next
	}
	return prev
}

// Flush forces any pending update for a job out now.
func (r *Reporter) Flush(ctx context.Context, jobID int64) error {
	return r.flushJob(ctx, jobID, true)
}

// Close stops background flushing and writes every pending update.
func (r *Reporter) Close(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done

	r.mu.Lock()
	ids := make([]int64, 0, len(r.state))
	for id, st := range r.state {
		if st.pending != nil {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := r.flushJob(ctx, id, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Reporter) flushDue(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	var ids []int64
	for id, st := range r.state {
		if st.pending != nil && !now.Before(st.lastSent.Add(r.opts.MinInterval)) {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		if err := r.flushJob(ctx, id, false); err != nil {
			r.log.Error("check run update failed", "job_id", id, "err", err)
		}
	}
}

// flushJob writes the pending update for one job. force blocks for the send
// lock and retries; a non-forced flush skips a check run already being written.
func (r *Reporter) flushJob(ctx context.Context, jobID int64, force bool) error {
	r.mu.Lock()
	st := r.state[jobID]
	r.mu.Unlock()
	if st == nil {
		return nil
	}
	if force {
		st.sendMu.Lock()
	} else if !st.sendMu.TryLock() {
		return nil
	}
	defer st.sendMu.Unlock()

	r.mu.Lock()
	u := st.pending
	st.pending = nil
	r.mu.Unlock()
	if u == nil {
		return nil
	}

	attempts := 1
	if force && u.Terminal() {
		attempts = r.opts.FinalAttempts
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := r.opts.FinalBackoff << (i - 1)
			if err := r.opts.Sleep(ctx, wait); err != nil {
				lastErr = errors.Join(lastErr, err)
				break
			}
		}
		err := r.write(ctx, st, *u)
		if err == nil {
			r.mu.Lock()
			st.lastSent = r.now()
			st.completed = u.Terminal()
			r.mu.Unlock()
			return nil
		}
		lastErr = err
		r.log.Warn("check run write failed",
			"job_id", jobID, "name", u.Name, "attempt", i+1, "of", attempts, "err", err)
	}

	// Never dropped silently: requeue it so a later Flush retries, and say so.
	r.mu.Lock()
	if st.pending == nil {
		st.pending = u
	}
	r.mu.Unlock()
	if errors.Is(lastErr, gh.ErrRateLimited) {
		r.log.Error("check run update blocked by the GitHub rate limit and was NOT delivered",
			"job_id", jobID, "name", u.Name, "status", u.Status, "conclusion", u.Conclusion,
			"rate_limit", r.cli.RateLimit(), "err", lastErr)
	} else if u.Terminal() {
		r.log.Error("final check run update was NOT delivered; the check run is still open on GitHub",
			"job_id", jobID, "name", u.Name, "conclusion", u.Conclusion, "err", lastErr)
	}
	return fmt.Errorf("checks: job %d (%s): %w", jobID, u.Name, lastErr)
}

// checkRunPayload is the wire body of a create or update.
type checkRunPayload struct {
	Name        string   `json:"name"`
	HeadSHA     string   `json:"head_sha,omitempty"`
	DetailsURL  string   `json:"details_url,omitempty"`
	ExternalID  string   `json:"external_id,omitempty"`
	Status      string   `json:"status,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	Conclusion  string   `json:"conclusion,omitempty"`
	CompletedAt string   `json:"completed_at,omitempty"`
	Output      *Output  `json:"output,omitempty"`
	Actions     []Action `json:"actions,omitempty"`
}

type checkRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// write performs the create-or-update plus any annotation follow-ups.
func (r *Reporter) write(ctx context.Context, st *jobState, u Update) error {
	r.mu.Lock()
	id := st.checkRunID
	annSent := st.annSent
	r.mu.Unlock()

	now := r.now()
	out := Render(u, now)

	pendingAnns := u.Annotations
	if annSent > 0 && annSent <= len(pendingAnns) {
		pendingAnns = pendingAnns[annSent:]
	}
	head, rest := chunkAnnotations(pendingAnns)
	out.Annotations = head

	body := checkRunPayload{
		Name:       u.Name,
		DetailsURL: u.DetailsURL,
		ExternalID: u.ExternalID,
		Status:     string(u.Status),
		Output:     &out,
		Actions:    u.Actions,
	}
	if u.StartedAt != nil {
		body.StartedAt = u.StartedAt.UTC().Format(time.RFC3339)
	}
	if u.Terminal() {
		conclusion, _ := ConclusionToCheck(u.Conclusion)
		body.Conclusion = conclusion
		completed := now
		if u.CompletedAt != nil {
			completed = *u.CompletedAt
		}
		body.CompletedAt = completed.UTC().Format(time.RFC3339)
	}

	var resp checkRunResponse
	if id == 0 {
		body.HeadSHA = u.HeadSHA
		if _, err := r.cli.Post(ctx, checkRunsPath(u.Repo), body, &resp); err != nil {
			return err
		}
		if resp.ID == 0 {
			return fmt.Errorf("checks: GitHub created a check run for %s with no id", u.Name)
		}
		id = resp.ID
		r.mu.Lock()
		st.checkRunID = id
		r.mu.Unlock()
		if r.opts.OnCheckRunID != nil {
			r.opts.OnCheckRunID(u.JobID, id)
		}
	} else {
		if _, err := r.cli.Patch(ctx, checkRunPath(u.Repo, id), body, &resp); err != nil {
			return err
		}
	}

	delivered := len(head)
	for _, chunk := range rest {
		follow := checkRunPayload{
			Name: u.Name,
			Output: &Output{
				Title:       out.Title,
				Summary:     out.Summary,
				Text:        out.Text,
				Annotations: chunk,
			},
		}
		if _, err := r.cli.Patch(ctx, checkRunPath(u.Repo, id), follow, &resp); err != nil {
			r.mu.Lock()
			st.annSent = annSent + delivered
			r.mu.Unlock()
			return fmt.Errorf("checks: delivering annotations for %s: %w", u.Name, err)
		}
		delivered += len(chunk)
	}

	r.mu.Lock()
	st.annSent = annSent + delivered
	r.mu.Unlock()
	return nil
}

// chunkAnnotations converts and splits annotations at GitHub's per-request cap.
func chunkAnnotations(in []model.Annotation) (head []Annotation, rest [][]Annotation) {
	if len(in) == 0 {
		return nil, nil
	}
	all := make([]Annotation, 0, len(in))
	for _, a := range in {
		all = append(all, annotationFor(a))
	}
	head = all[:min(MaxAnnotationsPerRequest, len(all))]
	for i := len(head); i < len(all); i += MaxAnnotationsPerRequest {
		rest = append(rest, all[i:min(i+MaxAnnotationsPerRequest, len(all))])
	}
	return head, rest
}

func checkRunsPath(repo gh.Repo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/check-runs"
}

func checkRunPath(repo gh.Repo, id int64) string {
	return fmt.Sprintf("%s/%d", checkRunsPath(repo), id)
}

// CheckRunID returns the check run id known for a job, 0 if none was created.
func (r *Reporter) CheckRunID(jobID int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.state[jobID]; st != nil {
		return st.checkRunID
	}
	return 0
}
