//! Sandbox Business readiness — the part of a self-service Business that
//! Financial Setup was not creating.
//!
//! The Developer Platform's public Financial Setup provisioned a merchant, a
//! wallet and its PRIMARY account, and stopped there. That is enough to RECEIVE
//! money and not enough to move it: application settlement names its parties by
//! @banza, and a Business with no entry in `handle_registry` cannot be named at
//! all — not as a beneficiary, and not as its own application-fee destination.
//! Zero of the five Sandbox owners the platform had provisioned had a handle, so
//! no ordinary external Developer Project could complete a settlement.
//!
//! The same gap closes the fee-bearing path: ADR-028 requires an application-fee
//! destination to be KYB-approved AND to be an APPLICATION/PLATFORM account, and
//! nothing on the public Sandbox lifecycle could ever produce either. KYB was
//! closed first; the TYPE was not, so all nine self-service Sandbox Businesses
//! were left as `MERCHANT` — the create-time default — and every one of them
//! failed its own fee-destination check with FEE_DESTINATION_TYPE_NOT_ALLOWED.
//! A Developer Project Business is an application routing value on behalf of an
//! application. That is what APPLICATION means in ADR-028, so it is declared
//! here rather than left to a default that describes something else.
//!
//! WHY HERE AND NOT IN THE API LAYER
//!
//! `handle_registry` is the routing table a payment resolves a named party
//! through, and compliance state gates settlement. Both are Core's, and an API
//! service writing them directly would be the Go layer minting financial
//! identity. Core exposes one internal, Sandbox-only operation instead.
//!
//! WHY THE HANDLE IS DERIVED AND NOT CHOSEN
//!
//! The caller supplies a handle derived from the project, exactly as it already
//! derives the merchant's name and address. A caller-chosen handle is a
//! caller-chosen identity, and handles are a scarce public namespace: letting a
//! developer name their Sandbox Business `@banco` would reserve it.
//!
//! LIVE IS REFUSED HERE, NOT ONLY BY THE CALLER
//!
//! Approving KYB is a compliance decision. The Developer Platform already
//! refuses to provision outside Sandbox, but a guard that lives only in the
//! caller is one deployment mistake from being absent. This route refuses in
//! LIVE on its own reading of the environment.

use axum::{extract::State, http::StatusCode, Json};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::{
    error::{ApiError, ApiResult},
    state::AppState,
};

#[derive(Deserialize)]
pub struct ReadinessBody {
    pub merchant_id: String,
    /// Derived from the project by the caller; never developer-supplied.
    pub handle: String,
}

#[derive(Serialize)]
pub struct ReadinessResponse {
    pub merchant_id: String,
    pub handle: String,
    pub kyb_status: String,
    /// The ADR-028 taxonomy this Business ended up with. Reported rather than
    /// assumed: the two fee-destination conditions are KYB and type, and a
    /// caller that can only see one of them cannot tell which one refused.
    pub business_account_type: String,
    /// True when this call created what was missing rather than finding it.
    pub provisioned: bool,
}

