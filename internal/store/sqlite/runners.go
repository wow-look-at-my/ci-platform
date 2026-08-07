package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const runnerCols = `id, name, labels, runner_group, state, current_job_id, capacity, version,
	os, arch, first_seen_at, last_heartbeat`

func scanRunner(row scanner) (*model.Runner, error) {
	var r model.Runner
	var labels, firstSeen, lastHeartbeat string
	if err := row.Scan(&r.ID, &r.Name, &labels, &r.Group, &r.State, &r.CurrentJobID,
		&r.Capacity, &r.Version, &r.OS, &r.Arch, &firstSeen, &lastHeartbeat); err != nil {
		return nil, err
	}
	if err := jsonInto(labels, &r.Labels); err != nil {
		return nil, err
	}
	r.Labels = emptyToNil(r.Labels)

	var err error
	if r.FirstSeenAt, err = mustTime(firstSeen); err != nil {
		return nil, err
	}
	if r.LastHeartbeat, err = mustTime(lastHeartbeat); err != nil {
		return nil, err
	}
	return &r, nil
}

func validRunnerState(s model.RunnerState) bool {
	switch s {
	case model.RunnerIdle, model.RunnerBusy, model.RunnerOffline, model.RunnerDrained:
		return true
	}
	return false
}

// RegisterRunner records or refreshes an agent. FirstSeenAt survives
// re-registration; a restarting agent is the same host.
func (s *Store) RegisterRunner(ctx context.Context, r *model.Runner) error {
	if r == nil {
		return fmt.Errorf("sqlite: RegisterRunner: nil runner")
	}
	if r.ID == "" {
		return fmt.Errorf("sqlite: RegisterRunner: runner has no id")
	}
	if !validRunnerState(r.State) {
		return fmt.Errorf("sqlite: RegisterRunner: invalid state %q", r.State)
	}
	labels, err := jsonText(r.Labels)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO runners (id, name, labels, runner_group, state, current_job_id, capacity, version,
	os, arch, first_seen_at, last_heartbeat)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (id) DO UPDATE SET
	name = excluded.name, labels = excluded.labels, runner_group = excluded.runner_group,
	state = excluded.state, current_job_id = excluded.current_job_id,
	capacity = excluded.capacity, version = excluded.version, os = excluded.os,
	arch = excluded.arch, last_heartbeat = excluded.last_heartbeat`
	_, err = s.db.ExecContext(ctx, q, r.ID, r.Name, labels, r.Group, string(r.State),
		r.CurrentJobID, r.Capacity, r.Version, r.OS, r.Arch, ts(r.FirstSeenAt), ts(r.LastHeartbeat))
	return mapErr("sqlite: RegisterRunner", err)
}

// RunnerHeartbeat refreshes liveness and brings a runner back from offline: a
// heartbeat is proof it is not offline.
func (s *Store) RunnerHeartbeat(ctx context.Context, id string, at time.Time) error {
	const q = `
UPDATE runners
SET last_heartbeat = ?,
    state = CASE WHEN state = 'offline' THEN 'idle' ELSE state END
WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, ts(at), id)
	if err != nil {
		return mapErr("sqlite: RunnerHeartbeat", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr("sqlite: RunnerHeartbeat", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetRunner(ctx context.Context, id string) (*model.Runner, error) {
	r, err := scanRunner(s.db.QueryRowContext(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr("sqlite: GetRunner", err)
	}
	return r, nil
}

func (s *Store) ListRunners(ctx context.Context) ([]*model.Runner, error) {
	return s.queryRunners(ctx, "sqlite: ListRunners", `SELECT `+runnerCols+` FROM runners ORDER BY id`)
}

// MarkOfflineRunners flips stale runners offline and returns them so their
// in-flight jobs can be requeued with a recorded reason.
func (s *Store) MarkOfflineRunners(ctx context.Context, deadline time.Time) ([]*model.Runner, error) {
	const q = `
UPDATE runners SET state = 'offline'
WHERE last_heartbeat < ? AND state <> 'offline'
RETURNING ` + runnerCols
	return s.queryRunners(ctx, "sqlite: MarkOfflineRunners", q, ts(deadline))
}

func (s *Store) queryRunners(ctx context.Context, op, q string, args ...any) ([]*model.Runner, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(op, err)
	}
	defer rows.Close()
	var out []*model.Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, mapErr(op, err)
		}
		out = append(out, r)
	}
	return out, mapErr(op, rows.Err())
}
