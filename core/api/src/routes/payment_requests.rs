use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    Json,
};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::{
    error::{ApiError, ApiResult},
    routes::risk,
    state::AppState,
};

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

#[derive(Serialize)]
pub struct PaymentRequestResponse {
    pub id: String,
    pub requester_id: String,
    pub payer_id: String,
    pub amount_minor: i64,
    pub currency: String,
    pub message: Option<String>,
    pub status: String,
    pub transfer_id: Option<String>,
    pub expires_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub paid_at: Option<DateTime<Utc>>,
    pub declined_at: Option<DateTime<Utc>>,
    // Enriched view — present because FK guarantees consumer exists
    pub requester_handle: Option<String>,
    pub payer_handle: Option<String>,
}

// ---------------------------------------------------------------------------
// POST /internal/v1/payment-requests — create request
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct CreateRequestBody {
    pub requester_id: String,
    pub payer_id: String,
    pub amount_minor: i64,
    pub currency: Option<String>,
    pub message: Option<String>,
    pub idempotency_key: Option<String>,
}

pub async fn create(
    State(state): State<AppState>,
    Json(body): Json<CreateRequestBody>,
) -> ApiResult<(StatusCode, Json<PaymentRequestResponse>)> {
    if body.amount_minor <= 0 {
        return Err(ApiError::bad_request("amount_minor must be positive"));
    }

    let requester_id: Uuid = body
        .requester_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid requester_id"))?;
    let payer_id: Uuid = body
        .payer_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid payer_id"))?;

    if requester_id == payer_id {
        return Err(ApiError::bad_request("cannot request money from yourself"));
    }

    let currency = body.currency.unwrap_or_else(|| "AOA".into());

    // Ensure both consumers exist and are ACTIVE
    for (label, cid) in [("requester", requester_id), ("payer", payer_id)] {
        let status: Option<String> =
            sqlx::query_scalar!("SELECT status FROM consumers WHERE id = $1", cid,)
                .fetch_optional(&state.pool)
                .await
                .map_err(|e| ApiError::internal(e.to_string()))?;

        match status.as_deref() {
            None => return Err(ApiError::not_found(format!("{label} not found"))),
            Some(s) if s != "ACTIVE" => {
                return Err(ApiError::unprocessable(
                    "CONSUMER_NOT_ACTIVE",
                    format!("{label} account is not active"),
                ))
            }
            _ => {}
        }
    }

    // Check for account freezes
    if risk::is_frozen(&state.pool, "CONSUMER", requester_id).await
        || risk::is_frozen(&state.pool, "CONSUMER", payer_id).await
    {
        return Err(ApiError::unprocessable(
            "ACCOUNT_FROZEN",
            "one or both accounts are frozen",
        ));
    }

    let request_id = Uuid::new_v4();
    let idempotency_key = body
        .idempotency_key
        .unwrap_or_else(|| Uuid::new_v4().to_string());

    // Idempotent insert
    sqlx::query!(
        r#"
        INSERT INTO payment_requests
            (id, requester_id, payer_id, amount_minor, currency, message, idempotency_key)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (idempotency_key) DO NOTHING
        "#,
        request_id,
        requester_id,
        payer_id,
        body.amount_minor,
        currency,
        body.message,
        idempotency_key,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let actual_id: Uuid = sqlx::query_scalar!(
        "SELECT id FROM payment_requests WHERE idempotency_key = $1",
        idempotency_key,
    )
    .fetch_one(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    Ok((
        StatusCode::CREATED,
        Json(fetch_request(&state.pool, actual_id).await?),
    ))
}

// ---------------------------------------------------------------------------
// POST /internal/v1/payment-requests/:id/pay — payer approves and pays
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct PayRequestBody {
    pub payer_id: String,
    pub idempotency_key: String,
}

pub async fn pay(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(body): Json<PayRequestBody>,
) -> ApiResult<Json<PaymentRequestResponse>> {
    let request_id: Uuid = id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid request id"))?;
    let payer_id: Uuid = body
        .payer_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid payer_id"))?;

    let req = sqlx::query!(
        r#"
        SELECT id, requester_id, payer_id, amount_minor, currency, status
        FROM payment_requests
        WHERE id = $1 AND payer_id = $2
        "#,
        request_id,
        payer_id,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::not_found("payment request not found for this payer"))?;

    if req.status != "PENDING" {
        return Err(ApiError::unprocessable(
            "REQUEST_NOT_PENDING",
            format!("payment request status is {}", req.status),
        ));
    }

    // Check expiry
    let expires_at: DateTime<Utc> = sqlx::query_scalar!(
        "SELECT expires_at FROM payment_requests WHERE id = $1",
        request_id,
    )
    .fetch_one(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    if Utc::now() > expires_at {
        sqlx::query!(
            "UPDATE payment_requests SET status = 'EXPIRED', updated_at = NOW() WHERE id = $1",
            request_id,
        )
        .execute(&state.pool)
        .await
        .ok();
        return Err(ApiError::unprocessable(
            "REQUEST_EXPIRED",
            "payment request has expired",
        ));
    }

    // Check freezes
    if risk::is_frozen(&state.pool, "CONSUMER", payer_id).await {
        return Err(ApiError::unprocessable(
            "ACCOUNT_FROZEN",
            "payer account is frozen",
        ));
    }

    // Find payer and requester wallets
    let payer_wallet = sqlx::query!(
        "SELECT id, available_account_id FROM consumer_wallets
         WHERE consumer_id = $1 AND currency = $2 AND status = 'ACTIVE'",
        payer_id,
        req.currency,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::unprocessable("WALLET_NOT_FOUND", "payer has no active wallet"))?;

    let requester_wallet = sqlx::query!(
        "SELECT id, available_account_id FROM consumer_wallets
         WHERE consumer_id = $1 AND currency = $2 AND status = 'ACTIVE'",
        req.requester_id,
        req.currency,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(|| ApiError::unprocessable("WALLET_NOT_FOUND", "requester has no active wallet"))?;

    // Check payer balance
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
    .fetch_one(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .unwrap_or(0);

    if payer_balance < req.amount_minor {
        return Err(ApiError::unprocessable(
            "INSUFFICIENT_FUNDS",
            format!("available {payer_balance}, requested {}", req.amount_minor),
        ));
    }

    // Execute the transfer via ledger posting
    let transfer_id = Uuid::new_v4();
    let now = Utc::now();
    let ledger_key = format!("payment-request-{request_id}");

    let posting_id = Uuid::new_v4();
    sqlx::query!(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, $2, $3, $4) ON CONFLICT (idempotency_key) DO NOTHING",
        posting_id,
        format!("payment-request:{request_id}"),
        ledger_key,
        now,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let actual_posting: Uuid = sqlx::query_scalar!(
        "SELECT id FROM ledger_postings WHERE idempotency_key = $1",
        ledger_key,
    )
    .fetch_one(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    // DR payer, CR requester
    sqlx::query!(
        "INSERT INTO ledger_entries (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'DEBIT', $4, $5, $6) ON CONFLICT DO NOTHING",
        Uuid::new_v4(), actual_posting, payer_wallet.available_account_id, req.amount_minor, req.currency, now,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    sqlx::query!(
        "INSERT INTO ledger_entries (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'CREDIT', $4, $5, $6) ON CONFLICT DO NOTHING",
        Uuid::new_v4(), actual_posting, requester_wallet.available_account_id, req.amount_minor, req.currency, now,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    // Record transfer (sender_id/recipient_id are consumer UUIDs, per 0012_transfers_schema.sql)
    sqlx::query!(
        r#"
        INSERT INTO transfers
            (id, idempotency_key, sender_id, recipient_id,
             amount_minor, currency, status, ledger_posting_id,
             environment, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, 'COMPLETED', $7, $8, $9, $9)
        ON CONFLICT (idempotency_key) DO NOTHING
        "#,
        transfer_id,
        body.idempotency_key,
        payer_id,
        req.requester_id,
        req.amount_minor,
        req.currency,
        actual_posting,
        // From the process, never the requester. A payment request is initiated by
        // one consumer and paid by another; neither of them gets to say which
        // financial universe the resulting transfer belongs to.
        state.environment.as_str(),
        now,
    )
    .execute(&state.pool)
    .await
    .ok();

    // Mark request PAID
    sqlx::query!(
        r#"
        UPDATE payment_requests
        SET status = 'PAID', transfer_id = $1, paid_at = $2, updated_at = $2
        WHERE id = $3 AND status = 'PENDING'
        "#,
        transfer_id,
        now,
        request_id,
    )
    .execute(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    risk::audit(
        &state.pool,
        "CONSUMER",
        "PAYMENT_REQUEST_PAID",
        &format!("consumer:{payer_id}"),
        serde_json::json!({
            "request_id":    request_id,
            "requester_id":  req.requester_id,
            "amount_minor":  req.amount_minor,
        }),
        None,
    )
    .await;

    Ok(Json(fetch_request(&state.pool, request_id).await?))
}

// ---------------------------------------------------------------------------
// POST /internal/v1/payment-requests/:id/decline
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct DeclineRequestBody {
    pub payer_id: String,
}

pub async fn decline(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(body): Json<DeclineRequestBody>,
) -> ApiResult<Json<PaymentRequestResponse>> {
    let request_id: Uuid = id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid request id"))?;
    let payer_id: Uuid = body
        .payer_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid payer_id"))?;

    let rows = sqlx::query!(
        "UPDATE payment_requests SET status = 'DECLINED', declined_at = NOW(), updated_at = NOW()
         WHERE id = $1 AND payer_id = $2 AND status = 'PENDING'
         RETURNING id",
        request_id,
        payer_id,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    if rows.is_none() {
        return Err(ApiError::not_found(
            "payment request not found or not in PENDING status",
        ));
    }

    Ok(Json(fetch_request(&state.pool, request_id).await?))
}

// ---------------------------------------------------------------------------
// POST /internal/v1/payment-requests/:id/cancel
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct CancelRequestBody {
    pub requester_id: String,
}

pub async fn cancel(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(body): Json<CancelRequestBody>,
) -> ApiResult<Json<PaymentRequestResponse>> {
    let request_id: Uuid = id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid request id"))?;
    let requester_id: Uuid = body
        .requester_id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid requester_id"))?;

    let row = sqlx::query!(
        "UPDATE payment_requests SET status = 'CANCELLED', updated_at = NOW()
         WHERE id = $1 AND requester_id = $2 AND status = 'PENDING'
         RETURNING id",
        request_id,
        requester_id,
    )
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    if row.is_none() {
        return Err(ApiError::not_found(
            "payment request not found or not in PENDING status",
        ));
    }

    Ok(Json(fetch_request(&state.pool, request_id).await?))
}

// ---------------------------------------------------------------------------
// GET /internal/v1/payment-requests/:id
// ---------------------------------------------------------------------------

pub async fn get(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> ApiResult<Json<PaymentRequestResponse>> {
    let id: Uuid = id
        .parse()
        .map_err(|_| ApiError::bad_request("invalid request id"))?;
    Ok(Json(fetch_request(&state.pool, id).await?))
}

// ---------------------------------------------------------------------------
// GET /internal/v1/payment-requests?requester_id=|payer_id=&status=
// ---------------------------------------------------------------------------

#[derive(Deserialize)]
pub struct ListRequestsQuery {
    pub requester_id: Option<String>,
    pub payer_id: Option<String>,
    pub status: Option<String>,
    pub limit: Option<i64>,
}

pub async fn list(
    State(state): State<AppState>,
    Query(q): Query<ListRequestsQuery>,
) -> ApiResult<Json<serde_json::Value>> {
    let limit = q.limit.unwrap_or(20).clamp(1, 100);

    let rows = sqlx::query!(
        r#"
        SELECT pr.id, pr.requester_id, pr.payer_id, pr.amount_minor, pr.currency,
               pr.message, pr.status, pr.transfer_id, pr.expires_at,
               pr.created_at, pr.updated_at, pr.paid_at, pr.declined_at,
               rc.handle AS requester_handle,
               pc.handle AS payer_handle
        FROM payment_requests pr
        LEFT JOIN consumers rc ON rc.id = pr.requester_id
        LEFT JOIN consumers pc ON pc.id = pr.payer_id
        WHERE ($1::uuid IS NULL OR pr.requester_id = $1)
          AND ($2::uuid IS NULL OR pr.payer_id     = $2)
          AND ($3::text  IS NULL OR pr.status      = $3)
        ORDER BY pr.created_at DESC
        LIMIT $4
        "#,
        q.requester_id
            .as_deref()
            .and_then(|s| s.parse::<Uuid>().ok()),
        q.payer_id.as_deref().and_then(|s| s.parse::<Uuid>().ok()),
        q.status,
        limit,
    )
    .fetch_all(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let data: Vec<serde_json::Value> = rows
        .iter()
        .map(|r| {
            serde_json::json!({
                "id":               r.id,
                "requester_id":     r.requester_id,
                "payer_id":         r.payer_id,
                "amount_minor":     r.amount_minor,
                "currency":         r.currency,
                "message":          r.message,
                "status":           r.status,
                "transfer_id":      r.transfer_id,
                "expires_at":       r.expires_at,
                "created_at":       r.created_at,
                "updated_at":       r.updated_at,
                "paid_at":          r.paid_at,
                "declined_at":      r.declined_at,
                "requester_handle": r.requester_handle,
                "payer_handle":     r.payer_handle,
            })
        })
        .collect();

    Ok(Json(serde_json::json!({ "data": data })))
}

// ---------------------------------------------------------------------------
// Shared fetch helper
// ---------------------------------------------------------------------------

async fn fetch_request(pool: &sqlx::PgPool, id: Uuid) -> ApiResult<PaymentRequestResponse> {
    sqlx::query!(
        r#"
        SELECT pr.id, pr.requester_id, pr.payer_id, pr.amount_minor, pr.currency,
               pr.message, pr.status, pr.transfer_id, pr.expires_at,
               pr.created_at, pr.updated_at, pr.paid_at, pr.declined_at,
               rc.handle AS requester_handle,
               pc.handle AS payer_handle
        FROM payment_requests pr
        LEFT JOIN consumers rc ON rc.id = pr.requester_id
        LEFT JOIN consumers pc ON pc.id = pr.payer_id
        WHERE pr.id = $1
        "#,
        id,
    )
    .fetch_optional(pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .map(|r| PaymentRequestResponse {
        id: r.id.to_string(),
        requester_id: r.requester_id.to_string(),
        payer_id: r.payer_id.to_string(),
        amount_minor: r.amount_minor,
        currency: r.currency,
        message: r.message,
        status: r.status,
        transfer_id: r.transfer_id.map(|u| u.to_string()),
        expires_at: r.expires_at,
        created_at: r.created_at,
        updated_at: r.updated_at,
        paid_at: r.paid_at,
        declined_at: r.declined_at,
        requester_handle: Some(r.requester_handle),
        payer_handle: Some(r.payer_handle),
    })
    .ok_or_else(|| ApiError::not_found("payment request not found"))
}