/// POST /internal/v1/sandbox/business-readiness
///
/// Idempotent and resumable: a Business that already has a handle keeps it, and
/// an existing compliance decision is never overwritten — an operator's REJECTED
/// or SUSPENDED must not be undone by a provisioning retry.
pub async fn business_readiness(
    State(state): State<AppState>,
    Json(body): Json<ReadinessBody>,
) -> ApiResult<(StatusCode, Json<ReadinessResponse>)> {
    if state.environment.is_live() {
        tracing::error!("sandbox::business_readiness called in LIVE environment — rejected");
        return Err(ApiError::forbidden(
            "sandbox business readiness is not available in LIVE environment",
        ));
    }

    let merchant_id = Uuid::parse_str(body.merchant_id.trim())
        .map_err(|_| ApiError::bad_request("invalid merchant_id"))?;

    // The merchant must already exist. This completes a Business; it never
    // conjures one, so a wrong id is a not-found rather than a new account.
    let exists: Option<(String,)> =
        sqlx::query_as("SELECT status FROM merchants WHERE id = $1")
            .bind(merchant_id)
            .fetch_optional(&state.pool)
            .await
            .map_err(|e| ApiError::internal(e.to_string()))?;
    let Some((status,)) = exists else {
        return Err(ApiError::not_found("merchant not found"));
    };
    if status != "ACTIVE" {
        return Err(ApiError::unprocessable(
            "MERCHANT_NOT_ACTIVE",
            "business account is not active",
        ));
    }

    let desired = banzami_identity::normalize_handle(&body.handle);
    banzami_identity::validate_handle(&desired)
        .map_err(|e| ApiError::bad_request(format!("invalid handle: {e}")))?;

    let mut provisioned = false;

    // A handle this merchant already holds wins over the derived one: an owner's
    // identity is not reassigned by a retry.
    let existing: Option<(String,)> =
        sqlx::query_as("SELECT handle FROM handle_registry WHERE owner_id = $1 AND owner_type = 'MERCHANT'")
            .bind(merchant_id)
            .fetch_optional(&state.pool)
            .await
            .map_err(|e| ApiError::internal(e.to_string()))?;

    let handle = match existing {
        Some((h,)) => h,
        None => {
            // ON CONFLICT DO NOTHING rather than an existence check: two
            // concurrent provisioning attempts must not both believe they won,
            // and a handle already taken by ANOTHER owner must not be stolen.
            let inserted: Option<(String,)> = sqlx::query_as(
                "INSERT INTO handle_registry (handle, owner_type, owner_id, created_at)
                 VALUES ($1, 'MERCHANT', $2, now())
                 ON CONFLICT (handle) DO NOTHING
                 RETURNING handle",
            )
            .bind(&desired)
            .bind(merchant_id)
            .fetch_optional(&state.pool)
            .await
            .map_err(|e| ApiError::internal(e.to_string()))?;
            match inserted {
                Some((h,)) => {
                    provisioned = true;
                    h
                }
                None => {
                    return Err(ApiError::conflict(
                        "HANDLE_TAKEN",
                        "the derived handle is already registered to another owner",
                    ))
                }
            }
        }
    };

    // The ADR-028 taxonomy. Promoted only FROM the create-time default: an
    // operator who has deliberately set this account to something else has made
    // a decision, and a provisioning retry does not get to overrule it. Coming
    // from 'MERCHANT' means nobody chose — the merchant was created by
    // /internal/v1/merchants with no type at all.
    let promoted: Option<(String,)> = sqlx::query_as(
        "UPDATE merchants
            SET business_account_type = 'APPLICATION', updated_at = now()
          WHERE id = $1 AND COALESCE(business_account_type, 'MERCHANT') = 'MERCHANT'
          RETURNING business_account_type",
    )
    .bind(merchant_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;
    if promoted.is_some() {
        provisioned = true;
    }

    // Sandbox KYB. DO NOTHING, never an upsert: a real decision recorded against
    // this merchant — including a REJECTED or SUSPENDED one — outranks
    // provisioning, and a retry must not launder it into APPROVED.
    let kyb_inserted: Option<(String,)> = sqlx::query_as(
        "INSERT INTO merchant_compliance
             (merchant_id, kyb_status, aml_status, reviewed_at, notes, created_at, updated_at)
         VALUES ($1, 'APPROVED', 'APPROVED', now(),
                 'Sandbox test readiness provisioned by Developer Financial Setup', now(), now())
         ON CONFLICT (merchant_id) DO NOTHING
         RETURNING kyb_status",
    )
    .bind(merchant_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?;

    let kyb_status = match kyb_inserted {
        Some((s,)) => {
            provisioned = true;
            s
        }
        None => sqlx::query_scalar::<_, String>(
            "SELECT kyb_status FROM merchant_compliance WHERE merchant_id = $1",
        )
        .bind(merchant_id)
        .fetch_one(&state.pool)
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?,
    };

    let account_type = sqlx::query_scalar::<_, Option<String>>(
        "SELECT business_account_type FROM merchants WHERE id = $1",
    )
    .bind(merchant_id)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .unwrap_or_else(|| "MERCHANT".into());

    super::risk::audit(
        &state.pool,
        "SYSTEM",
        "KYC_STATUS_CHANGED",
        &format!("merchant:{merchant_id}"),
        serde_json::json!({
            "kind": "KYB",
            "decision": kyb_status,
            "source": "sandbox_business_readiness",
            "handle": handle,
            "business_account_type": account_type,
            "environment": state.environment.as_str(),
        }),
        None,
    )
    .await;

    tracing::info!(
        merchant_id = %merchant_id, handle = %handle, provisioned,
        "sandbox: business readiness resolved"
    );

    Ok((
        StatusCode::OK,
        Json(ReadinessResponse {
            merchant_id: merchant_id.to_string(),
            handle,
            kyb_status,
            business_account_type: account_type,
            provisioned,
        }),
    ))
}
