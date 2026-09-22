// Package members is the gateway's own ownership table: which (org, repo) a
// tenant maintains. A tenant sees ONLY its own rows, so visibility is exactly
// membership. Standalone (free) sessions are also recorded here, keyed by
// their session name with an empty org/repo.
package members

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Store is the ownership table.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the sqlite store at dsn.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS org_owners (
		tenant TEXT NOT NULL,
		org    TEXT NOT NULL,
		PRIMARY KEY (tenant, org)
	)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS repo_owners (
		tenant TEXT NOT NULL,
		org    TEXT NOT NULL,
		repo   TEXT NOT NULL,
		PRIMARY KEY (tenant, org, repo)
	)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS free_sessions (
		tenant  TEXT NOT NULL,
		session TEXT NOT NULL,
		role    TEXT NOT NULL,
		PRIMARY KEY (tenant, session)
	)`); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// AddRepo records that tenant maintains org/repo (idempotent).
func (s *Store) AddRepo(tenant, org, repo string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO repo_owners (tenant, org, repo) VALUES (?,?,?)`,
		tenant, org, repo,
	)
	return err
}

// OwnsRepo reports whether tenant maintains org/repo.
func (s *Store) OwnsRepo(tenant, org, repo string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM repo_owners WHERE tenant=? AND org=? AND repo=?`,
		tenant, org, repo,
	).Scan(&n)
	return n > 0, err
}

// AddOrg records that tenant owns org (idempotent).
func (s *Store) AddOrg(tenant, org string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO org_owners (tenant, org) VALUES (?,?)`,
		tenant, org,
	)
	return err
}

// OwnsOrg reports whether tenant owns org.
func (s *Store) OwnsOrg(tenant, org string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM org_owners WHERE tenant=? AND org=?`,
		tenant, org,
	).Scan(&n)
	return n > 0, err
}

// ListOrgs returns every org tenant owns.
func (s *Store) ListOrgs(tenant string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT org FROM org_owners WHERE tenant=? ORDER BY org`, tenant,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, err
		}
		out = append(out, org)
	}
	return out, rows.Err()
}

// ListRepos returns every (org, repo) tenant maintains.
func (s *Store) ListRepos(tenant string) ([][2]string, error) {
	rows, err := s.db.Query(
		`SELECT org, repo FROM repo_owners WHERE tenant=? ORDER BY org, repo`, tenant,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var org, repo string
		if err := rows.Scan(&org, &repo); err != nil {
			return nil, err
		}
		out = append(out, [2]string{org, repo})
	}
	return out, rows.Err()
}

// AddFreeSession records a standalone session's role (admin/explorer).
func (s *Store) AddFreeSession(tenant, session, role string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO free_sessions (tenant, session, role) VALUES (?,?,?)`,
		tenant, session, role,
	)
	return err
}

// FreeRole returns the role of a standalone session, ok=false when unknown.
func (s *Store) FreeRole(tenant, session string) (string, bool, error) {
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM free_sessions WHERE tenant=? AND session=?`, tenant, session,
	).Scan(&role)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

// DeleteFreeSession removes a standalone session row.
func (s *Store) DeleteFreeSession(tenant, session string) error {
	_, err := s.db.Exec(`DELETE FROM free_sessions WHERE tenant=? AND session=?`, tenant, session)
	return err
}

// FreeSessions returns the tenant's standalone sessions.
func (s *Store) FreeSessions(tenant string) ([][2]string, error) {
	rows, err := s.db.Query(
		`SELECT session, role FROM free_sessions WHERE tenant=? ORDER BY session`, tenant,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var sess, role string
		if err := rows.Scan(&sess, &role); err != nil {
			return nil, err
		}
		out = append(out, [2]string{sess, role})
	}
	return out, rows.Err()
}
