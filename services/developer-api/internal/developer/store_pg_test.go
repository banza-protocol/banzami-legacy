package developer

import (
	"context"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/banzami/banzami/services/common/env"
)

// devPoolOrSkip returns a real Postgres pool for the developer schema, or skips.
// Persistence invariants MUST NOT be mocked — these run against a migrated DB
// (DATABASE_URL) and are skipped when the developer schema is not present.
func devPoolOrSkip(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set — skipping DB-backed developer store test")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("cannot connect to DATABASE_URL: %v", err)
	}
	var reg *string
	_ = pool.QueryRow(ctx, `SELECT to_regclass('developer.dev_api_keys')::text`).Scan(&reg)
	if reg == nil {
		pool.Close()
		t.Skip("developer schema not migrated — skipping")
	}
	return pool
}

// TestPgStore_APIKeyLifecycle proves the key persistence invariants against a real
// DB: an active synthetic key is accepted, a revoked key is rejected, and an
// invalid key is rejected — all via the real pgStore (no mocks).
func TestPgStore_APIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := devPoolOrSkip(ctx, t)
	defer pool.Close()

	svc := NewService(NewPGStore(pool, env.Sandbox), "invite-secret-fixture", "api-key-pepper-fixture", time.Hour)
	svc.SetFixturesEnabled(true) // sandbox

	// developer.dev_workspaces.created_by is a uuid column, so the actor id must
	// be a UUID. Passing a human label here made every fixture insert fail with a
	// type error, which the service correctly maps to a fail-closed
	// ErrUnavailable — so this test could never reach the key lifecycle it exists
	// to prove.
	actor := uuid.NewString()
	_, proj, err := svc.CreateFixtureProject(ctx, "Phase0 Real DB Platform", actor, "", "")
	if err != nil {
		t.Fatalf("fixture project must be creatable on a migrated developer schema: %v", err)
	}
	key, secret, err := svc.CreateFixtureAPIKey(ctx, proj.ID, "e2e-realdb", []string{"payment_sessions:write"}, actor, "", "")
	if err != nil || secret == "" {
		t.Fatalf("fixture key must be issuable: %v", err)
	}

	// 1) active synthetic key accepted for its scope.
	if _, err := svc.AuthorizeKey(ctx, secret, "payment_sessions:write"); err != nil {
		t.Fatalf("active key must authorize its scope: %v", err)
	}
	// 2) invalid key rejected (neutral ErrForbidden).
	if _, err := svc.AuthorizeKey(ctx, "bz_test_sk_invalid_"+proj.ID, "payment_sessions:write"); err != ErrForbidden {
		t.Errorf("invalid key: want ErrForbidden, got %v", err)
	}
	// 3) revoked key rejected — persisted status transition, re-read from the DB.
	if err := svc.store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("revoke must persist: %v", err)
	}
	if _, err := svc.AuthorizeKey(ctx, secret, "payment_sessions:write"); err != ErrForbidden {
		t.Errorf("revoked key: want ErrForbidden, got %v", err)
	}
	// 4) missing-scope on an otherwise-valid (fresh) key rejected.
	key2, secret2, err := svc.CreateFixtureAPIKey(ctx, proj.ID, "e2e-scope", []string{"payment_sessions:read"}, actor, "", "")
	if err != nil {
		t.Fatalf("second fixture key: %v", err)
	}
	_ = key2
	if _, err := svc.AuthorizeKey(ctx, secret2, "payment_sessions:write"); err != ErrForbidden {
		t.Errorf("missing scope: want ErrForbidden, got %v", err)
	}
}
