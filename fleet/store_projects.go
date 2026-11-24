package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RegisterProject validates p against its repository and stores it.
//
// The path must be inside a git repository; it is resolved to the
// repository's top level. Empty fields are filled in: ID and Name from the
// directory, Remote and DefaultBranch from the repository, and the rest from
// the package defaults. The default branch must exist. A trust mode that
// pushes requires a remote.
func (s *Store) RegisterProject(ctx context.Context, p Project) (Project, error) {
	if err := s.prepareProject(ctx, &p); err != nil {
		return Project{}, err
	}
	now := s.now()
	p.CreatedAt, p.UpdatedAt = now.UTC(), now.UTC()

	commands, err := json.Marshal(p.VerifyCommands)
	if err != nil {
		return Project{}, fmt.Errorf("fleet: encode verify commands: %w", err)
	}
	budget, err := json.Marshal(p.Budget)
	if err != nil {
		return Project{}, fmt.Errorf("fleet: encode budget: %w", err)
	}
	err = s.inTx(ctx, func(tx querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_projects
			(id, name, repo_path, remote, default_branch, trust_mode, verify_commands,
			 max_concurrent, branch_prefix, budget, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.Name, p.RepoPath, p.Remote, p.DefaultBranch, string(p.TrustMode), string(commands),
			p.MaxConcurrent, p.BranchPrefix, string(budget), toMillis(now), toMillis(now))
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s (%s)", ErrProjectExists, p.ID, p.RepoPath)
		}
		if err != nil {
			return fmt.Errorf("fleet: insert project: %w", err)
		}
		return replaceBindings(ctx, tx, p.ID, p.Bindings)
	})
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// prepareProject resolves the repository, applies defaults and validates.
func (s *Store) prepareProject(ctx context.Context, p *Project) error {
	if err := resolveRepo(ctx, p); err != nil {
		return err
	}
	if err := p.normalize(); err != nil {
		return err
	}
	if err := checkBranchPrefix(ctx, p.RepoPath, p.BranchPrefix); err != nil {
		return err
	}
	if p.TrustMode.NeedsRemote() && p.Remote == "" {
		return fmt.Errorf("%w: trust mode %s pushes, but the repository has no remote", ErrInvalidProject, p.TrustMode)
	}
	return nil
}

const projectColumns = `id, name, repo_path, remote, default_branch, trust_mode, verify_commands,
	max_concurrent, branch_prefix, budget, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var (
		p                       Project
		trust, commands, budget string
		created, updated        int64
	)
	err := row.Scan(&p.ID, &p.Name, &p.RepoPath, &p.Remote, &p.DefaultBranch, &trust, &commands,
		&p.MaxConcurrent, &p.BranchPrefix, &budget, &created, &updated)
	if err != nil {
		return Project{}, err
	}
	if err := json.Unmarshal([]byte(budget), &p.Budget); err != nil {
		return Project{}, fmt.Errorf("fleet: decode budget of %s: %w", p.ID, err)
	}
	p.TrustMode = TrustMode(trust)
	if err := json.Unmarshal([]byte(commands), &p.VerifyCommands); err != nil {
		return Project{}, fmt.Errorf("fleet: decode verify commands of %s: %w", p.ID, err)
	}
	if p.VerifyCommands == nil {
		p.VerifyCommands = []string{}
	}
	p.CreatedAt, p.UpdatedAt = fromMillis(created), fromMillis(updated)
	return p, nil
}

func getProject(ctx context.Context, q querier, id string) (Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM fleet_projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: %s", ErrProjectNotFound, id)
	}
	if err != nil {
		return Project{}, fmt.Errorf("fleet: load project: %w", err)
	}
	if p.Bindings, err = loadBindings(ctx, q, id); err != nil {
		return Project{}, err
	}
	return p, nil
}

// GetProject returns the project with the given ID.
func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	return getProject(ctx, s.db, id)
}

// GetProjectByRepoPath returns the project registered for a repository path.
// The path must be the repository's top level, as stored at registration.
func (s *Store) GetProjectByRepoPath(ctx context.Context, repoPath string) (Project, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM fleet_projects WHERE repo_path = ?`, repoPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: %s", ErrProjectNotFound, repoPath)
	}
	if err != nil {
		return Project{}, fmt.Errorf("fleet: load project: %w", err)
	}
	return s.GetProject(ctx, id)
}

// ListProjects returns every project, ordered by ID.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	return s.queryProjects(ctx, `SELECT `+projectColumns+` FROM fleet_projects ORDER BY id`)
}

// ProjectsByBinding returns the projects that carry the given key/value
// binding, ordered by ID. Use it to find, say, the project bound to a chat
// topic.
func (s *Store) ProjectsByBinding(ctx context.Context, key, value string) ([]Project, error) {
	return s.queryProjects(ctx, `SELECT `+projectColumns+` FROM fleet_projects
		WHERE id IN (SELECT project_id FROM fleet_project_bindings WHERE key = ? AND value = ?)
		ORDER BY id`, key, value)
}

