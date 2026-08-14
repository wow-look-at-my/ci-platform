package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

const runnerHostCols = `fingerprint, public_key, state, name, os, arch, version, labels,
	enrolled_from, enrolled_at, approved_by, approved_at, last_seen_at, note`

func scanRunnerHost(row scanner) (*model.RunnerHost, error) {
	var h model.RunnerHost
	var labels, enrolledAt, approvedAt, lastSeenAt string
	if err := row.Scan(&h.Fingerprint, &h.PublicKey, &h.State, &h.Name, &h.OS, &h.Arch, &h.Version,
		&labels, &h.EnrolledFrom, &enrolledAt, &h.ApprovedBy, &approvedAt, &lastSeenAt, &h.Note); err != nil {
		return nil, err
	}
	if err := jsonInto(labels, &h.Labels); err != nil {
		return nil, err
	}
	h.Labels = emptyToNil(h.Labels)

	var err error
	if h.EnrolledAt, err = mustTime(enrolledAt); err != nil {
		return nil, err
	}
	if h.ApprovedAt, err = mustTime(approvedAt); err != nil {
		return nil, err
	}
	if h.LastSeenAt, err = mustTime(lastSeenAt); err != nil {
		return nil, err
	}
	return &h, nil
}

func validRunnerHostState(s model.RunnerHostState) bool {
	switch s {
	case model.RunnerHostPending, model.RunnerHostApproved, model.RunnerHostRevoked:
		return true
	}
	return false
}

// EnrolRunnerHost records a host the first time it is seen and is a no-op
// afterwards.
//
// The no-op is the point. If re-enrolling could write, a host that had been
// revoked could clear its own state by restarting, and an approved fingerprint
// could have a different public key written behind it. Both are silent
// escalations, so an existing row is returned untouched.
func (s *Store) EnrolRunnerHost(ctx context.Context, h *model.RunnerHost) (*model.RunnerHost, error) {
	if h == nil || h.Fingerprint == "" {
		return nil, fmt.Errorf("sqlite: EnrolRunnerHost: host has no fingerprint")
	}
	if existing, err := s.GetRunnerHost(ctx, h.Fingerprint); err == nil {
		return existing, nil
	} else if err != store.ErrNotFound {
		return nil, err
	}

	labels, err := jsonText(h.Labels)
	if err != nil {
		return nil, err
	}
	const q = `INSERT INTO runner_hosts (` + runnerHostCols + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', '')`
	_, err = s.db.ExecContext(ctx, q, h.Fingerprint, h.PublicKey, string(model.RunnerHostPending),
		h.Name, h.OS, h.Arch, h.Version, labels, h.EnrolledFrom, ts(h.EnrolledAt))
	if err != nil {
		return nil, mapErr("sqlite: EnrolRunnerHost", err)
	}
	return s.GetRunnerHost(ctx, h.Fingerprint)
}

// GetRunnerHost reads one host by fingerprint.
func (s *Store) GetRunnerHost(ctx context.Context, fingerprint string) (*model.RunnerHost, error) {
	const q = `SELECT ` + runnerHostCols + ` FROM runner_hosts WHERE fingerprint = ?`
	h, err := scanRunnerHost(s.db.QueryRowContext(ctx, q, fingerprint))
	if err != nil {
		return nil, mapErr("sqlite: GetRunnerHost", err)
	}
	return h, nil
}

// ListRunnerHosts returns every host, pending first: the pending ones are the
// list an operator is being asked to act on.
func (s *Store) ListRunnerHosts(ctx context.Context) ([]*model.RunnerHost, error) {
	const q = `SELECT ` + runnerHostCols + ` FROM runner_hosts
ORDER BY CASE state WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 ELSE 2 END, enrolled_at`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, mapErr("sqlite: ListRunnerHosts", err)
	}
	defer rows.Close()

	var out []*model.RunnerHost
	for rows.Next() {
		h, err := scanRunnerHost(rows)
		if err != nil {
			return nil, mapErr("sqlite: ListRunnerHosts", err)
		}
		out = append(out, h)
	}
	return out, mapErr("sqlite: ListRunnerHosts", rows.Err())
}

// SetRunnerHostState approves or revokes a host.
func (s *Store) SetRunnerHostState(ctx context.Context, fingerprint string, state model.RunnerHostState, by, note string, at time.Time) (*model.RunnerHost, error) {
	if !validRunnerHostState(state) {
		return nil, fmt.Errorf("sqlite: SetRunnerHostState: invalid state %q", state)
	}
	// Approval is what hands out access, so it is the one transition that
	// cannot be anonymous.
	if state == model.RunnerHostApproved && by == "" {
		return nil, fmt.Errorf("sqlite: SetRunnerHostState: approving a host must name who approved it")
	}
	// Approved_by names who granted access, so it is cleared the moment access
	// is not granted. A field that keeps saying "approved by X" about a revoked
	// host is a field that lies to whoever reads the list next.
	approvedBy, approvedAt := "", ""
	if state == model.RunnerHostApproved {
		approvedBy, approvedAt = by, ts(at)
	}
	const q = `UPDATE runner_hosts SET state = ?, approved_by = ?, approved_at = ?, note = ?
WHERE fingerprint = ?`
	res, err := s.db.ExecContext(ctx, q, string(state), approvedBy, approvedAt, note, fingerprint)
	if err != nil {
		return nil, mapErr("sqlite: SetRunnerHostState", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetRunnerHost(ctx, fingerprint)
}

// SetRunnerHostLabels replaces what this host's runners may claim.
func (s *Store) SetRunnerHostLabels(ctx context.Context, fingerprint string, labels []string) (*model.RunnerHost, error) {
	encoded, err := jsonText(labels)
	if err != nil {
		return nil, err
	}
	const q = `UPDATE runner_hosts SET labels = ? WHERE fingerprint = ?`
	res, err := s.db.ExecContext(ctx, q, encoded, fingerprint)
	if err != nil {
		return nil, mapErr("sqlite: SetRunnerHostLabels", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetRunnerHost(ctx, fingerprint)
}

// TouchRunnerHost records that the host was seen.
func (s *Store) TouchRunnerHost(ctx context.Context, fingerprint string, at time.Time) error {
	const q = `UPDATE runner_hosts SET last_seen_at = ? WHERE fingerprint = ?`
	res, err := s.db.ExecContext(ctx, q, ts(at), fingerprint)
	if err != nil {
		return mapErr("sqlite: TouchRunnerHost", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}
