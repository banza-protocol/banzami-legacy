use axum::{
    extract::{Path, State},
    http::StatusCode,
    Json,
};
use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use sqlx::Row as _;
use uuid::Uuid;

use crate::{
    error::{ApiError, ApiResult},
    routes::risk,
    state::AppState,
};

// Local row type for the FOR UPDATE fetch — uses runtime query (not query!)
// to avoid sqlx offline-cache dependency for the FOR UPDATE clause.
struct LockedLinkRow {
    id: Uuid,
    receiver_consumer_id: Uuid,
    amount_minor: Option<i64>,
    note: Option<String>,
    currency: String,
    locked: bool,
    status: String,
    expires_at: Option<DateTime<Utc>>,
}

// ---------------------------------------------------------------------------
// Code generation — 8 random chars from unambiguous alphanumeric set
// ---------------------------------------------------------------------------

fn generate_link_code() -> String {
    let bytes = Uuid::new_v4().into_bytes();
    let chars: &[u8] = b"ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    bytes[..8]
        .iter()
        .map(|b| chars[(*b as usize) % chars.len()] as char)
        .collect()
}

// ---------------------------------------------------------------------------
// Response type
// ---------------------------------------------------------------------------

#[derive(Serialize)]
pub struct ConsumerPayLinkResponse {
    pub id: String,
    pub link_code: String,
    pub receiver_consumer_id: String,
    pub receiver_handle: String,
    pub receiver_display_name: Option<String>,
    pub amount_minor: Option<i64>,
    pub note: Option<String>,
    pub currency: String,
    pub locked: bool,
    pub status: String,
    pub payer_consumer_id: Option<String>,
    pub transfer_id: Option<String>,
    pub expires_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub paid_at: Option<DateTime<Utc>>,
}

// ---------------------------------------------------------------------------
// POST /internal/v1/consumer-pay-links — create
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct CreateConsumerPayLinkBody {
    pub receiver_consumer_id: String,
    pub amount_minor: Option<i64>,
    pub note: Option<String>,
    pub currency: Option<String>,
    pub locked: Option<bool>,
    pub expires_in_hours: Option<i64>,
}

