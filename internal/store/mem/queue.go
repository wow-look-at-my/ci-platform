package mem

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Event kinds written by the queue, identical to the Postgres store's.
const (
	EventDispatched   = "dispatched"
	EventRedispatched = "redispatched"
	EventRequeued     = "requeued"
	EventRunnerLost   = "runner_lost"
)

// labelsSatisfied reports whether a runner offering have can take a job
// requiring want: every required label must be present.
func labelsSatisfied(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	for _, l := range want {
		if _, ok := set[l]; !ok {
			return false
		}
	}
	return true
}

// Enqueue makes a job eligible for dispatch. It is idempotent on job id: a job
// that is already queued or already leased keeps its row, its queue position,
// and its lease.
func (s *Store) Enqueue(_ context.Context, q store.QueuedJob) error {
	if q.JobID == 0 {
		return fmt.Errorf("mem: Enqueue: queued job has no job id")
	}
	queuedAt := q.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = time.Now()
	}
	notBefore := q.NotBefore
	if notBefore.IsZero() {
		notBefore = queuedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[q.JobID]
	if !ok {
		return fmt.Errorf("mem: Enqueue: job %d: %w", q.JobID, store.ErrNotFound)
	}
	if q.RunID != 0 && q.RunID != j.RunID {
		return fmt.Errorf("mem: Enqueue: job %d belongs to run %d, not %d", q.JobID, j.RunID, q.RunID)
	}
	// The caller states the attempt because it is half of the dispatch
	// idempotency key. Disagreeing with the job row means the caller and the
	// store have different ideas about which attempt this is, which would let
	// the same attempt dispatch twice; refuse rather than pick.
	if q.Attempt != 0 && q.Attempt != j.Attempt {
		return fmt.Errorf("mem: Enqueue: job %d is on attempt %d, caller enqueued attempt %d",
			q.JobID, j.Attempt, q.Attempt)
	}
	if _, exists := s.queue[q.JobID]; exists {
		// Already queued or leased. Not an error, and not a second row.
		return nil
	}
	s.queue[q.JobID] = &queueRow{
		jobID:     q.JobID,
		runID:     j.RunID,
		attempt:   j.Attempt,
		labels:    cloneStrings(q.Labels),
		group:     q.Group,
		priority:  q.Priority,
		queuedAt:  queuedAt.UTC(),
		notBefore: notBefore.UTC(),
	}
	j.Status = model.StatusQueued
	j.Labels = cloneStrings(q.Labels)
	j.ConcurrencyGroup = q.Group
	if j.QueuedAt == nil {
		at := queuedAt.UTC()
		j.QueuedAt = &at
	}
	return nil
}

// Dequeue claims the highest-priority eligible job for a runner. Eligibility is
// queued state, not_before reached, and every required label present in the
// runner's set; ordering is priority descending then queued_at ascending. The
// store lock makes the claim atomic, so 50 runners racing on 20 jobs hand out
// each job exactly once.
func (s *Store) Dequeue(_ context.Context, runnerID string, labels []string, ttl time.Duration) (*model.Job, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("mem: Dequeue: empty runner id")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("mem: Dequeue: lease ttl must be positive, got %s", ttl)
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	var eligible []*queueRow
	for _, r := range s.queue {
		if r.leased || r.notBefore.After(now) {
			continue
		}
		if !labelsSatisfied(labels, r.labels) {
			continue
		}
		eligible = append(eligible, r)
	}
	if len(eligible) == 0 {
		return nil, store.ErrNotFound
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].priority != eligible[j].priority {
			return eligible[i].priority > eligible[j].priority
		}
		if !eligible[i].queuedAt.Equal(eligible[j].queuedAt) {
			return eligible[i].queuedAt.Before(eligible[j].queuedAt)
		}
		return eligible[i].jobID < eligible[j].jobID
	})
	row := eligible[0]
	row.leased = true
	row.runnerID = runnerID
	row.leaseExpiresAt = expires

	// The dispatch record is keyed on (run, job, attempt) and created at most
	// once. A redelivery after a lease loss bumps the count; it never becomes a
	// second dispatch.
	dk := dispatchKey{row.runID, row.jobID, row.attempt}
	d, existed := s.dispatches[dk]
	if !existed {
		s.dispatches[dk] = &dispatchRow{runnerID: runnerID, firstAt: now, lastAt: now, dispatchCount: 1}
	} else {
		d.runnerID = runnerID
		d.lastAt = now
		d.dispatchCount++
	}

	j := s.jobs[row.jobID]
	j.Status = model.StatusInProgress
	j.RunnerID = runnerID
	j.LeaseExpiresAt = &expires
	hb := now
	j.LastHeartbeatAt = &hb
	st := now
	j.StartedAt = &st
	j.SetupCompletedAt = nil

	kind, msg := EventDispatched, fmt.Sprintf("Dispatched to runner %s.", runnerID)
	if existed {
		kind = EventRedispatched
		msg = fmt.Sprintf("Redispatched to runner %s after the previous lease was lost.", runnerID)
	}
	s.appendEventLocked(store.Event{
		RunID: row.runID, JobID: row.jobID, Kind: kind, Message: msg,
		Detail: map[string]any{"runner_id": runnerID, "attempt": row.attempt},
		At:     now,
	})
	return cloneJob(j), nil
}

