package service

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// processOutbox fans a core-emitted webhook_event out into deliveries for the
// affected merchant's subscribed endpoints only. DB-backed; skipped when
// DATABASE_URL is unset.
func TestProcessOutbox_FansOutToOwningMerchantOnly(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set — skipping DB-backed outbox test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	svc := &PostgresWebhookService{pool: pool}

	merchantA := uuid.NewString()
	merchantB := uuid.NewString()
	epA := uuid.NewString()
	epB := uuid.NewString()
	eventID := uuid.NewString()
	idemKey := "refund.completed:" + uuid.NewString()

	// Clean up everything this test creates.
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE event_id = $1`, eventID)
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_events WHERE id = $1`, eventID)
		_, _ = pool.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = ANY($1)`, []string{epA, epB})
	}()

	// Two merchants each subscribe an active endpoint to refund.completed.
	for _, e := range []struct{ id, merchant string }{{epA, merchantA}, {epB, merchantB}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_endpoints (id, merchant_id, url, events, active, secret, environment)
			 VALUES ($1, $2, 'https://example.test/hook', ARRAY['refund.completed'], true, 'sec', 'SANDBOX')`,
			e.id, e.merchant,
		); err != nil {
			t.Fatalf("seed endpoint: %v", err)
		}
	}

	// An undispatched outbox event for merchant A only.
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_events (id, merchant_id, event_type, payload, idempotency_key)
		 VALUES ($1, $2, 'refund.completed', '{"data":{}}'::jsonb, $3)`,
		eventID, merchantA, idemKey,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	svc.processOutbox(ctx)

	// Exactly one delivery — to merchant A's endpoint.
	var toA, toB int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE event_id = $1 AND endpoint_id = $2`,
		eventID, epA).Scan(&toA)
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE event_id = $1 AND endpoint_id = $2`,
		eventID, epB).Scan(&toB)
	if toA != 1 {
		t.Errorf("expected 1 delivery to owning merchant, got %d", toA)
	}
	if toB != 0 {
		t.Errorf("expected 0 deliveries to unrelated merchant, got %d", toB)
	}

	// The event is now marked dispatched and is not re-fanned-out.
	var dispatched bool
	_ = pool.QueryRow(ctx,
		`SELECT dispatched_at IS NOT NULL FROM webhook_events WHERE id = $1`, eventID).Scan(&dispatched)
	if !dispatched {
		t.Error("event should be marked dispatched after fan-out")
	}

	svc.processOutbox(ctx) // idempotent — no duplicate deliveries
	var total int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE event_id = $1`, eventID).Scan(&total)
	if total != 1 {
		t.Errorf("re-running fan-out must not duplicate deliveries, got %d", total)
	}
}