pub async fn create(
    State(state): State<AppState>,
    Json(body): Json<CreateConsumerPayLinkBody>,
) -> ApiResult<(StatusCode, Json<ConsumerPayLinkResponse>)> {
    let receiver_id: Uuid = body
        .receiver_consumer_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid receiver_consumer_id"))?;

    if let Some(amt) = body.amount_minor {
        if amt <= 0 {
            return Err(ApiError::bad_request("amount_minor must be positive"));
        }
    }

    let status: Option<String> =
        sqlx::query_scalar!("SELECT status FROM consumers WHERE id = $1", receiver_id,)
            .fetch_optional(&state.pool)
            .await
            .map_err(|e| ApiError::internal(e.to_string()))?;

    match status.as_deref() {
        None => return Err(ApiError::not_found("receiver not found")),
        Some(s) if s != "ACTIVE" => {
            return Err(ApiError::unprocessable(
                "CONSUMER_NOT_ACTIVE",
                "receiver account is not active",
            ))
        }
        _ => {}
    }

    let currency = body.currency.unwrap_or_else(|| "AOA".into());
    let locked = body.locked.unwrap_or(true);
    let expires_at = body
        .expires_in_hours
        .map(|h| Utc::now() + Duration::hours(h));

    // Generate a unique link code (retry on rare collision)
    let mut link_code = generate_link_code();
    for _ in 0..5 {
        let exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS(SELECT 1 FROM consumer_pay_links WHERE link_code = $1)",
            link_code,
        )
        .fetch_one(&state.pool)
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?
        .unwrap_or(false);
        if !exists {
            break;
        }
        link_code = generate_link_code();
    }

    let id = Uuid::new_v4();
    sqlx::query!(
        r#"
        INSERT INTO consumer_pay_links
            (id, link_code, receiver_consumer_id, amount_minor, note, currency, locked, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        "#,
        id,
        link_code,
        receiver_id,
        body.amount_minor,
        body.note,
        currency,
        locked,
        expires_at,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    Ok((
        StatusCode::CREATED,
        Json(fetch_by_id(&state.pool, id).await?),
    ))
}

// ---------------------------------------------------------------------------
// GET /internal/v1/consumer-pay-links/by-code/:code — public lookup
// ---------------------------------------------------------------------------

pub async fn get_by_code(
    State(state): State<AppState>,
    Path(code): Path<String>,
) -> ApiResult<Json<ConsumerPayLinkResponse>> {
    let link = fetch_by_code(&state.pool, &code).await?;

    // Auto-expire if past deadline
    if link.status == "ACTIVE" {
        if let Some(exp) = link.expires_at {
            if Utc::now() > exp {
                sqlx::query!(
                    "UPDATE consumer_pay_links SET status = 'EXPIRED' WHERE link_code = $1",
                    code,
                )
                .execute(&state.pool)
                .await
                .ok();
                return Ok(Json(ConsumerPayLinkResponse {
                    status: "EXPIRED".into(),
                    ..link
                }));
            }
        }
    }

    Ok(Json(link))
}

// ---------------------------------------------------------------------------
// POST /internal/v1/consumer-pay-links/:code/pay — authenticated payer pays
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct PayConsumerPayLinkBody {
    pub payer_consumer_id: String,
    pub amount_minor: Option<i64>,
    pub idempotency_key: String,
}

pub async fn pay(
    State(state): State<AppState>,
    Path(code): Path<String>,
    Json(body): Json<PayConsumerPayLinkBody>,
) -> ApiResult<Json<ConsumerPayLinkResponse>> {
    let payer_id: Uuid = body
        .payer_consumer_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid payer_consumer_id"))?;

    // ── Pre-flight checks (no row lock yet) ────────────────────────────────
    // These fast checks reject obviously invalid requests before taking a lock.

    if risk::is_frozen(&state.pool, "CONSUMER", payer_id).await {
        return Err(ApiError::unprocessable(
            "ACCOUNT_FROZEN",
            "payer account is frozen",
        ));
    }

    let pre = sqlx::query!(
        r#"
        SELECT id, receiver_consumer_id, amount_minor, currency,
               locked, status, expires_at
        FROM consumer_pay_links
        WHERE link_code = $1
        "#,
        code,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::not_found("consumer pay link not found"))?;

    if pre.status != "ACTIVE" {
        return Err(ApiError::unprocessable(
            "LINK_NOT_ACTIVE",
            format!("link status is {}", pre.status),
        ));
    }

    if let Some(exp) = pre.expires_at {
        if Utc::now() > exp {
            sqlx::query!(
                "UPDATE consumer_pay_links SET status = 'EXPIRED' WHERE link_code = $1",
                code,
            )
            .execute(&state.pool)
            .await
            .ok();
            return Err(ApiError::unprocessable("LINK_EXPIRED", "link has expired"));
        }
    }

    if payer_id == pre.receiver_consumer_id {
        return Err(ApiError::bad_request("cannot pay your own link"));
    }

    // ── Begin transaction — acquires row-level lock ─────────────────────────
    // SELECT ... FOR UPDATE prevents concurrent payment attempts from racing
    // past the status check. The second concurrent request blocks here until
    // the first commits, then sees status = 'PAID' and returns LINK_NOT_ACTIVE.
    let mut tx = state
        .pool
        .begin()
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;

    // Runtime query (not query!) so FOR UPDATE doesn't need an offline cache entry.
    let row = sqlx::query(
        "SELECT id, receiver_consumer_id, amount_minor, note, currency, locked, status, expires_at \
         FROM consumer_pay_links WHERE link_code = $1 FOR UPDATE",
    )
    .bind(&code)
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::not_found("consumer pay link not found"))?;

    let link = LockedLinkRow {
        id: row
            .try_get("id")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        receiver_consumer_id: row
            .try_get("receiver_consumer_id")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        amount_minor: row
            .try_get("amount_minor")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        note: row
            .try_get("note")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        currency: row
            .try_get("currency")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        locked: row
            .try_get("locked")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        status: row
            .try_get("status")
            .map_err(|e| ApiError::internal(e.to_string()))?,
        expires_at: row
            .try_get("expires_at")
            .map_err(|e| ApiError::internal(e.to_string()))?,
    };

    // Re-check under lock — another transaction may have paid between pre-flight and now.
    if link.status != "ACTIVE" {
        return Err(ApiError::unprocessable(
            "LINK_NOT_ACTIVE",
            format!("link status is {}", link.status),
        ));
    }

    if let Some(exp) = link.expires_at {
        if Utc::now() > exp {
            sqlx::query!(
                "UPDATE consumer_pay_links SET status = 'EXPIRED' WHERE link_code = $1",
                code,
            )
            .execute(&mut *tx)
            .await
            .ok();
            tx.commit()
                .await
                .map_err(|e| ApiError::internal(e.to_string()))?;
            return Err(ApiError::unprocessable("LINK_EXPIRED", "link has expired"));
        }
    }

    // ── Amount resolution ──────────────────────────────────────────────────
    // For locked links the server-stored amount is authoritative; client value
    // is silently ignored, preventing tampered-amount attacks.
    let amount = if link.locked {
        link.amount_minor
            .ok_or_else(|| ApiError::bad_request("locked link has no amount set"))?
    } else {
        body.amount_minor
            .filter(|&a| a > 0)
            .ok_or_else(|| ApiError::bad_request("amount_minor required for open links"))?
    };

    // ── Wallet and balance checks ──────────────────────────────────────────
    let payer_wallet = sqlx::query!(
        "SELECT id, available_account_id FROM consumer_wallets
         WHERE consumer_id = $1 AND currency = $2 AND status = 'ACTIVE'",
        payer_id,
        link.currency,
    )
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::unprocessable("WALLET_NOT_FOUND", "payer has no active wallet"))?;

    let receiver_wallet = sqlx::query!(
        "SELECT id, available_account_id FROM consumer_wallets
         WHERE consumer_id = $1 AND currency = $2 AND status = 'ACTIVE'",
        link.receiver_consumer_id,
        link.currency,
    )
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::unprocessable("WALLET_NOT_FOUND", "receiver has no active wallet"))?;

    let payer_balance: i64 = sqlx::query_scalar!(
        r#"
        SELECT COALESCE(
            (SELECT SUM(CASE WHEN entry_type = 'CREDIT' THEN amount_minor ELSE -amount_minor END)
             FROM ledger_entries WHERE account_id = $1),
            0
        )::BIGINT
        "#,
        payer_wallet.available_account_id,
    )
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .unwrap_or(0);

    if payer_balance < amount {
        return Err(ApiError::unprocessable(
            "INSUFFICIENT_FUNDS",
            format!("available {payer_balance}, requested {amount}"),
        ));
    }

    // ── Double-entry ledger posting ────────────────────────────────────────
    let transfer_id = Uuid::new_v4();
    let now = Utc::now();
    let ledger_key = format!("consumer-pay-link-{}", link.id);

    let posting_id = Uuid::new_v4();
    sqlx::query!(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, $2, $3, $4) ON CONFLICT (idempotency_key) DO NOTHING",
        posting_id,
        format!("consumer-pay-link:{}", link.id),
        ledger_key,
        now,
    )
    .execute(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let actual_posting: Uuid = sqlx::query_scalar!(
        "SELECT id FROM ledger_postings WHERE idempotency_key = $1",
        ledger_key,
    )
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    // uq_ledger_entry_posting_type (migration 0040) enforces at most one DEBIT
    // and one CREDIT per posting — ON CONFLICT DO NOTHING catches that constraint.
    sqlx::query!(
        "INSERT INTO ledger_entries
             (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'DEBIT', $4, $5, $6) ON CONFLICT DO NOTHING",
        Uuid::new_v4(),
        actual_posting,
        payer_wallet.available_account_id,
        amount,
        link.currency,
        now,
    )
    .execute(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    sqlx::query!(
        "INSERT INTO ledger_entries
             (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'CREDIT', $4, $5, $6) ON CONFLICT DO NOTHING",
        Uuid::new_v4(),
        actual_posting,
        receiver_wallet.available_account_id,
        amount,
        link.currency,
        now,
    )
    .execute(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    // Runtime query (no !) so adding `description` doesn't need a new sqlx cache entry.
    // link.note propagates here so the activity feed shows the pay-link note.
    sqlx::query(
        "INSERT INTO transfers
             (id, idempotency_key, sender_id, recipient_id,
              amount_minor, currency, status, ledger_posting_id, description,
              environment, created_at, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, 'COMPLETED', $7, $8, $9, $10, $10)
         ON CONFLICT (idempotency_key) DO NOTHING",
    )
    .bind(transfer_id)
    .bind(&body.idempotency_key)
    .bind(payer_id)
    .bind(link.receiver_consumer_id)
    .bind(amount)
    .bind(&link.currency)
    .bind(actual_posting)
    .bind(&link.note)  // Option<String> — NULL when no note
    // From the process, never the payer and never the column default. The default
    // is 'LIVE', so omitting it here recorded Sandbox pay-link settlements as real
    // money — invisible to the Sandbox proof lookup that should have found them.
    .bind(state.environment.as_str())
    .bind(now)
    .execute(&mut *tx)
    .await
    .ok();

    // WHERE status = 'ACTIVE' is a final safety gate; under the FOR UPDATE lock
    // this UPDATE should always match exactly one row at this point.
    sqlx::query!(
        r#"
        UPDATE consumer_pay_links
        SET status = 'PAID', payer_consumer_id = $1, transfer_id = $2, paid_at = $3
        WHERE link_code = $4 AND status = 'ACTIVE'
        "#,
        payer_id,
        transfer_id,
        now,
        code,
    )
    .execute(&mut *tx)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    tx.commit()
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;

    // Audit is async and non-critical; runs after the transaction commits.
    risk::audit(
        &state.pool,
        "CONSUMER",
        "CONSUMER_PAY_LINK_PAID",
        &format!("consumer:{payer_id}"),
        serde_json::json!({
            "link_code":    code,
            "receiver_id":  link.receiver_consumer_id,
            "amount_minor": amount,
        }),
        None,
    )
    .await;

    Ok(Json(fetch_by_code(&state.pool, &code).await?))
}

// ---------------------------------------------------------------------------
// Shared fetch helpers
// ---------------------------------------------------------------------------

async fn fetch_by_id(pool: &sqlx::PgPool, id: Uuid) -> ApiResult<ConsumerPayLinkResponse> {
    sqlx::query!(
        r#"
        SELECT cpl.id, cpl.link_code, cpl.receiver_consumer_id,
               cpl.amount_minor, cpl.note, cpl.currency, cpl.locked, cpl.status,
               cpl.payer_consumer_id, cpl.transfer_id, cpl.expires_at,
               cpl.created_at, cpl.paid_at,
               rc.handle          AS receiver_handle,
               rc.display_name    AS receiver_display_name
        FROM consumer_pay_links cpl
        JOIN consumers rc ON rc.id = cpl.receiver_consumer_id
        WHERE cpl.id = $1
        "#,
        id,
    )
    .fetch_optional(pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .map(|r| ConsumerPayLinkResponse {
        id: r.id.to_string(),
        link_code: r.link_code,
        receiver_consumer_id: r.receiver_consumer_id.to_string(),
        receiver_handle: r.receiver_handle,
        receiver_display_name: r.receiver_display_name,
        amount_minor: r.amount_minor,
        note: r.note,
        currency: r.currency,
        locked: r.locked,
        status: r.status,
        payer_consumer_id: r.payer_consumer_id.map(|u| u.to_string()),
        transfer_id: r.transfer_id.map(|u| u.to_string()),
        expires_at: r.expires_at,
        created_at: r.created_at,
        paid_at: r.paid_at,
    })
    .ok_or_else(|| ApiError::not_found("consumer pay link not found"))
}

async fn fetch_by_code(pool: &sqlx::PgPool, code: &str) -> ApiResult<ConsumerPayLinkResponse> {
    sqlx::query!(
        r#"
        SELECT cpl.id, cpl.link_code, cpl.receiver_consumer_id,
               cpl.amount_minor, cpl.note, cpl.currency, cpl.locked, cpl.status,
               cpl.payer_consumer_id, cpl.transfer_id, cpl.expires_at,
               cpl.created_at, cpl.paid_at,
               rc.handle          AS receiver_handle,
               rc.display_name    AS receiver_display_name
        FROM consumer_pay_links cpl
        JOIN consumers rc ON rc.id = cpl.receiver_consumer_id
        WHERE cpl.link_code = $1
        "#,
        code,
    )
    .fetch_optional(pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .map(|r| ConsumerPayLinkResponse {
        id: r.id.to_string(),
        link_code: r.link_code,
        receiver_consumer_id: r.receiver_consumer_id.to_string(),
        receiver_handle: r.receiver_handle,
        receiver_display_name: r.receiver_display_name,
        amount_minor: r.amount_minor,
        note: r.note,
        currency: r.currency,
        locked: r.locked,
        status: r.status,
        payer_consumer_id: r.payer_consumer_id.map(|u| u.to_string()),
        transfer_id: r.transfer_id.map(|u| u.to_string()),
        expires_at: r.expires_at,
        created_at: r.created_at,
        paid_at: r.paid_at,
    })
    .ok_or_else(|| ApiError::not_found("consumer pay link not found"))
}
