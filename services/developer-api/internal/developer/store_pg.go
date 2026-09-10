package developer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/banzami/banzami/services/common/env"
)

// pgStore is the Postgres-backed developer Store (developer.* schema).
type pgStore struct {
	pool *pgxpool.Pool
	// environment tags rows this store creates in environment-scoped tables.
	// It is process configuration, not caller input: a Console user chooses what
	// to build, never which financial universe it is built in.
	environment env.Environment
}

// NewPGStore builds a Postgres developer Store.
func NewPGStore(pool *pgxpool.Pool, environment env.Environment) Store {
	return &pgStore{pool: pool, environment: environment}
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ── workspaces ───────────────────────────────────────────────────────────────

func (s *pgStore) CreateWorkspace(ctx context.Context, name, slug, owner string) (Workspace, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, err
	}
	defer tx.Rollback(ctx)
	var w Workspace
	err = tx.QueryRow(ctx,
		`INSERT INTO developer.dev_workspaces (name, slug, created_by)
		 VALUES ($1,$2,$3) RETURNING id, name, slug, created_by, status, created_at, updated_at`,
		name, slug, owner).Scan(&w.ID, &w.Name, &w.Slug, &w.CreatedBy, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		if isUnique(err) {
			return Workspace{}, ErrConflict
		}
		return Workspace{}, err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO developer.dev_workspace_members (workspace_id, user_id, role, accepted_at, status)
		 VALUES ($1,$2,'OWNER', now(), 'ACTIVE')`, w.ID, owner); err != nil {
		return Workspace{}, err
	}
	return w, tx.Commit(ctx)
}

func (s *pgStore) WorkspacesForUser(ctx context.Context, userID string) ([]Workspace, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.name, w.slug, w.created_by, w.status, w.created_at, w.updated_at
		   FROM developer.dev_workspaces w
		   JOIN developer.dev_workspace_members m ON m.workspace_id = w.id
		  WHERE m.user_id = $1 AND m.status = 'ACTIVE'
		  ORDER BY w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.CreatedBy, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *pgStore) Workspace(ctx context.Context, id string) (Workspace, error) {
	var w Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, slug, created_by, status, created_at, updated_at
		   FROM developer.dev_workspaces WHERE id = $1`, id).
		Scan(&w.ID, &w.Name, &w.Slug, &w.CreatedBy, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return w, err
}

func (s *pgStore) Membership(ctx context.Context, workspaceID, userID string) (*Member, error) {
	var m Member
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, user_id, role, status, created_at
		   FROM developer.dev_workspace_members
		  WHERE workspace_id = $1 AND user_id = $2 AND status = 'ACTIVE'`,
		workspaceID, userID).Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *pgStore) Members(ctx context.Context, workspaceID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, user_id, role, status, created_at
		   FROM developer.dev_workspace_members
		  WHERE workspace_id = $1 AND status = 'ACTIVE' ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *pgStore) CountOwners(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM developer.dev_workspace_members
		  WHERE workspace_id = $1 AND role = 'OWNER' AND status = 'ACTIVE'`, workspaceID).Scan(&n)
	return n, err
}

