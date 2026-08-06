package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const runnerCols = `id, name, labels, runner_group, state, current_job_id, capacity, version,
	os, arch, first_seen_at, last_heartbeat`

func scanRunner(row pgx.Row) (*model.Runner, error) {
	var r model.Runner
	if err := row.Scan(&r.ID, &r.Name, &r.Labels, &r.Group, &r.State, &r.CurrentJobID,
		&r.Capacity, &r.Version, &r.OS, &r.Arch, &r.FirstSeenAt, &r.LastHeartbeat); err != nil {
		return nil, err
	}
	r.Labels = emptyToNil(r.Labels)
	r.FirstSeenAt = r.FirstSeenAt.UTC()
	r.LastHeartbeat = r.LastHeartbeat.UTC()
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
		return fmt.Errorf("pg: RegisterRunner: nil runner")
	}
	if r.ID == "" {
		return fmt.Errorf("pg: RegisterRunner: runner has no id")
	}
	if !validRunnerState(r.State) {
		return fmt.Errorf("pg: RegisterRunner: invalid state %q", r.State)
	}
	const q = `
INSERT INTO runners (id, name, labels, runner_group, state, current_job_id, capacity, version,
	os, arch, first_seen_at, last_heartbeat)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (id) DO UPDATE SET
	name = EXCLUDED.name, labels = EXCLUDED.labels, runner_group = EXCLUDED.runner_group,
	state = EXCLUDED.state, current_job_id = EXCLUDED.current_job_id,
	capacity = EXCLUDED.capacity, version = EXCLUDED.version, os = EXCLUDED.os,
	arch = EXCLUDED.arch, last_heartbeat = EXCLUDED.last_heartbeat`
	_, err := s.pool.Exec(ctx, q, r.ID, r.Name, nonNilStrings(r.Labels), r.Group, string(r.State),
		r.CurrentJobID, r.Capacity, r.Version, r.OS, r.Arch, utc(r.FirstSeenAt), utc(r.LastHeartbeat))
	return mapErr("pg: RegisterRunner", err)
}

// RunnerHeartbeat refreshes liveness and brings a runner back from offline: a
// heartbeat is proof it is not offline.
func (s *Store) RunnerHeartbeat(ctx context.Context, id string, at time.Time) error {
	const q = `
UPDATE runners
SET last_heartbeat = $2,
    state = CASE WHEN state = 'offline' THEN 'idle' ELSE state END
WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, utc(at))
	if err != nil {
		return mapErr("pg: RunnerHeartbeat", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetRunner(ctx context.Context, id string) (*model.Runner, error) {
	r, err := scanRunner(s.pool.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr("pg: GetRunner", err)
	}
	return r, nil
}

func (s *Store) ListRunners(ctx context.Context) ([]*model.Runner, error) {
	return s.queryRunners(ctx, "pg: ListRunners", `SELECT `+runnerCols+` FROM runners ORDER BY id`)
}

// MarkOfflineRunners flips stale runners offline and returns them so their
// in-flight jobs can be requeued with a recorded reason.
func (s *Store) MarkOfflineRunners(ctx context.Context, deadline time.Time) ([]*model.Runner, error) {
	const q = `
UPDATE runners SET state = 'offline'
WHERE last_heartbeat < $1 AND state <> 'offline'
RETURNING ` + runnerCols
	return s.queryRunners(ctx, "pg: MarkOfflineRunners", q, utc(deadline))
}

func (s *Store) queryRunners(ctx context.Context, op, q string, args ...any) ([]*model.Runner, error) {
	rows, err := s.pool.Query(ctx, q, args...)
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
