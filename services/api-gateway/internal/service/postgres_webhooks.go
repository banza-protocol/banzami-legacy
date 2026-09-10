package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/banzami/banzami/services/api-gateway/internal/crypto"
	"github.com/banzami/banzami/services/common/env"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/banzami/banzami/services/api-gateway/internal/webhook"
)

// backoffSchedule defines how long to wait before each retry attempt.
// Index 0 = after 1st failure, index 4 = after 5th (final) failure.
var backoffSchedule = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	8 * time.Hour,
}

// PostgresWebhookService is the production implementation of WebhookService.
// Endpoints, events, and deliveries are persisted in PostgreSQL.
// A background worker polls for pending deliveries and dispatches them with
// exponential-backoff retries (max 5 attempts).
type PostgresWebhookService struct {
	pool   *pgxpool.Pool
	client *http.Client
	cipher *crypto.SecretCipher // encrypts webhook signing secrets at rest (SEC-002)
	// environment tags every endpoint this service creates. It comes from the
	// process configuration, not from the merchant: a Sandbox endpoint that
	// claimed to be LIVE would be selected by the LIVE dispatch query and would
	// receive real payment events.
	environment env.Environment
}

func NewPostgresWebhookService(pool *pgxpool.Pool, cipher *crypto.SecretCipher, environment env.Environment) *PostgresWebhookService {
	return &PostgresWebhookService{
		pool:        pool,
		client:      newSafeWebhookClient(30 * time.Second),
		cipher:      cipher,
		environment: environment,
	}
}

// StartWorker launches the background delivery worker. The worker stops when
// ctx is cancelled (i.e. on graceful shutdown).
func (s *PostgresWebhookService) StartWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Fan out core-emitted outbox events first, then deliver.
				s.processOutbox(ctx)
				s.processPendingDeliveries(ctx)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// WebhookService interface
// ---------------------------------------------------------------------------

func (s *PostgresWebhookService) RegisterEndpoint(
	ctx context.Context,
	req RegisterEndpointRequest,
) (*WebhookEndpoint, error) {
	if err := ValidateWebhookURL(req.URL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWebhookURL, err)
	}

	// Fail closed on an undeclared environment. The column default is 'LIVE', so
	// the alternative to refusing here is registering a real-money endpoint for a
	// process that could not say which universe it serves. Registering a webhook
	// is not urgent enough to guess.
	if !s.environment.IsKnown() {
		return nil, fmt.Errorf("%w: ENVIRONMENT is not set to LIVE or SANDBOX", ErrEnvironmentUndeclared)
	}

	secret := generateWebhookSecret()
	id := uuid.NewString()
	now := time.Now().UTC()

	// Store the signing secret encrypted at rest (SEC-002). The plaintext is
	// returned to the merchant once, here, and never persisted in the clear.
	storedSecret, err := s.cipher.Encrypt(secret)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO webhook_endpoints (id, merchant_id, url, events, active, secret, environment, created_at)
		 VALUES ($1, $2, $3, $4, true, $5, $6, $7)`,
		id, req.MerchantID, req.URL, req.Events, storedSecret, s.environment.String(), now,
	)
	if err != nil {
		return nil, fmt.Errorf("register webhook endpoint: %w", err)
	}

	return &WebhookEndpoint{
		ID:         id,
		MerchantID: req.MerchantID,
		URL:        req.URL,
		Events:     req.Events,
		Active:     true,
		Secret:     secret,
		CreatedAt:  now,
	}, nil
}

func (s *PostgresWebhookService) GetEndpoint(
	ctx context.Context,
	merchantID, endpointID string,
) (*WebhookEndpoint, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, merchant_id, url, events, active, created_at
		 FROM webhook_endpoints
		 WHERE id = $1 AND merchant_id = $2`,
		endpointID, merchantID,
	)

	var ep WebhookEndpoint
	if err := row.Scan(
		&ep.ID, &ep.MerchantID, &ep.URL, &ep.Events, &ep.Active, &ep.CreatedAt,
	); err != nil {
		return nil, ErrEndpointNotFound
	}
	return &ep, nil
}

