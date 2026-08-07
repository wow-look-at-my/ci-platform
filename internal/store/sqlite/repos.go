package sqlite

import (
	"context"
	"fmt"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

const repoCols = `id, owner, name, installation_id, default_branch, private`

// UpsertRepo inserts or replaces a repository. The id is GitHub's, so the
// caller must supply it; a zero id is a programming error, not a request to
// allocate one.
func (s *Store) UpsertRepo(ctx context.Context, r *model.Repo) error {
	if r == nil {
		return fmt.Errorf("sqlite: UpsertRepo: nil repo")
	}
	if r.ID == 0 {
		return fmt.Errorf("sqlite: UpsertRepo: repo %q has no id", r.Owner+"/"+r.Name)
	}
	const q = `
INSERT INTO repos (id, owner, name, installation_id, default_branch, private)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    owner = excluded.owner,
    name = excluded.name,
    installation_id = excluded.installation_id,
    default_branch = excluded.default_branch,
    private = excluded.private`
	_, err := s.db.ExecContext(ctx, q, r.ID, r.Owner, r.Name, r.InstallationID, r.DefaultBranch, boolInt(r.Private))
	return mapErr("sqlite: UpsertRepo", err)
}

func (s *Store) GetRepo(ctx context.Context, id int64) (*model.Repo, error) {
	var r model.Repo
	err := s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repos WHERE id = ?`, id).
		Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private)
	if err != nil {
		return nil, mapErr("sqlite: GetRepo", err)
	}
	return &r, nil
}

func (s *Store) GetRepoByName(ctx context.Context, owner, name string) (*model.Repo, error) {
	var r model.Repo
	err := s.db.QueryRowContext(ctx,
		`SELECT `+repoCols+` FROM repos WHERE owner = ? AND name = ?`, owner, name).
		Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private)
	if err != nil {
		return nil, mapErr("sqlite: GetRepoByName", err)
	}
	return &r, nil
}

func (s *Store) ListRepos(ctx context.Context) ([]*model.Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoCols+` FROM repos ORDER BY owner, name`)
	if err != nil {
		return nil, mapErr("sqlite: ListRepos", err)
	}
	defer rows.Close()
	var out []*model.Repo
	for rows.Next() {
		var r model.Repo
		if err := rows.Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private); err != nil {
			return nil, mapErr("sqlite: ListRepos", err)
		}
		out = append(out, &r)
	}
	return out, mapErr("sqlite: ListRepos", rows.Err())
}
