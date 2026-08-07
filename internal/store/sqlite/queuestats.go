package sqlite

// What the queue page reads: current depth by label, who could take the work,
// and the sampled history behind the graph.

import (
	"context"
	"sort"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// QueueStats is the queue page's data and the starvation alarm.
//
// "Online" is idle or busy. A drained runner is deliberately not counted: it
// finishes what it has and takes nothing new, so counting it would hide a
// starving label behind a runner that will never pick the work up.
func (s *Store) QueueStats(ctx context.Context, now time.Time) (*store.QueueStats, error) {
	now = now.UTC()
	out := &store.QueueStats{
		DepthByLabel:   map[string]int{},
		RunnersByLabel: map[string]int{},
		IdleByLabel:    map[string]int{},
		At:             now,
	}

	const depthQ = `SELECT job_id, labels, queued_at FROM job_queue WHERE state = 'queued'`
	rows, err := s.db.QueryContext(ctx, depthQ)
	if err != nil {
		return nil, mapErr("sqlite: QueueStats", err)
	}
	var oldest time.Time
	for rows.Next() {
		var jobID int64
		var rawLabels, rawQueuedAt string
		if err := rows.Scan(&jobID, &rawLabels, &rawQueuedAt); err != nil {
			rows.Close()
			return nil, mapErr("sqlite: QueueStats", err)
		}
		var labels []string
		if err := jsonInto(rawLabels, &labels); err != nil {
			rows.Close()
			return nil, err
		}
		queuedAt, err := mustTime(rawQueuedAt)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out.Depth++
		for _, l := range labels {
			out.DepthByLabel[l]++
		}
		if oldest.IsZero() || queuedAt.Before(oldest) {
			oldest = queuedAt
			out.OldestJobID = jobID
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapErr("sqlite: QueueStats", err)
	}
	if !oldest.IsZero() {
		if d := now.Sub(oldest); d > 0 {
			out.OldestWaiting = d
		}
	}

	const runnerQ = `SELECT labels, state FROM runners WHERE state IN ('idle', 'busy')`
	rrows, err := s.db.QueryContext(ctx, runnerQ)
	if err != nil {
		return nil, mapErr("sqlite: QueueStats", err)
	}
	for rrows.Next() {
		var rawLabels, state string
		if err := rrows.Scan(&rawLabels, &state); err != nil {
			rrows.Close()
			return nil, mapErr("sqlite: QueueStats", err)
		}
		var labels []string
		if err := jsonInto(rawLabels, &labels); err != nil {
			rrows.Close()
			return nil, err
		}
		for _, l := range labels {
			out.RunnersByLabel[l]++
			if state == string(model.RunnerIdle) {
				out.IdleByLabel[l]++
			}
		}
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, mapErr("sqlite: QueueStats", err)
	}

	for label, depth := range out.DepthByLabel {
		if depth > 0 && out.RunnersByLabel[label] == 0 {
			out.StarvedLabels = append(out.StarvedLabels, label)
		}
	}
	sort.Strings(out.StarvedLabels)
	return out, nil
}

func (s *Store) RecordQueueSample(ctx context.Context, sample store.QueueSample) error {
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	byLabel, err := jsonText(sample.DepthByLabel)
	if err != nil {
		return err
	}
	const q = `INSERT INTO queue_samples (at, depth, depth_by_label, busy, idle)
	           VALUES (?,?,?,?,?)`
	_, err = s.db.ExecContext(ctx, q, ts(sample.At), sample.Depth, byLabel, sample.Busy, sample.Idle)
	return mapErr("sqlite: RecordQueueSample", err)
}

func (s *Store) QueueDepthHistory(ctx context.Context, since time.Time) ([]store.QueueSample, error) {
	const q = `SELECT at, depth, depth_by_label, busy, idle FROM queue_samples
	           WHERE at >= ? ORDER BY at, id`
	rows, err := s.db.QueryContext(ctx, q, ts(since))
	if err != nil {
		return nil, mapErr("sqlite: QueueDepthHistory", err)
	}
	defer rows.Close()
	var out []store.QueueSample
	for rows.Next() {
		var sample store.QueueSample
		var at, byLabel string
		if err := rows.Scan(&at, &sample.Depth, &byLabel, &sample.Busy, &sample.Idle); err != nil {
			return nil, mapErr("sqlite: QueueDepthHistory", err)
		}
		if sample.At, err = mustTime(at); err != nil {
			return nil, err
		}
		if err := jsonInto(byLabel, &sample.DepthByLabel); err != nil {
			return nil, err
		}
		sample.DepthByLabel = emptyMapToNil(sample.DepthByLabel)
		out = append(out, sample)
	}
	return out, mapErr("sqlite: QueueDepthHistory", rows.Err())
}
