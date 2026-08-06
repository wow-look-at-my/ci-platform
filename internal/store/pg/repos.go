package pg

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
		return fmt.Errorf("pg: UpsertRepo: nil repo")
	}
	if r.ID == 0 {
		return fmt.Errorf("pg: UpsertRepo: repo %q has no id", r.Owner+"/"+r.Name)
	}
	const q = `
INSERT INTO repos (id, owner, name, installation_id, default_branch, private)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
    owner = EXCLUDED.owner,
    name = EXCLUDED.name,
    installation_id = EXCLUDED.installation_id,
    default_branch = EXCLUDED.default_branch,
    private = EXCLUDED.private`
	_, err := s.pool.Exec(ctx, q, r.ID, r.Owner, r.Name, r.InstallationID, r.DefaultBranch, r.Private)
	return mapErr("pg: UpsertRepo", err)
}

func (s *Store) GetRepo(ctx context.Context, id int64) (*model.Repo, error) {
	var r model.Repo
	err := s.pool.QueryRow(ctx, `SELECT `+repoCols+` FROM repos WHERE id = $1`, id).
		Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private)
	if err != nil {
		return nil, mapErr("pg: GetRepo", err)
	}
	return &r, nil
}

func (s *Store) GetRepoByName(ctx context.Context, owner, name string) (*model.Repo, error) {
	var r model.Repo
	err := s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE owner = $1 AND name = $2`, owner, name).
		Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private)
	if err != nil {
		return nil, mapErr("pg: GetRepoByName", err)
	}
	return &r, nil
}

func (s *Store) ListRepos(ctx context.Context) ([]*model.Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+repoCols+` FROM repos ORDER BY owner, name`)
	if err != nil {
		return nil, mapErr("pg: ListRepos", err)
	}
	defer rows.Close()
	var out []*model.Repo
	for rows.Next() {
		var r model.Repo
		if err := rows.Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.DefaultBranch, &r.Private); err != nil {
			return nil, mapErr("pg: ListRepos", err)
		}
		out = append(out, &r)
	}
	return out, mapErr("pg: ListRepos", rows.Err())
}