// Heartbeat extends the lease, but only for the runner that still holds it.
func (s *Store) Heartbeat(_ context.Context, runnerID string, jobID int64, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("mem: Heartbeat: lease ttl must be positive, got %s", ttl)
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.queue[jobID]
	if !ok || !row.leased || row.runnerID != runnerID {
		return store.ErrLeaseLost
	}
	row.leaseExpiresAt = expires
	if j, ok := s.jobs[jobID]; ok {
		j.LeaseExpiresAt = &expires
		hb := now
		j.LastHeartbeatAt = &hb
	}
	return nil
}

// ReleaseLease drops a lease without completing the job and puts it back on the
// queue. The reason is validated first: a requeue nobody can explain is the bug
// this signature exists to prevent.
func (s *Store) ReleaseLease(_ context.Context, runnerID string, jobID int64, reason model.CancelReason) error {
	if err := reason.Validate(); err != nil {
		return fmt.Errorf("mem: ReleaseLease: %w", err)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.queue[jobID]
	if !ok || !row.leased || row.runnerID != runnerID {
		return store.ErrLeaseLost
	}
	s.requeueLocked(row)
	s.appendEventLocked(store.Event{
		RunID: row.runID, JobID: row.jobID, Kind: EventRequeued, Message: reason.Sentence,
		Detail: map[string]any{
			"actor":         string(reason.Actor),
			"triggered_by":  reason.TriggeredBy,
			"runner_id":     runnerID,
			"requeue_count": row.requeueCount,
		},
		At: now,
	})
	return nil
}

// requeueLocked puts a leased row back on the queue and syncs the job row.
//
// not_before is left alone: a requeue is immediately dispatchable to another
// runner. Backoff belongs to a retry, which completes the job and enqueues the
// next attempt with its own NotBefore.
func (s *Store) requeueLocked(row *queueRow) {
	row.leased = false
	row.runnerID = ""
	row.leaseExpiresAt = time.Time{}
	row.requeueCount++
	if j, ok := s.jobs[row.jobID]; ok {
		j.Status = model.StatusQueued
		j.RunnerID = ""
		j.LeaseExpiresAt = nil
		j.StartedAt = nil
		j.SetupCompletedAt = nil
		j.RequeueCount = row.requeueCount
	}
}

// ReapExpiredLeases requeues every job whose lease expired and returns them.
// This is what turns "the runner disappeared" into a requeue rather than a lost
// or failed job: the status goes back to queued, never to failed, and the
// requeue is explained in the event log.
func (s *Store) ReapExpiredLeases(_ context.Context, now time.Time) ([]*model.Job, error) {
	now = now.UTC()
	s.mu.Lock()
	var rows []*queueRow
	for _, r := range s.queue {
		if r.leased && !r.leaseExpiresAt.After(now) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].jobID < rows[j].jobID })

	var out []*model.Job
	for _, row := range rows {
		lost := row.runnerID
		s.requeueLocked(row)
		if j, ok := s.jobs[row.jobID]; ok {
			out = append(out, cloneJob(j))
		}
		sentence := fmt.Sprintf(
			"Runner %s stopped reporting, so its lease on this job expired and the job was put "+
				"back on the queue (requeue %d). Nothing your workflow did caused this.",
			lost, row.requeueCount)
		s.appendEventLocked(store.Event{
			RunID: row.runID, JobID: row.jobID, Kind: EventRunnerLost, Message: sentence,
			Detail: map[string]any{
				"actor":         string(model.CancelActorRunnerLost),
				"runner_id":     lost,
				"requeue_count": row.requeueCount,
			},
			At: now,
		})
	}
	s.mu.Unlock()

	for _, j := range out {
		slog.Warn("requeued job after lease expiry",
			"job_id", j.ID, "run_id", j.RunID, "requeue_count", j.RequeueCount)
	}
	return out, nil
}

// QueueStats is the queue page's data and the starvation alarm.
//
// "Online" is idle or busy. A drained runner is deliberately not counted: it
// finishes what it has and takes nothing new, so counting it would hide a
// starving label behind a runner that will never pick the work up.
func (s *Store) QueueStats(_ context.Context, now time.Time) (*store.QueueStats, error) {
	now = now.UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := &store.QueueStats{
		DepthByLabel:   map[string]int{},
		RunnersByLabel: map[string]int{},
		IdleByLabel:    map[string]int{},
		At:             now,
	}
	var oldest time.Time
	for _, r := range s.queue {
		if r.leased {
			continue
		}
		out.Depth++
		for _, l := range r.labels {
			out.DepthByLabel[l]++
		}
		if oldest.IsZero() || r.queuedAt.Before(oldest) {
			oldest = r.queuedAt
			out.OldestJobID = r.jobID
		}
	}
	if !oldest.IsZero() {
		if d := now.Sub(oldest); d > 0 {
			out.OldestWaiting = d
		}
	}
	for _, r := range s.runners {
		if r.State != model.RunnerIdle && r.State != model.RunnerBusy {
			continue
		}
		for _, l := range r.Labels {
			out.RunnersByLabel[l]++
			if r.State == model.RunnerIdle {
				out.IdleByLabel[l]++
			}
		}
	}
	for label, depth := range out.DepthByLabel {
		if depth > 0 && out.RunnersByLabel[label] == 0 {
			out.StarvedLabels = append(out.StarvedLabels, label)
		}
	}
	sort.Strings(out.StarvedLabels)
	return out, nil
}

func (s *Store) RecordQueueSample(_ context.Context, sample store.QueueSample) error {
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, cloneQueueSample(sample))
	return nil
}

func (s *Store) QueueDepthHistory(_ context.Context, since time.Time) ([]store.QueueSample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.QueueSample
	for _, sample := range s.samples {
		if !sample.At.Before(since) {
			out = append(out, cloneQueueSample(sample))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