func (s *pgStore) SetMemberRole(ctx context.Context, workspaceID, userID, role string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_workspace_members SET role = $3, updated_at = now()
		  WHERE workspace_id = $1 AND user_id = $2 AND status = 'ACTIVE'`, workspaceID, userID, role)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_workspace_members SET status = 'REMOVED', updated_at = now()
		  WHERE workspace_id = $1 AND user_id = $2 AND status = 'ACTIVE'`, workspaceID, userID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ── invites ──────────────────────────────────────────────────────────────────

func scanInvite(row pgx.Row) (*Invite, error) {
	var i Invite
	err := row.Scan(&i.ID, &i.WorkspaceID, &i.Email, &i.Role, &i.InvitedBy, &i.ExpiresAt, &i.AcceptedAt, &i.RevokedAt, &i.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

const inviteCols = `id, workspace_id, email, role, coalesce(invited_by_user_id::text,''), expires_at, accepted_at, revoked_at, created_at`

func (s *pgStore) CreateInvite(ctx context.Context, in InviteInsert) (Invite, error) {
	i, err := scanInvite(s.pool.QueryRow(ctx,
		`INSERT INTO developer.dev_workspace_invites (workspace_id, email, role, token_hash, invited_by_user_id, expires_at)
		 VALUES ($1, lower($2), $3, $4, nullif($5,'')::uuid, $6)
		 RETURNING `+inviteCols,
		in.WorkspaceID, in.Email, in.Role, in.TokenHash, in.InvitedBy, in.ExpiresAt))
	if err != nil {
		if isUnique(err) {
			return Invite{}, ErrConflict
		}
		return Invite{}, err
	}
	return *i, nil
}

func (s *pgStore) ActiveInviteByEmail(ctx context.Context, workspaceID, email string) (*Invite, error) {
	return scanInvite(s.pool.QueryRow(ctx,
		`SELECT `+inviteCols+` FROM developer.dev_workspace_invites
		  WHERE workspace_id = $1 AND lower(email) = lower($2)
		    AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		  LIMIT 1`, workspaceID, email))
}

func (s *pgStore) InviteByTokenHash(ctx context.Context, tokenHash string) (*Invite, error) {
	return scanInvite(s.pool.QueryRow(ctx,
		`SELECT `+inviteCols+` FROM developer.dev_workspace_invites WHERE token_hash = $1`, tokenHash))
}

func (s *pgStore) AcceptInvite(ctx context.Context, inviteID, userID string) (Member, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback(ctx)
	var wsID, role string
	if err = tx.QueryRow(ctx,
		`UPDATE developer.dev_workspace_invites SET accepted_at = now(), updated_at = now()
		  WHERE id = $1 RETURNING workspace_id, role`, inviteID).Scan(&wsID, &role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotFound
		}
		return Member{}, err
	}
	var m Member
	if err = tx.QueryRow(ctx,
		`INSERT INTO developer.dev_workspace_members (workspace_id, user_id, role, accepted_at, status)
		 VALUES ($1,$2,$3, now(), 'ACTIVE')
		 ON CONFLICT (workspace_id, user_id)
		 DO UPDATE SET role = EXCLUDED.role, status = 'ACTIVE', updated_at = now()
		 RETURNING id, workspace_id, user_id, role, status, created_at`,
		wsID, userID, role).Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt); err != nil {
		return Member{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *pgStore) RevokeInvite(ctx context.Context, inviteID string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_workspace_invites SET revoked_at = now(), updated_at = now()
		  WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, inviteID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ── projects ─────────────────────────────────────────────────────────────────

func (s *pgStore) CreateProject(ctx context.Context, workspaceID, name, slug string) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`INSERT INTO developer.dev_projects (workspace_id, name, slug)
		 VALUES ($1,$2,$3) RETURNING id, workspace_id, name, slug, status, created_at, updated_at`,
		workspaceID, name, slug).Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Slug, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if isUnique(err) {
			return Project{}, ErrConflict
		}
		return Project{}, err
	}
	return p, nil
}

// ArchiveProject moves a project to ARCHIVED and revokes its remaining ACTIVE
// keys in the same transaction. It deliberately does not touch the project's
// sandbox binding: a sealed binding is immutable by ADR-055, and retiring the
// container that holds it is the supported way to dispose of a fixture without
// weakening that invariant.
func (s *pgStore) ArchiveProject(ctx context.Context, id string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Converge, do not refuse.
	//
	// This returned NotFound when the project was already ARCHIVED, which made
	// retirement non-idempotent in the one way that mattered: a project archived
	// BEFORE binding-disable existed could never have its binding repaired,
	// because the only operation that would repair it declined to run. A
	// historical Sandbox project was found in exactly that state — ARCHIVED, and
	// still holding an ACTIVE binding.
	//
	// NotFound now means the project does not exist. An archived one is brought
	// the rest of the way.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM developer.dev_projects WHERE id = $1`, id).Scan(&exists); err != nil {
		return 0, ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE developer.dev_projects SET status = 'ARCHIVED', updated_at = now()
		  WHERE id = $1 AND status = 'ACTIVE'`, id); err != nil {
		return 0, err
	}

	keys, err := tx.Exec(ctx,
		`UPDATE developer.dev_api_keys SET status = 'REVOKED', revoked_at = now()
		  WHERE project_id = $1 AND status = 'ACTIVE'`, id)
	if err != nil {
		return 0, err
	}

	// Retiring a project must retire its authority, or the project is archived
	// while its financial owner still answers to an ACTIVE binding — which is
	// exactly the state that left a historical Sandbox merchant holding a handle
	// under a live binding nobody could reach.
	//
	// A SEALED binding is immutable (ADR-055) and is deliberately left alone: it
	// is the record that this owner once issued a payer-facing artifact, and that
	// record does not stop being true because the project was archived. Only an
	// UNSEALED binding — one that never issued anything — is disabled.
	if _, err := tx.Exec(ctx,
		`UPDATE developer.dev_project_sandbox_binding
		    SET state = 'DISABLED', updated_at = now()
		  WHERE project_id = $1 AND state = 'ACTIVE' AND artifact_created = false`, id); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(keys.RowsAffected()), nil
}

func (s *pgStore) ProjectsForWorkspace(ctx context.Context, workspaceID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, slug, status, created_at, updated_at
		   FROM developer.dev_projects WHERE workspace_id = $1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Slug, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *pgStore) Project(ctx context.Context, id string) (*Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, slug, status, created_at, updated_at
		   FROM developer.dev_projects WHERE id = $1`, id).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Slug, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ── api keys ─────────────────────────────────────────────────────────────────

const keyMetaCols = `id, project_id, environment, kind, name, key_prefix, coalesce(public_value,''), scopes, status, rotated_from, created_at, last_used_at`

func scanKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.ProjectID, &k.Environment, &k.Kind, &k.Name, &k.KeyPrefix,
		&k.PublicValue, &k.Scopes, &k.Status, &k.RotatedFrom, &k.CreatedAt, &k.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *pgStore) insertKeyTx(ctx context.Context, q pgx.Tx, in APIKeyInsert) (APIKey, error) {
	k, err := scanKey(q.QueryRow(ctx,
		`INSERT INTO developer.dev_api_keys
		    (project_id, environment, kind, name, key_prefix, key_hash, hash_version, public_value, scopes, created_by, rotated_from)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,nullif($8,''),$9,$10,$11)
		 RETURNING `+keyMetaCols,
		in.ProjectID, in.Environment, in.Kind, in.Name, in.KeyPrefix, in.KeyHash, in.HashVersion,
		in.PublicValue, in.Scopes, in.CreatedBy, in.RotatedFrom))
	if err != nil {
		return APIKey{}, err
	}
	return *k, nil
}