func (s *PostgresWebhookService) ListEndpoints(
	ctx context.Context,
	merchantID string,
) ([]*WebhookEndpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, merchant_id, url, events, active, created_at
		 FROM webhook_endpoints
		 WHERE merchant_id = $1
		 ORDER BY created_at DESC`,
		merchantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list webhook endpoints: %w", err)
	}
	defer rows.Close()

	var out []*WebhookEndpoint
	for rows.Next() {
		var ep WebhookEndpoint
		if err := rows.Scan(
			&ep.ID, &ep.MerchantID, &ep.URL, &ep.Events, &ep.Active, &ep.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook endpoint: %w", err)
		}
		out = append(out, &ep)
	}
	return out, rows.Err()
}

func (s *PostgresWebhookService) RotateEndpointSecret(
	ctx context.Context,
	merchantID, endpointID string,
) (*WebhookEndpoint, error) {
	secret := generateWebhookSecret()
	stored, err := s.cipher.Encrypt(secret)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	// Scoped by merchant_id in the UPDATE itself, not checked beforehand: a
	// separate read-then-write would authorize against a row that could change
	// underneath it, and would answer differently for an endpoint that exists
	// but belongs to someone else.
	row := s.pool.QueryRow(ctx,
		`UPDATE webhook_endpoints SET secret = $1
		  WHERE id = $2 AND merchant_id = $3
		  RETURNING id, merchant_id, url, events, active, created_at`,
		stored, endpointID, merchantID,
	)

	var ep WebhookEndpoint
	if err := row.Scan(
		&ep.ID, &ep.MerchantID, &ep.URL, &ep.Events, &ep.Active, &ep.CreatedAt,
	); err != nil {
		return nil, ErrEndpointNotFound
	}
	// Returned once, exactly as at registration, and never persisted in clear.
	ep.Secret = secret
	return &ep, nil
}

func (s *PostgresWebhookService) DeactivateEndpoint(
	ctx context.Context,
	merchantID, endpointID string,
) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_endpoints SET active = false
		 WHERE id = $1 AND merchant_id = $2`,
		endpointID, merchantID,
	)
	if err != nil {
		return fmt.Errorf("deactivate webhook endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEndpointNotFound
	}
	return nil
}