func (s *Store) queryProjects(ctx context.Context, query string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet: list projects: %w", err)
	}
	projects := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("fleet: list projects: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("fleet: list projects: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list projects: %w", err)
	}
	for i := range projects {
		if projects[i].Bindings, err = loadBindings(ctx, s.db, projects[i].ID); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// UpdateProject applies change to the stored project and saves the result
// after validating it the same way registration does. The ID cannot change.
// Returning an error from change aborts the update.
//
// Changing TrustMode affects only tasks dispatched afterwards.
func (s *Store) UpdateProject(ctx context.Context, id string, change func(*Project) error) (Project, error) {
	current, err := s.GetProject(ctx, id)
	if err != nil {
		return Project{}, err
	}
	updated := current
	updated.VerifyCommands = append([]string(nil), current.VerifyCommands...)
	updated.Bindings = make(map[string]string, len(current.Bindings))
	for k, v := range current.Bindings {
		updated.Bindings[k] = v
	}
	if err := change(&updated); err != nil {
		return Project{}, err
	}
	if updated.ID != id {
		return Project{}, fmt.Errorf("%w: project ID cannot change (%s -> %s)", ErrInvalidProject, id, updated.ID)
	}
	if err := s.prepareProject(ctx, &updated); err != nil {
		return Project{}, err
	}
	now := s.now()
	updated.CreatedAt, updated.UpdatedAt = current.CreatedAt, now.UTC()

	commands, err := json.Marshal(updated.VerifyCommands)
	if err != nil {
		return Project{}, fmt.Errorf("fleet: encode verify commands: %w", err)
	}
	budget, err := json.Marshal(updated.Budget)
	if err != nil {
		return Project{}, fmt.Errorf("fleet: encode budget: %w", err)
	}
	err = s.inTx(ctx, func(tx querier) error {
		res, err := tx.ExecContext(ctx, `UPDATE fleet_projects SET
			name = ?, repo_path = ?, remote = ?, default_branch = ?, trust_mode = ?,
			verify_commands = ?, max_concurrent = ?, branch_prefix = ?, budget = ?, updated_at = ?
			WHERE id = ?`,
			updated.Name, updated.RepoPath, updated.Remote, updated.DefaultBranch, string(updated.TrustMode),
			string(commands), updated.MaxConcurrent, updated.BranchPrefix, string(budget), toMillis(now), id)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrProjectExists, updated.RepoPath)
		}
		if err != nil {
			return fmt.Errorf("fleet: update project: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrProjectNotFound, id)
		}
		return replaceBindings(ctx, tx, id, updated.Bindings)
	})
	if err != nil {
		return Project{}, err
	}
	return updated, nil
}

// DeleteProject removes a project. A project that has any task, finished or
// not, is refused with ErrProjectHasTasks so task history is never orphaned.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx querier) error {
		var tasks int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleet_tasks WHERE project_id = ?`, id).Scan(&tasks); err != nil {
			return fmt.Errorf("fleet: count project tasks: %w", err)
		}
		if tasks > 0 {
			return fmt.Errorf("%w: %s has %d", ErrProjectHasTasks, id, tasks)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM fleet_projects WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("fleet: delete project: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrProjectNotFound, id)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_project_bindings WHERE project_id = ?`, id); err != nil {
			return fmt.Errorf("fleet: delete project bindings: %w", err)
		}
		return nil
	})
}

// SetBinding attaches or replaces one key/value binding on a project.
func (s *Store) SetBinding(ctx context.Context, projectID, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("%w: binding key must not be empty", ErrInvalidProject)
	}
	return s.inTx(ctx, func(tx querier) error {
		if err := projectExists(ctx, tx, projectID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_project_bindings (project_id, key, value)
			VALUES (?, ?, ?)
			ON CONFLICT (project_id, key) DO UPDATE SET value = excluded.value`, projectID, key, value)
		if err != nil {
			return fmt.Errorf("fleet: set binding: %w", err)
		}
		return nil
	})
}

// DeleteBinding removes one binding. Removing a binding that is not set is
// not an error.
func (s *Store) DeleteBinding(ctx context.Context, projectID, key string) error {
	return s.inTx(ctx, func(tx querier) error {
		if err := projectExists(ctx, tx, projectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_project_bindings WHERE project_id = ? AND key = ?`,
			projectID, strings.TrimSpace(key)); err != nil {
			return fmt.Errorf("fleet: delete binding: %w", err)
		}
		return nil
	})
}

func projectExists(ctx context.Context, q querier, id string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM fleet_projects WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("fleet: load project: %w", err)
	}
	return nil
}

func loadBindings(ctx context.Context, q querier, projectID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM fleet_project_bindings WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("fleet: load bindings: %w", err)
	}
	defer rows.Close()
	bindings := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("fleet: load bindings: %w", err)
		}
		bindings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: load bindings: %w", err)
	}
	return bindings, nil
}

func replaceBindings(ctx context.Context, tx querier, projectID string, bindings map[string]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_project_bindings WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("fleet: replace bindings: %w", err)
	}
	keys := make([]string, 0, len(bindings))
	for k := range bindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_project_bindings (project_id, key, value) VALUES (?, ?, ?)`,
			projectID, k, bindings[k]); err != nil {
			return fmt.Errorf("fleet: replace bindings: %w", err)
		}
	}
	return nil
}