func (s *pgStore) CreateAPIKey(ctx context.Context, in APIKeyInsert) (APIKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, err
	}
	defer tx.Rollback(ctx)
	k, err := s.insertKeyTx(ctx, tx, in)
	if err != nil {
		return APIKey{}, err
	}
	return k, tx.Commit(ctx)
}

func (s *pgStore) APIKeysForProject(ctx context.Context, projectID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+keyMetaCols+` FROM developer.dev_api_keys WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.Environment, &k.Kind, &k.Name, &k.KeyPrefix,
			&k.PublicValue, &k.Scopes, &k.Status, &k.RotatedFrom, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *pgStore) APIKeyByID(ctx context.Context, id string) (*APIKey, error) {
	return scanKey(s.pool.QueryRow(ctx, `SELECT `+keyMetaCols+` FROM developer.dev_api_keys WHERE id = $1`, id))
}

func (s *pgStore) APIKeyByHash(ctx context.Context, keyHash string) (*APIKeyAuth, error) {
	var a APIKeyAuth
	err := s.pool.QueryRow(ctx,
		`SELECT id, project_id, environment, status, scopes FROM developer.dev_api_keys WHERE key_hash = $1`,
		keyHash).Scan(&a.ID, &a.ProjectID, &a.Environment, &a.Status, &a.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *pgStore) RevokeAPIKey(ctx context.Context, id string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_api_keys SET status = 'REVOKED', revoked_at = now()
		  WHERE id = $1 AND status = 'ACTIVE'`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// apiKeyTouchInterval is how stale last_used_at may get before another write.
//
// A key is presented on every request, and stamping a row that often would add
// a write to the busiest path in the service to gain precision nobody reads:
// the Console renders this as "há 3 minutos", and rotation decisions are made
// in hours. Five minutes keeps the answer honest at a cost that does not scale
// with traffic.
const apiKeyTouchInterval = 5 * time.Minute

func (s *pgStore) TouchAPIKeyUsed(ctx context.Context, id string) error {
	// The WHERE clause is the throttle. A busy key matches no rows almost every
	// time, which is a single indexed lookup and no row write.
	_, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_api_keys
		    SET last_used_at = now()
		  WHERE id = $1
		    AND (last_used_at IS NULL OR last_used_at < now() - $2::interval)`,
		id, apiKeyTouchInterval.String())
	return err
}

func (s *pgStore) RotateAPIKey(ctx context.Context, oldID string, replacement APIKeyInsert) (APIKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, err
	}
	defer tx.Rollback(ctx)
	nk, err := s.insertKeyTx(ctx, tx, replacement)
	if err != nil {
		return APIKey{}, err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE developer.dev_api_keys SET status = 'REVOKED', revoked_at = now()
		  WHERE id = $1 AND status = 'ACTIVE'`, oldID)
	if err != nil {
		return APIKey{}, err
	}
	if ct.RowsAffected() == 0 {
		return APIKey{}, ErrNotFound
	}
	return nk, tx.Commit(ctx)
}

