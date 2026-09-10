# Banza Operations Runbooks

Runbooks are step-by-step procedures for common operational tasks. They describe **how** to perform an action safely, not why the system is designed a certain way (that is in the ADRs).

---

## Index

| Runbook | Purpose |
|---------|---------|
| [RB-001: Deployment](#rb-001-deployment) | Deploy a service to production |
| [RB-002: Database Migrations](#rb-002-database-migrations) | Run and verify migrations |
| [RB-003: Service Health Check](#rb-003-service-health-check) | Verify all services are healthy |
| [RB-004: Payment Link Expiry Worker](#rb-004-payment-link-expiry-worker) | Verify and restart the expiry worker |
| [RB-005: Consumer PIN Reset](#rb-005-consumer-pin-reset) | Manually reset a consumer PIN |
| [RB-006: Merchant API Key Revocation](#rb-006-merchant-api-key-revocation) | Revoke a compromised API key |
| [RB-007: Incident — Duplicate Ledger Entry](#rb-007-incident-duplicate-ledger-entry) | Investigate and resolve duplicate entries |

## Release runbooks

| Runbook | Purpose |
|---------|---------|
| [Release A cutover](release-a-cutover.md) | The order code, the environment repair (0113) and the historical proof backfill must run in, and the two windows that order exists to close |

---

## RB-001: Deployment

### Pre-flight

```bash
# Verify all tests pass
cargo test --workspace
go test ./... (run in each service directory)

# Verify no uncommitted migrations
git status db/migrations/
```

### Build Docker images

```bash
docker build -t banzami/core-api:$(git rev-parse --short HEAD)    ./core
docker build -t banzami/api-gateway:$(git rev-parse --short HEAD)  ./services/api-gateway
docker build -t banzami/public-api:$(git rev-parse --short HEAD)   ./services/public-api
docker build -t banzami/admin-api:$(git rev-parse --short HEAD)    ./services/admin-api
```

### Deploy order

Always deploy in this order to avoid breaking in-flight requests:

1. Run database migrations (RB-002) **before** deploying services.
2. Deploy `core-api` first (other services call it).
3. Deploy `api-gateway`, `public-api`, `admin-api` in any order.
4. Deploy `apps/pay`, `apps/dashboard` last.

### Verify after deploy

```bash
# Check liveness of each service
curl -fsS http://localhost:8080/health  # api-gateway
curl -fsS http://localhost:8083/health  # public-api
curl -fsS http://localhost:8082/health  # admin-api
curl -fsS http://localhost:8081/health  # core-api

# Check Prometheus metrics endpoint
curl -fsS http://localhost:8080/metrics | grep http_server
```

---

## RB-002: Database Migrations

Migrations are sequential SQL files in `db/migrations/`. They must be run in order. Use `sqlx migrate` or `psql` directly.

### Run with sqlx CLI

```bash
DATABASE_URL="postgres://banzami:banzami_dev@localhost:5433/banzami_dev" \
  sqlx migrate run --source db/migrations
```

### Verify migrations applied

```bash
psql $DATABASE_URL -c "SELECT version, description, installed_on FROM _sqlx_migrations ORDER BY version;"
```

### Rollback policy

Migrations are **not** automatically reversible. If a migration must be reverted:

1. Write a new migration (`0NNN_revert_xxx.sql`) that undoes the change.
2. Never delete or edit a committed migration file.
3. Destructive rollbacks (DROP TABLE) require manual verification that no data will be lost.

---

## RB-003: Service Health Check

Run this after any deployment or when an alert fires.

```bash
#!/usr/bin/env bash
set -euo pipefail

services=(
  "api-gateway  http://localhost:8080/health"
  "public-api   http://localhost:8083/health"
  "admin-api    http://localhost:8082/health"
  "core-api     http://localhost:8081/health"
)

for entry in "${services[@]}"; do
  name=$(echo "$entry" | awk '{print $1}')
  url=$(echo "$entry" | awk '{print $2}')
  if curl -fsS --max-time 5 "$url" > /dev/null; then
    echo "✓ $name OK"
  else
    echo "✗ $name FAILED — check logs: docker logs banzami-$name"
  fi
done
```

Check Prometheus for error rates:

```
rate(http_server_duration_count{status_code=~"5.."}[5m]) > 0.01
```

---

## RB-004: Payment Link Expiry Worker

The expiry worker runs inside `core-api` and ticks every 60 seconds (configurable via `PAYMENT_LINK_EXPIRY_INTERVAL_SECS`).

### Verify it is running

```bash
# Check core-api logs for the expiry worker
docker logs banzami-core-api | grep "payment_link_expiry"
```

Expected output every 60 seconds:
```json
{"level":"info","message":"payment link expiry worker tick","expired":0}
```

### Force a manual expiry run

```bash
# Direct SQL — use with caution; only in emergencies
psql $DATABASE_URL << 'EOF'
UPDATE payment_links
SET status = 'EXPIRED', updated_at = NOW()
WHERE status = 'ACTIVE'
  AND expires_at IS NOT NULL
  AND expires_at <= NOW();
SELECT ROW_COUNT();
EOF
```

### Restart if stalled

```bash
docker restart banzami-core-api
# Verify restart
sleep 5 && curl -fsS http://localhost:8081/health
```

---

## RB-005: Consumer PIN Reset

There is no self-service PIN reset in V1. This procedure requires operator access.

### Prerequisites

- Verify consumer identity via out-of-band channel (phone call, document scan).
- Log the reset action with operator name, timestamp, and verification method.

### Procedure

```sql
-- 1. Find the consumer record
SELECT id, handle, status FROM consumers WHERE handle = '<handle>';

-- 2. Verify the consumer is ACTIVE and not suspended
-- If status != 'ACTIVE', resolve the account status first.

-- 3. Delete the old credential (forces re-registration)
-- The consumer must register again with a new PIN via POST /v1/auth/register
-- with the same handle — this will fail because the handle is taken.
-- Correct approach: update the pin_hash directly.

-- 4. Generate a temporary PIN and set it (replace 'TEMP_HASH' with bcrypt output)
-- Use: htpasswd -nbBC 10 "" "TEMP_PIN" | tr -d ':\n' | sed 's/$2y/$2a/'
-- Or use a bcrypt CLI tool.
UPDATE public_api_credentials
SET pin_hash = '<bcrypt_hash_of_temp_pin>',
    created_at = NOW()
WHERE handle = '<handle>';

-- 5. Verify
SELECT consumer_id, handle, created_at FROM public_api_credentials WHERE handle = '<handle>';
```

After the PIN is reset, communicate the temporary PIN to the consumer via the verified out-of-band channel. Instruct them to change it immediately.

---

## RB-006: Merchant API Key Revocation

### Via dashboard (preferred)

1. Log in to `admin.banzami.com`.
2. Navigate to Merchants → Select merchant → API Keys.
3. Click "Revoke" on the compromised key.

### Via admin-api

```bash
curl -X DELETE "http://admin.internal:8082/admin/v1/merchants/{merchant_id}/api-keys/{key_id}" \
  -H "X-Admin-Key: $ADMIN_API_KEY"
```

### Verify revocation

```bash
# Attempt to exchange the revoked key — must return 401
curl -X POST "https://api.banzami.com/v1/auth/token" \
  -H "Content-Type: application/json" \
  -d '{"api_key": "<revoked_key>"}'
# Expected: 401 KEY_REVOKED
```

### Notify merchant

After revocation, notify the merchant via their registered email that their key was revoked and instruct them to issue a new one from the dashboard.

---

## RB-007: Incident — Duplicate Ledger Entry

The double-entry ledger has idempotency guards at the database level. This runbook handles the (extremely rare) case where a duplicate entry is suspected.

### Investigate

```sql
-- Check for duplicate idempotency keys
SELECT idempotency_key, COUNT(*) as count
FROM ledger_postings
GROUP BY idempotency_key
HAVING COUNT(*) > 1;

-- If duplicates found, inspect the postings
SELECT * FROM ledger_postings WHERE idempotency_key = '<key>';
SELECT * FROM ledger_entries WHERE posting_id IN (
  SELECT id FROM ledger_postings WHERE idempotency_key = '<key>'
);
```

### Expected: no duplicates

The `UNIQUE` constraint on `(idempotency_key)` in `ledger_postings` prevents true duplicates at the database level. If the query above returns rows, it means two different idempotency keys resulted in the same economic effect.

### Response

1. **Do not delete ledger entries.** The ledger is append-only — deletions destroy the audit trail.
2. Record the duplicate posting IDs and affected account IDs.
3. Create a correcting posting (reverse entry) if the duplicate caused a balance error:
   ```
   Correcting posting:
   - Credit the account that was erroneously debited
   - Debit the account that was erroneously credited
   - Mark idempotency key as 'correction-{original_key}'
   ```
4. Open a post-mortem ticket to identify the root cause.

---

## Environment Variables Reference

| Variable | Service | Default | Description |
|----------|---------|---------|-------------|
| `DATABASE_URL` | core-api, api-gateway, public-api | — | PostgreSQL connection string |
| `REDIS_URL` | api-gateway | — | Redis connection string |
| `JWT_SECRET` | api-gateway, public-api | — | HMAC key for JWT signing |
| `ADMIN_API_KEY` | admin-api | — | Static key for admin access |
| `CORE_API_URL` | api-gateway, public-api, admin-api | `http://127.0.0.1:8081` | Rust core URL |
| `PUBLIC_API_PORT` | public-api | `8083` | Listening port |
| `OTLP_ENDPOINT` | all services | `` (disabled) | OTLP collector endpoint |
| `LOG_LEVEL` | all services | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | all services | `json` | `json` or `pretty` |
| `PAYMENT_LINK_EXPIRY_INTERVAL_SECS` | core-api | `60` | Expiry worker interval |