func (s *PostgresWebhookService) Dispatch(
	ctx context.Context,
	req DispatchRequest,
) (*WebhookEvent, error) {
	eventID := uuid.NewString()
	now := time.Now().UTC()

	envelope, err := json.Marshal(map[string]any{
		"id":         eventID,
		"type":       req.EventType,
		"created_at": now,
		"data":       req.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal webhook envelope: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook dispatch tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Persist the event. This inline path creates its deliveries in the same
	// transaction, so the event is already dispatched — the outbox worker skips it.
	_, err = tx.Exec(ctx,
		`INSERT INTO webhook_events (id, merchant_id, event_type, payload, created_at, dispatched_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		eventID, req.MerchantID, req.EventType, envelope, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert webhook event: %w", err)
	}

	// Find matching active endpoints.
	endpointRows, err := tx.Query(ctx,
		`SELECT id FROM webhook_endpoints
		 WHERE merchant_id = $1 AND active = true
		   AND ($2 = ANY(events) OR '*' = ANY(events))`,
		req.MerchantID, req.EventType,
	)
	if err != nil {
		return nil, fmt.Errorf("query webhook endpoints: %w", err)
	}

	var endpointIDs []string
	for endpointRows.Next() {
		var id string
		if err := endpointRows.Scan(&id); err != nil {
			endpointRows.Close()
			return nil, fmt.Errorf("scan endpoint id: %w", err)
		}
		endpointIDs = append(endpointIDs, id)
	}
	endpointRows.Close()
	if err := endpointRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate endpoints: %w", err)
	}

	// Create one pending delivery per matching endpoint.
	for _, epID := range endpointIDs {
		_, err = tx.Exec(ctx,
			`INSERT INTO webhook_deliveries
			     (id, event_id, endpoint_id, status, scheduled_at, created_at)
			 VALUES ($1, $2, $3, 'PENDING', now(), now())
			 ON CONFLICT (event_id, endpoint_id) DO NOTHING`,
			uuid.NewString(), eventID, epID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert webhook delivery: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit webhook dispatch: %w", err)
	}

	return &WebhookEvent{
		ID:         eventID,
		MerchantID: req.MerchantID,
		EventType:  req.EventType,
		Payload:    json.RawMessage(envelope),
		CreatedAt:  now,
	}, nil
}

func (s *PostgresWebhookService) ListEvents(
	ctx context.Context,
	merchantID string,
	limit int,
) ([]*WebhookEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, merchant_id, event_type, payload, created_at
		 FROM webhook_events
		 WHERE merchant_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`,
		merchantID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list webhook events: %w", err)
	}
	defer rows.Close()

	var out []*WebhookEvent
	for rows.Next() {
		var ev WebhookEvent
		if err := rows.Scan(
			&ev.ID, &ev.MerchantID, &ev.EventType, &ev.Payload, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook event: %w", err)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

func (s *PostgresWebhookService) ListDeliveries(
	ctx context.Context,
	merchantID, eventID string,
) ([]*WebhookDelivery, error) {
	// RA-060: join to the owning event and filter on merchant_id. Without this
	// any authenticated merchant could read any event's delivery history by id,
	// including the receiver's response body and the endpoint it was sent to.
	//
	// RA-061: status_code and response_body are NULL until an attempt completes,
	// and scanning NULL into int/string fails — so this endpoint returned 500 for
	// its own owner on every pending delivery. COALESCE keeps the wire contract
	// (0 / "") while making the scan total.
	// Ownership is resolved first so a foreign or unknown event is reported as
	// not-found, matching GET /webhooks/endpoints/{id}. Filtering alone would
	// return an empty list, which reads as "no deliveries yet" and quietly tells
	// the caller the id exists.
	var owned bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM webhook_events WHERE id = $1 AND merchant_id = $2)`,
		eventID, merchantID,
	).Scan(&owned); err != nil {
		return nil, fmt.Errorf("resolve webhook event owner: %w", err)
	}
	if !owned {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx,
		`SELECT d.id, d.event_id, d.endpoint_id, d.attempt_count, d.status,
		        COALESCE(d.status_code, 0), COALESCE(d.response_body, ''),
		        d.delivered_at, d.created_at
		 FROM webhook_deliveries d
		 JOIN webhook_events e ON e.id = d.event_id
		 WHERE d.event_id = $1 AND e.merchant_id = $2
		 ORDER BY d.created_at`,
		eventID, merchantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list webhook deliveries: %w", err)
	}
	defer rows.Close()

	var out []*WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(
			&d.ID, &d.EventID, &d.EndpointID, &d.AttemptNumber, &d.Status,
			&d.StatusCode, &d.ResponseBody, &d.DeliveredAt, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook delivery: %w", err)
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Replay — re-queue a permanently-failed delivery
// ---------------------------------------------------------------------------

func (s *PostgresWebhookService) ReplayDelivery(ctx context.Context, merchantID, deliveryID string) (*WebhookDelivery, error) {
	// Verify the delivery exists and belongs to the merchant's endpoint.
	var eventID, endpointID string
	err := s.pool.QueryRow(ctx,
		`SELECT d.event_id, d.endpoint_id
		 FROM webhook_deliveries d
		 JOIN webhook_endpoints ep ON ep.id = d.endpoint_id
		 JOIN webhook_events    e  ON e.id  = d.event_id
		 WHERE d.id = $1 AND ep.merchant_id = $2`,
		deliveryID, merchantID,
	).Scan(&eventID, &endpointID)
	if err != nil {
		return nil, ErrEndpointNotFound
	}

	// Re-queue the EXISTING delivery rather than inserting a second one.
	//
	// This used to INSERT a fresh row, which `webhook_deliveries` forbids: it is
	// UNIQUE on (event_id, endpoint_id), so every replay of a real delivery hit
	// the constraint and answered 500. The endpoint could not succeed at all.
	//
	// The constraint is the design, not an obstacle — one delivery row per event
	// per endpoint, with retries counted on that row (attempt_count), which is
	// exactly what makes an event impossible to deliver as two separate
	// deliveries. So a replay resets the row and lets the dispatcher pick it up,
	// keeping the same delivery identity.
	now := time.Now().UTC()
	var attempts int
	err = s.pool.QueryRow(ctx,
		`UPDATE webhook_deliveries
		    SET status = 'PENDING', scheduled_at = now(), last_error = NULL
		  WHERE id = $1
		RETURNING attempt_count`,
		deliveryID,
	).Scan(&attempts)
	if err != nil {
		return nil, fmt.Errorf("replay delivery: %w", err)
	}

	slog.Info("webhook delivery re-queued", "delivery_id", deliveryID, "attempts_so_far", attempts)

	return &WebhookDelivery{
		ID:            deliveryID,
		EventID:       eventID,
		EndpointID:    endpointID,
		Status:        "PENDING",
		AttemptNumber: attempts,
		CreatedAt:     now,
	}, nil
}

// ---------------------------------------------------------------------------
// EndpointHealth — delivery success/failure stats for the last 24 hours
// ---------------------------------------------------------------------------

func (s *PostgresWebhookService) EndpointHealth(ctx context.Context, merchantID, endpointID string) (*EndpointHealth, error) {
	// Verify the endpoint belongs to the merchant.
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(1) FROM webhook_endpoints WHERE id = $1 AND merchant_id = $2`,
		endpointID, merchantID,
	).Scan(&count)
	if err != nil || count == 0 {
		return nil, ErrEndpointNotFound
	}

	type statsRow struct {
		Total         int
		Success       int
		Failed        int
		LastDelivered *time.Time
		LastFailed    *time.Time
	}

	var stats statsRow
	err = s.pool.QueryRow(ctx,
		`SELECT
		     COUNT(*)                                                        AS total,
		     COUNT(*) FILTER (WHERE status = 'SUCCESS')                     AS success,
		     COUNT(*) FILTER (WHERE status = 'FAILED')                      AS failed,
		     MAX(delivered_at) FILTER (WHERE status = 'SUCCESS')            AS last_delivered,
		     MAX(created_at)   FILTER (WHERE status = 'FAILED')             AS last_failed
		 FROM webhook_deliveries
		 WHERE endpoint_id = $1
		   AND created_at >= NOW() - INTERVAL '24 hours'`,
		endpointID,
	).Scan(&stats.Total, &stats.Success, &stats.Failed, &stats.LastDelivered, &stats.LastFailed)
	if err != nil {
		return nil, fmt.Errorf("endpoint health query: %w", err)
	}

	successRate := 0.0
	if stats.Total > 0 {
		successRate = float64(stats.Success) / float64(stats.Total) * 100
	}

	return &EndpointHealth{
		EndpointID:      endpointID,
		TotalLast24h:    stats.Total,
		SuccessLast24h:  stats.Success,
		FailedLast24h:   stats.Failed,
		SuccessRatePct:  successRate,
		LastDeliveredAt: stats.LastDelivered,
		LastFailedAt:    stats.LastFailed,
	}, nil
}

// ---------------------------------------------------------------------------
// Background delivery worker
// ---------------------------------------------------------------------------

type pendingDelivery struct {
	id           string
	eventID      string
	endpointID   string
	attemptCount int
	maxAttempts  int
	payload      []byte
	url          string
	secret       string
}

// processOutbox fans core-emitted events (the transactional outbox written by
// the Rust core for refund.completed / dispute.opened / dispute.resolved) out
// into per-endpoint deliveries. Rows with dispatched_at IS NULL are awaiting
// fan-out; the inline Dispatch path sets dispatched_at itself and is skipped.
func (s *PostgresWebhookService) processOutbox(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, merchant_id, event_type FROM webhook_events
		 WHERE dispatched_at IS NULL
		 ORDER BY created_at
		 LIMIT 100`,
	)
	if err != nil {
		slog.Error("webhook outbox: query failed", "error", err)
		return
	}
	type outboxEvent struct {
		id, merchantID, eventType string
	}
	var events []outboxEvent
	for rows.Next() {
		var e outboxEvent
		if err := rows.Scan(&e.id, &e.merchantID, &e.eventType); err != nil {
			slog.Error("webhook outbox: scan failed", "error", err)
			continue
		}
		events = append(events, e)
	}
	rows.Close()

	for _, e := range events {
		// Create one PENDING delivery per matching active endpoint.
		_, err := s.pool.Exec(ctx,
			`INSERT INTO webhook_deliveries (id, event_id, endpoint_id, status, scheduled_at, created_at)
			 SELECT gen_random_uuid(), $1, ep.id, 'PENDING', now(), now()
			 FROM webhook_endpoints ep
			 WHERE ep.merchant_id = $2 AND ep.active = true
			   AND ($3 = ANY(ep.events) OR '*' = ANY(ep.events))
			 ON CONFLICT (event_id, endpoint_id) DO NOTHING`,
			e.id, e.merchantID, e.eventType,
		)
		if err != nil {
			slog.Error("webhook outbox: fan-out failed", "event_id", e.id, "error", err)
			continue
		}
		// Mark dispatched even when there are no endpoints — the event is handled.
		if _, err := s.pool.Exec(ctx,
			`UPDATE webhook_events SET dispatched_at = now() WHERE id = $1`, e.id,
		); err != nil {
			slog.Error("webhook outbox: mark dispatched failed", "event_id", e.id, "error", err)
		}
	}
}

func (s *PostgresWebhookService) processPendingDeliveries(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT d.id, d.event_id, d.endpoint_id, d.attempt_count, d.max_attempts,
		        e.payload, ep.url, ep.secret
		 FROM webhook_deliveries d
		 JOIN webhook_events   e  ON e.id  = d.event_id
		 JOIN webhook_endpoints ep ON ep.id = d.endpoint_id
		 WHERE d.status = 'PENDING' AND d.scheduled_at <= NOW()
		 ORDER BY d.scheduled_at
		 LIMIT 50
		 FOR UPDATE OF d SKIP LOCKED`,
	)
	if err != nil {
		slog.Error("webhook worker: query failed", "error", err)
		return
	}
	defer rows.Close()

	var deliveries []pendingDelivery
	for rows.Next() {
		var d pendingDelivery
		if err := rows.Scan(
			&d.id, &d.eventID, &d.endpointID, &d.attemptCount, &d.maxAttempts,
			&d.payload, &d.url, &d.secret,
		); err != nil {
			slog.Error("webhook worker: scan failed", "error", err)
			continue
		}
		deliveries = append(deliveries, d)
	}
	rows.Close()

	for _, d := range deliveries {
		go s.attemptDelivery(ctx, d)
	}
}

func (s *PostgresWebhookService) attemptDelivery(ctx context.Context, d pendingDelivery) {
	now := time.Now().UTC()
	attempt := d.attemptCount + 1

	// Decrypt the at-rest signing secret to sign this delivery (SEC-002).
	// Legacy plaintext secrets pass through unchanged.
	secret, err := s.cipher.Decrypt(d.secret)
	if err != nil {
		slog.Error("[webhook] could not decrypt signing secret", "delivery_id", d.id, "error", err)
		secret = d.secret
	}
	statusCode, respBody, deliveryErr := s.httpPost(d.url, secret, now, d.payload)

	if deliveryErr == nil && statusCode < 400 {
		// Success — mark terminal.
		_, err := s.pool.Exec(ctx,
			`UPDATE webhook_deliveries
			 SET status       = 'SUCCESS',
			     attempt_count = $2,
			     status_code  = $3,
			     response_body = $4,
			     delivered_at = $5
			 WHERE id = $1`,
			d.id, attempt, statusCode, respBody, now,
		)
		if err != nil {
			slog.Error("webhook worker: success update failed", "delivery_id", d.id, "error", err)
		}
		slog.Info("webhook delivered", "delivery_id", d.id, "url", d.url, "attempt", attempt)
		return
	}

	// Failure — schedule retry or mark permanently failed.
	errMsg := ""
	if deliveryErr != nil {
		errMsg = deliveryErr.Error()
	}

	if attempt >= d.maxAttempts {
		_, err := s.pool.Exec(ctx,
			`UPDATE webhook_deliveries
			 SET status        = 'FAILED',
			     attempt_count = $2,
			     status_code   = $3,
			     response_body = $4,
			     last_error    = $5
			 WHERE id = $1`,
			d.id, attempt, statusCode, respBody, errMsg,
		)
		if err != nil {
			slog.Error("webhook worker: fail update failed", "delivery_id", d.id, "error", err)
		}
		slog.Warn("webhook permanently failed", "delivery_id", d.id, "url", d.url, "attempts", attempt)
		return
	}

	// Schedule next retry with exponential backoff.
	backoffIdx := attempt - 1
	if backoffIdx >= len(backoffSchedule) {
		backoffIdx = len(backoffSchedule) - 1
	}
	nextAt := now.Add(backoffSchedule[backoffIdx])

	_, err = s.pool.Exec(ctx,
		`UPDATE webhook_deliveries
		 SET attempt_count = $2,
		     status_code   = $3,
		     response_body = $4,
		     last_error    = $5,
		     scheduled_at  = $6
		 WHERE id = $1`,
		d.id, attempt, statusCode, respBody, errMsg, nextAt,
	)
	if err != nil {
		slog.Error("webhook worker: retry schedule failed", "delivery_id", d.id, "error", err)
	}
	slog.Warn("webhook delivery failed, will retry",
		"delivery_id", d.id, "url", d.url,
		"attempt", attempt, "next_at", nextAt,
	)
}

func (s *PostgresWebhookService) httpPost(
	url, secret string,
	t time.Time,
	payload []byte,
) (statusCode int, body string, err error) {
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(payload),
	)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, t, payload))
	req.Header.Set("User-Agent", "Banzami-Webhook/1.0")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, string(raw), nil
}