// ── project→merchant sandbox binding (ADR-047) ───────────────────────────────

const bindingCols = `id, project_id, environment, merchant_id, wallet_id, wallet_account_id,
	state, artifact_created, created_by_user_id, created_at`

func scanBinding(row pgx.Row) (*SandboxBinding, error) {
	var b SandboxBinding
	err := row.Scan(&b.ID, &b.ProjectID, &b.Environment, &b.MerchantID, &b.WalletID,
		&b.WalletAccountID, &b.State, &b.ArtifactCreated, &b.CreatedByUserID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *pgStore) CreateBinding(ctx context.Context, in BindingInsert) (SandboxBinding, error) {
	b, err := scanBinding(s.pool.QueryRow(ctx,
		`INSERT INTO developer.dev_project_sandbox_binding
		    (project_id, merchant_id, wallet_id, wallet_account_id, created_by_user_id)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+bindingCols,
		in.ProjectID, in.MerchantID, in.WalletID, in.WalletAccountID, in.CreatedByUserID))
	if err != nil {
		if isUnique(err) {
			return SandboxBinding{}, ErrConflict // one ACTIVE binding per project
		}
		return SandboxBinding{}, err
	}
	return *b, nil
}

// SupersedeAndCreateBinding disables the project's ACTIVE binding and records a
// new one atomically. Returns the new binding and the id of the one it replaced
// ("" when the project had none).
func (s *pgStore) SupersedeAndCreateBinding(ctx context.Context, in BindingInsert) (SandboxBinding, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SandboxBinding{}, "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	var superseded string
	// A sealed binding is excluded here as well as in the service: once a payment
	// artifact exists the payee can never change (ADR-047 §3.2), and the rule
	// should hold even if a future caller reaches this store directly.
	err = tx.QueryRow(ctx,
		`UPDATE developer.dev_project_sandbox_binding
		    SET state = 'DISABLED', updated_at = now()
		  WHERE project_id = $1 AND state = 'ACTIVE' AND artifact_created = false
		  RETURNING id::text`, in.ProjectID).Scan(&superseded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SandboxBinding{}, "", err
	}

	b, err := scanBinding(tx.QueryRow(ctx,
		`INSERT INTO developer.dev_project_sandbox_binding
		    (project_id, merchant_id, wallet_id, wallet_account_id, created_by_user_id)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+bindingCols,
		in.ProjectID, in.MerchantID, in.WalletID, in.WalletAccountID, in.CreatedByUserID))
	if err != nil {
		if isUnique(err) {
			// The only ACTIVE row left is a sealed one, which the UPDATE above
			// deliberately refused to touch.
			return SandboxBinding{}, "", ErrConflict
		}
		return SandboxBinding{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return SandboxBinding{}, "", err
	}
	return *b, superseded, nil
}

// ProjectsBoundToMerchant answers "does anyone already hold this owner?".
//
// ACTIVE bindings only: a DISABLED one is a record of a binding that was
// superseded, and refusing on it would make a project's own history block its
// recovery.
func (s *pgStore) ProjectsBoundToMerchant(ctx context.Context, merchantID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT project_id::text FROM developer.dev_project_sandbox_binding
		  WHERE merchant_id = $1 AND state = 'ACTIVE'`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *pgStore) ActiveBindingForProject(ctx context.Context, projectID string) (*SandboxBinding, error) {
	b, err := scanBinding(s.pool.QueryRow(ctx,
		`SELECT `+bindingCols+` FROM developer.dev_project_sandbox_binding
		 WHERE project_id = $1 AND state = 'ACTIVE'`, projectID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil // no active binding is not an error
	}
	return b, err
}

func (s *pgStore) MarkBindingArtifactCreated(ctx context.Context, bindingID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE developer.dev_project_sandbox_binding
		    SET artifact_created = true, updated_at = now()
		  WHERE id = $1`, bindingID)
	return err
}

// ── audit ────────────────────────────────────────────────────────────────────

func (s *pgStore) InsertAudit(ctx context.Context, ev AuditEvent) error {
	meta := ev.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO developer.audit_events
		    (actor_user_id, workspace_id, project_id, action, subject, metadata, request_ip, request_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ev.ActorUserID, ev.WorkspaceID, ev.ProjectID, ev.Action, nz(ev.Subject), b, nz(ev.RequestIP), nz(ev.RequestID))
	return err
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── Webhook visibility ───────────────────────────────────────────────────────
//
// Every query is scoped by merchant_id in the statement itself. The merchant is
// never taken from a request; the caller resolves it from the project binding
// before calling here.

// WalletAccountsForMerchant lists the merchant's accounts with the balance each
// one holds, summed from the ledger in the same query. Summing here rather than
// reading a stored total means the number cannot drift from the entries it
// claims to describe — and a balance that drifts is worse than none, because it
// still looks authoritative.
//
// Ordering is (created_at DESC, id DESC): a run of campaigns is opened inside
// the same second, so created_at alone is not a stable key to page by.
func (s *pgStore) WalletAccountsForMerchant(ctx context.Context, merchantID string, f WalletAccountFilter) ([]WalletAccountView, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT wa.id, COALESCE(wa.label,''), wa.purpose,
		        COALESCE(wa.reference_type::text,''), COALESCE(wa.reference_id::text,''),
		        wa.currency,
		        COALESCE(SUM(CASE WHEN e.entry_type = 'CREDIT' THEN e.amount_minor
		                          ELSE -e.amount_minor END), 0)::bigint,
		        wa.status, wa.created_at
		   FROM wallet_accounts wa
		   JOIN wallets w ON w.id = wa.wallet_id
		   LEFT JOIN ledger_entries e ON e.account_id = wa.account_id
		  WHERE w.merchant_id = $1
		    AND ($2 = '' OR (wa.created_at, wa.id) <
		         (SELECT c.created_at, c.id FROM wallet_accounts c WHERE c.id::text = $2))
		  GROUP BY wa.id, wa.label, wa.purpose, wa.reference_type, wa.reference_id,
		           wa.currency, wa.status, wa.created_at
		  ORDER BY wa.created_at DESC, wa.id DESC
		  LIMIT $3`, merchantID, f.Cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WalletAccountView{}
	for rows.Next() {
		var v WalletAccountView
		if err := rows.Scan(&v.ID, &v.Label, &v.Purpose, &v.ReferenceType, &v.ReferenceID,
			&v.Currency, &v.BalanceMinor, &v.Status, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// TransactionsForMerchant unions the three released financial operation types
// into one ordered stream. Each keeps its own type: a payment and a refund are
// not the same event, and flattening them into "transaction" would lose the
// distinction a developer is reading the page to see.
//
// Paged by created_at rather than an offset, so a new operation arriving
// between two pages cannot shift rows across the boundary and make one vanish.
func (s *pgStore) TransactionsForMerchant(ctx context.Context, merchantID string, f TransactionFilter) ([]TransactionView, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		// The acquiring columns are the OPERATOR's view of execution, resolved from
		// whichever interface the payer actually used. A session can be paid
		// through its link or its QR, so both are consulted: reading only the link
		// would report a QR-paid session as UNPAID, which is a worse answer than
		// the one this query exists to replace.
		//
		// Nothing here edits the session's own status. The two columns sit side by
		// side precisely so a developer can see that value arrived while the
		// protocol representation has not moved (BANZA RFC-0007).
		`WITH ops AS (
		   SELECT s.id::text AS id, 'payment' AS type, s.status::text AS status,
		          s.amount_minor::bigint AS amount_minor, s.currency::text AS currency,
		          COALESCE(s.wallet_account_id::text,'') AS wallet_account_id,
		          COALESCE(s.reference_type::text,'') AS reference_type,
		          COALESCE(s.reference_id::text,'') AS reference_id,
		          s.created_at,
		          -- PAID means money moved, and the ledger is the only thing that
		          -- knows. This asked whether the LINK was USED, which is a
		          -- different fact: a payment confirmed before the acquiring
		          -- settlement defect was fixed marks its link USED and posts
		          -- nothing, so the column reported "PAGO · Recebido 100 000 Kz"
		          -- against a campaign account holding zero. Reproduced on the
		          -- deployed Sandbox for the 11:25 DOA payment, whose destination
		          -- account balance is 0 to this day. A view that invents a receipt
		          -- is worse than the blank one it replaced.
		          CASE WHEN settled.credited_minor IS NOT NULL THEN 'PAID'
		               WHEN s.status = 'PAID'                  THEN 'PAID'
		               ELSE 'UNPAID' END AS acq_state,
		          CASE WHEN settled.credited_minor IS NOT NULL THEN 'PAYMENT_LINK'
		               WHEN s.status = 'PAID' AND pl.status = 'USED' THEN 'PAYMENT_LINK'
		               WHEN s.status = 'PAID' AND q.status  = 'USED' THEN 'DYNAMIC_QR'
		               ELSE '' END AS acq_interface,
		          COALESCE(settled.posted_at,
		                   CASE WHEN s.status = 'PAID' THEN COALESCE(pl.paid_at, q.used_at) END) AS acq_paid_at,
		          -- What actually LANDED: the credit leg of the settlement posting.
		          -- Never the requested amount, and for an open-amount session never
		          -- a figure nobody paid.
		          CASE WHEN settled.credited_minor IS NOT NULL THEN settled.credited_minor
		               WHEN s.status = 'PAID' THEN s.amount_minor::bigint END AS acq_amount_minor,
		          CASE WHEN settled.credited_account IS NOT NULL THEN settled.credited_account::text
		               WHEN s.status = 'PAID'
		                    THEN COALESCE(pl.wallet_account_id::text, s.wallet_account_id::text, '')
		               ELSE '' END AS acq_account
		     FROM payment_sessions s
		     LEFT JOIN payment_links pl ON pl.id = s.payment_link_id
		     LEFT JOIN qr_codes      q  ON q.id  = s.qr_code_id
		     -- The receipt for an externally acquired payment. At most one is
		     -- CONFIRMED per link, so this cannot multiply the row.
		     LEFT JOIN acquiring_payments ap
		            ON ap.payment_link_id = s.payment_link_id AND ap.status = 'CONFIRMED'
		     -- The settlement itself, read from the ledger. Its idempotency key is
		     -- how the acquiring rail names a settlement, so its presence is the
		     -- fact that money moved, and the CREDIT leg is what landed and where.
		     LEFT JOIN LATERAL (
		         SELECT le.amount_minor AS credited_minor,
		                wa.id           AS credited_account,
		                lp.created_at   AS posted_at
		           FROM ledger_postings lp
		           JOIN ledger_entries  le ON le.posting_id = lp.id AND le.entry_type = 'CREDIT'
		           LEFT JOIN wallet_accounts wa ON wa.account_id = le.account_id
		          WHERE ap.id IS NOT NULL
		            AND lp.idempotency_key = 'acquiring-settle-' || ap.id::text
		          LIMIT 1
		     ) settled ON TRUE
		    WHERE s.merchant_id = $1
		   UNION ALL
		   SELECT r.id::text, 'refund', r.status::text,
		          r.amount_minor::bigint, r.currency::text,
		          '', COALESCE(r.source_type::text,''), COALESCE(r.source_id::text,''), r.created_at,
		          '', '', NULL::timestamptz, NULL::bigint, ''
		     FROM refunds r WHERE r.merchant_id = $1
		   UNION ALL
		   SELECT t.id::text, 'transfer', t.status::text,
		          t.amount_minor::bigint, t.currency::text,
		          COALESCE(t.dest_account_id::text,''), 'WALLET_ACCOUNT',
		          COALESCE(t.source_account_id::text,''), t.created_at,
		          '', '', NULL::timestamptz, NULL::bigint, ''
		     FROM wallet_account_transfers t WHERE t.merchant_id = $1
		 )
		 SELECT id, type, status, amount_minor, currency, wallet_account_id,
		        reference_type, reference_id, created_at,
		        acq_state, acq_interface, acq_paid_at, acq_amount_minor, acq_account
		   FROM ops
		  WHERE ($2 = '' OR type = $2)
		    AND ($3 = '' OR status = $3)
		    AND ($4::timestamptz IS NULL OR created_at >= $4)
		    AND ($5::timestamptz IS NULL OR created_at <= $5)
		    AND ($6::timestamptz IS NULL OR created_at < $6)
		  ORDER BY created_at DESC
		  LIMIT $7`,
		merchantID, f.Type, f.Status, f.Since, f.Until, cursorTime(f.Cursor), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransactionView{}
	for rows.Next() {
		var v TransactionView
		var acqState, acqInterface, acqAccount string
		var acqPaidAt *time.Time
		var acqAmount *int64
		if err := rows.Scan(&v.ID, &v.Type, &v.Status, &v.AmountMinor, &v.Currency,
			&v.WalletAccountID, &v.ReferenceType, &v.ReferenceID, &v.CreatedAt,
			&acqState, &acqInterface, &acqPaidAt, &acqAmount, &acqAccount); err != nil {
			return nil, err
		}
		// Only a payment has an execution state; a refund or an internal transfer
		// has no acquiring rail, and inventing an UNPAID for one would say
		// something false about it.
		if acqState != "" {
			a := &AcquiringView{
				State:                   acqState,
				AmountMinor:             acqAmount,
				PaidAt:                  acqPaidAt,
				CreditedWalletAccountID: acqAccount,
				Interface:               acqInterface,
			}
			// The note belongs only where the two genuinely disagree. Attaching it
			// to every row would train the reader to skip it.
			if acqState == "PAID" && v.Status != "PAID" {
				a.ProtocolNote = AcquiringProtocolNote
			}
			v.Acquiring = a
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// cursorTime turns the opaque cursor back into a timestamp, or nil when there
// is none. An unparseable cursor becomes nil rather than an error: the worst it
// can do is return the first page, and refusing the whole request because a
// query string was mangled helps nobody.
func cursorTime(c string) *time.Time {
	if c == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, c)
	if err != nil {
		return nil
	}
	return &t
}

func (s *pgStore) WalletAccountCountForMerchant(ctx context.Context, merchantID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM wallet_accounts wa JOIN wallets w ON w.id = wa.wallet_id
		  WHERE w.merchant_id = $1`, merchantID).Scan(&n)
	return n, err
}

func (s *pgStore) WebhookEndpointsForMerchant(ctx context.Context, merchantID string) ([]WebhookEndpointView, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, url, events, active, created_at
		   FROM webhook_endpoints
		  WHERE merchant_id = $1
		  ORDER BY created_at DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookEndpointView{}
	for rows.Next() {
		var v WebhookEndpointView
		if err := rows.Scan(&v.ID, &v.URL, &v.Events, &v.Active, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateWebhookEndpoint inserts an endpoint owned by merchantID.
//
// The secret arrives already encrypted (or already plaintext, in a deployment
// without a key). This layer does not know which, deliberately: choosing how a
// secret is protected is not a decision for a SQL statement.
func (s *pgStore) CreateWebhookEndpoint(ctx context.Context, merchantID, url string, events []string, storedSecret string) (*WebhookEndpointView, error) {
	// The environment column defaults to 'LIVE'. Omitting it here registered every
	// Console-created endpoint as a real-money subscriber, which is both the wrong
	// record and the wrong dispatch set.
	if !s.environment.IsKnown() {
		return nil, ErrEnvironmentUndeclared
	}
	var v WebhookEndpointView
	err := s.pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (id, merchant_id, url, events, active, secret, environment, created_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, true, $4, $5, now())
		 RETURNING id, url, events, active, created_at`,
		merchantID, url, events, storedSecret, s.environment.String(),
	).Scan(&v.ID, &v.URL, &v.Events, &v.Active, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// RotateWebhookEndpointSecret replaces the stored secret.
//
// merchant_id is in the WHERE clause rather than checked beforehand: a caller
// who guesses an endpoint id belonging to someone else updates zero rows and is
// told the endpoint does not exist, which is the same answer they get for one
// that really does not.
func (s *pgStore) RotateWebhookEndpointSecret(ctx context.Context, merchantID, endpointID, storedSecret string) (*WebhookEndpointView, error) {
	var v WebhookEndpointView
	err := s.pool.QueryRow(ctx,
		`UPDATE webhook_endpoints SET secret = $3
		  WHERE id = $2 AND merchant_id = $1
		 RETURNING id, url, events, active, created_at`,
		merchantID, endpointID, storedSecret,
	).Scan(&v.ID, &v.URL, &v.Events, &v.Active, &v.CreatedAt)
	if err != nil {
		return nil, ErrNotFound
	}
	return &v, nil
}

// SetWebhookEndpointActive enables or disables delivery. Same merchant scoping.
func (s *pgStore) SetWebhookEndpointActive(ctx context.Context, merchantID, endpointID string, active bool) (*WebhookEndpointView, error) {
	var v WebhookEndpointView
	err := s.pool.QueryRow(ctx,
		`UPDATE webhook_endpoints SET active = $3
		  WHERE id = $2 AND merchant_id = $1
		 RETURNING id, url, events, active, created_at`,
		merchantID, endpointID, active,
	).Scan(&v.ID, &v.URL, &v.Events, &v.Active, &v.CreatedAt)
	if err != nil {
		return nil, ErrNotFound
	}
	return &v, nil
}

func (s *pgStore) WebhookEventsForMerchant(ctx context.Context, merchantID string, limit int) ([]WebhookEventView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, event_type, created_at
		   FROM webhook_events
		  WHERE merchant_id = $1
		  ORDER BY created_at DESC
		  LIMIT $2`, merchantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookEventView{}
	for rows.Next() {
		var v WebhookEventView
		if err := rows.Scan(&v.ID, &v.EventType, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *pgStore) WebhookDeliveriesForEvent(ctx context.Context, merchantID, eventID string) ([]WebhookDeliveryView, error) {
	// Joined through webhook_events so a caller naming another tenant's event id
	// gets an empty list rather than that tenant's delivery history — naming an
	// id is not authority over it (RA-060 sibling of the gateway's own rule).
	rows, err := s.pool.Query(ctx,
		`SELECT d.id, d.event_id, d.endpoint_id, d.status, d.status_code,
		        d.attempt_count, d.delivered_at, d.created_at
		   FROM webhook_deliveries d
		   JOIN webhook_events e ON e.id = d.event_id
		  WHERE d.event_id = $1 AND e.merchant_id = $2
		  ORDER BY d.created_at DESC`, eventID, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookDeliveryView{}
	for rows.Next() {
		var v WebhookDeliveryView
		if err := rows.Scan(&v.ID, &v.EventID, &v.EndpointID, &v.Status, &v.StatusCode,
			&v.AttemptCount, &v.DeliveredAt, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// APIRequestLogs reads one project's Developer API request log (migration 0104).
//
// The project id is a bound parameter of the query itself, not a filter the
// caller composes: authorisation happens above, but even a bug there cannot make
// this statement return another project's rows. Every optional filter narrows.
func (s *pgStore) APIRequestLogs(ctx context.Context, projectID string, f RequestLogFilter) ([]APIRequestLogView, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	args := []any{projectID}
	var where []string
	// bind appends a value and returns its placeholder, so no clause has to know
	// its own position and no value is ever concatenated into the SQL.
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.RequestID != "" {
		where = append(where, "request_id = "+bind(f.RequestID))
	}
	if f.Status > 0 {
		where = append(where, "status = "+bind(f.Status))
	}
	if f.Path != "" {
		p := bind(f.Path)
		where = append(where, fmt.Sprintf("(path ILIKE '%%' || %s || '%%' OR route ILIKE '%%' || %s || '%%')", p, p))
	}
	if f.Since != nil {
		where = append(where, "created_at >= "+bind(*f.Since))
	}
	if f.Until != nil {
		where = append(where, "created_at <= "+bind(*f.Until))
	}

	q := `SELECT id, method, path, route, status, request_id, latency_ms, environment, created_at
	        FROM developer.dev_api_request_logs
	       WHERE project_id = $1`
	for _, c := range where {
		q += " AND " + c
	}
	q += " ORDER BY created_at DESC LIMIT " + bind(limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIRequestLogView{}
	for rows.Next() {
		var v APIRequestLogView
		if err := rows.Scan(&v.ID, &v.Method, &v.Path, &v.Route, &v.Status,
			&v.RequestID, &v.LatencyMS, &v.Environment, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// APIRequestLogSummary aggregates one project's request log over the same
// window the list uses. The project id is bound inside the statement for the
// same reason as the list: a filter may narrow it, nothing may widen it.
func (s *pgStore) APIRequestLogSummary(ctx context.Context, projectID string, f RequestLogFilter) (RequestLogSummary, error) {
	var out RequestLogSummary
	since := time.Now().AddDate(0, 0, -7)
	if f.Since != nil {
		since = *f.Since
	}
	out.WindowStart = &since

	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COUNT(*) FILTER (WHERE status >= 400),
		        percentile_disc(0.5) WITHIN GROUP (ORDER BY latency_ms)
		   FROM developer.dev_api_request_logs
		  WHERE project_id = $1 AND created_at >= $2`,
		projectID, since).Scan(&out.Requests, &out.Errors, &out.MedianMS)
	if err != nil {
		return out, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD') AS day, COUNT(*)
		   FROM developer.dev_api_request_logs
		  WHERE project_id = $1 AND created_at >= $2
		  GROUP BY 1 ORDER BY 1`, projectID, since)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.ByDay = []RequestDayCount{}
	for rows.Next() {
		var d RequestDayCount
		if err := rows.Scan(&d.Day, &d.Count); err != nil {
			return out, err
		}
		out.ByDay = append(out.ByDay, d)
	}
	return out, rows.Err()
}
