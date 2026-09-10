use chrono::Utc;
use sqlx::PgPool;
use uuid::Uuid;

use banzami_types::{LedgerPostingId, MerchantId, Money, PayoutId, WalletId};

use crate::{BankDestination, Payout, PayoutError, PayoutStatus};

// ---------------------------------------------------------------------------
// Trait
// ---------------------------------------------------------------------------

#[allow(async_fn_in_trait)]
pub trait PayoutRepository: Send + Sync {
    async fn create(&self, payout: &Payout) -> Result<(), PayoutError>;
    /// The pricing profile the operator assigned to this merchant.
    ///
    /// Resolved inside Core, from the merchant's own record. There is no payout
    /// request field for it and there must not be: a caller that could name its
    /// profile could name its tariff.
    async fn pricing_profile_for_merchant(
        &self,
        merchant_id: banzami_types::MerchantId,
    ) -> Result<Option<String>, PayoutError>;
    /// Persist the pricing decision for a payout, at the moment it is decided.
    ///
    /// `payouts` used to record `amount_minor` and nothing else, so explaining
    /// why a withdrawal cost what it did meant joining to `ledger_postings` on
    /// a derived idempotency key. That is how the RA-063 incident was eventually
    /// reconstructed, and it is not a reasonable way to answer "which rule
    /// priced this".
    async fn record_pricing(
        &self,
        id: PayoutId,
        pricing: &crate::WithdrawalPricing,
        net_minor: i64,
    ) -> Result<(), PayoutError>;
    async fn get(&self, id: PayoutId) -> Result<Payout, PayoutError>;
    async fn get_by_idempotency_key(&self, key: &str) -> Result<Option<Payout>, PayoutError>;
    async fn list_for_merchant(
        &self,
        merchant_id: MerchantId,
        limit: i64,
    ) -> Result<Vec<Payout>, PayoutError>;
    async fn list_all(&self, limit: i64, status: Option<&str>) -> Result<Vec<Payout>, PayoutError>;
    async fn update_status(
        &self,
        id: PayoutId,
        status: PayoutStatus,
        ledger_posting_id: Option<LedgerPostingId>,
        failure_reason: Option<String>,
    ) -> Result<Payout, PayoutError>;
}

// ---------------------------------------------------------------------------
// Row projection
// ---------------------------------------------------------------------------

#[derive(sqlx::FromRow)]
#[allow(dead_code)]
struct PayoutRow {
    id: Uuid,
    merchant_id: Uuid,
    wallet_id: Uuid,
    idempotency_key: String,
    status: String,
    environment: String,
    amount_minor: i64,
    currency: String,
    bank_account_number: String,
    bank_code: String,
    account_holder_name: String,
    ledger_posting_id: Option<Uuid>,
    failure_reason: Option<String>,
    created_at: chrono::DateTime<Utc>,
    sent_at: Option<chrono::DateTime<Utc>>,
    confirmed_at: Option<chrono::DateTime<Utc>>,
    returned_at: Option<chrono::DateTime<Utc>>,
    failed_at: Option<chrono::DateTime<Utc>>,
}

fn row_to_payout(row: PayoutRow) -> Result<Payout, PayoutError> {
    let currency = banzami_types::Currency::from_code(&row.currency)
        .ok_or_else(|| PayoutError::UnknownStatus(row.currency.clone()))?;
    let status = PayoutStatus::try_from_str(&row.status)
        .ok_or_else(|| PayoutError::UnknownStatus(row.status.clone()))?;
    Ok(Payout {
        id: PayoutId::from_uuid(row.id),
        merchant_id: MerchantId::from_uuid(row.merchant_id),
        wallet_id: WalletId::from_uuid(row.wallet_id),
        idempotency_key: row.idempotency_key,
        status,
        amount: Money::new(row.amount_minor, currency),
        destination: BankDestination {
            account_number: row.bank_account_number,
            bank_code: row.bank_code,
            account_holder_name: row.account_holder_name,
        },
        ledger_posting_id: row.ledger_posting_id.map(LedgerPostingId::from_uuid),
        failure_reason: row.failure_reason,
        created_at: row.created_at,
        sent_at: row.sent_at,
        confirmed_at: row.confirmed_at,
        returned_at: row.returned_at,
        failed_at: row.failed_at,
    })
}

// ---------------------------------------------------------------------------
// PostgreSQL implementation
// ---------------------------------------------------------------------------

pub struct PostgresPayoutRepository {
    pool: PgPool,
    /// Resolved once from the process, never from a caller.
    environment: banzami_types::Environment,
}

impl PostgresPayoutRepository {
    pub fn new(pool: PgPool) -> Self {
        Self { pool, environment: banzami_types::Environment::from_env() }
    }

    /// Construct with an explicit environment (tests).
    pub fn with_environment(pool: PgPool, environment: banzami_types::Environment) -> Self {
        Self { pool, environment }
    }
}

impl PayoutRepository for PostgresPayoutRepository {
    async fn create(&self, p: &Payout) -> Result<(), PayoutError> {
        sqlx::query!(
            r#"
            INSERT INTO payouts (
                id, merchant_id, wallet_id, idempotency_key, status,
                amount_minor, currency,
                bank_account_number, bank_code, account_holder_name,
                ledger_posting_id, failure_reason, environment, created_at
            ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
            "#,
            p.id.as_uuid(),
            p.merchant_id.as_uuid(),
            p.wallet_id.as_uuid(),
            p.idempotency_key,
            p.status.as_str(),
            p.amount.amount_minor(),
            p.amount.currency.code(),
            p.destination.account_number,
            p.destination.bank_code,
            p.destination.account_holder_name,
            p.ledger_posting_id.map(|id| id.as_uuid()),
            p.failure_reason.as_deref(),
            // Explicit, never the column default. That default is 'LIVE', so a writer
            // that omits it does not fail — it silently records Sandbox activity as
            // real money, which is how 272 rows came to claim the wrong universe.
            self.environment.as_str(),
            p.created_at,
        )
        .execute(&self.pool)
        .await
        .map_err(|e| {
            if let Some(db) = e.as_database_error() {
                if db.constraint() == Some("payouts_idempotency_key_key") {
                    return PayoutError::DuplicateIdempotencyKey(p.idempotency_key.clone());
                }
            }
            PayoutError::Database(e)
        })?;
        Ok(())
    }

    async fn get(&self, id: PayoutId) -> Result<Payout, PayoutError> {
        let row = sqlx::query_as!(
            PayoutRow,
            "SELECT id, merchant_id, wallet_id, idempotency_key, status, environment,
                    amount_minor, currency, bank_account_number, bank_code,
                    account_holder_name, ledger_posting_id, failure_reason,
                    created_at, sent_at, confirmed_at, returned_at, failed_at FROM payouts WHERE id = $1",
            id.as_uuid()
        )
        .fetch_optional(&self.pool)
        .await?
        .ok_or(PayoutError::NotFound(id))?;
        row_to_payout(row)
    }

    async fn get_by_idempotency_key(&self, key: &str) -> Result<Option<Payout>, PayoutError> {
        let row = sqlx::query_as!(
            PayoutRow,
            "SELECT id, merchant_id, wallet_id, idempotency_key, status, environment,
                    amount_minor, currency, bank_account_number, bank_code,
                    account_holder_name, ledger_posting_id, failure_reason,
                    created_at, sent_at, confirmed_at, returned_at, failed_at FROM payouts WHERE idempotency_key = $1",
            key
        )
        .fetch_optional(&self.pool)
        .await?;
        row.map(row_to_payout).transpose()
    }

    async fn list_for_merchant(
        &self,
        merchant_id: MerchantId,
        limit: i64,
    ) -> Result<Vec<Payout>, PayoutError> {
        let rows = sqlx::query_as!(
            PayoutRow,
            "SELECT id, merchant_id, wallet_id, idempotency_key, status, environment,
                    amount_minor, currency, bank_account_number, bank_code,
                    account_holder_name, ledger_posting_id, failure_reason,
                    created_at, sent_at, confirmed_at, returned_at, failed_at FROM payouts WHERE merchant_id = $1 ORDER BY created_at DESC LIMIT $2",
            merchant_id.as_uuid(),
            limit
        )
        .fetch_all(&self.pool)
        .await?;
        rows.into_iter().map(row_to_payout).collect()
    }

    async fn list_all(&self, limit: i64, status: Option<&str>) -> Result<Vec<Payout>, PayoutError> {
        let rows = if let Some(s) = status {
            sqlx::query_as!(
                PayoutRow,
                "SELECT id, merchant_id, wallet_id, idempotency_key, status, environment,
                    amount_minor, currency, bank_account_number, bank_code,
                    account_holder_name, ledger_posting_id, failure_reason,
                    created_at, sent_at, confirmed_at, returned_at, failed_at FROM payouts WHERE status = $1 ORDER BY created_at DESC LIMIT $2",
                s,
                limit
            )
            .fetch_all(&self.pool)
            .await?
        } else {
            sqlx::query_as!(
                PayoutRow,
                "SELECT id, merchant_id, wallet_id, idempotency_key, status, environment,
                    amount_minor, currency, bank_account_number, bank_code,
                    account_holder_name, ledger_posting_id, failure_reason,
                    created_at, sent_at, confirmed_at, returned_at, failed_at FROM payouts ORDER BY created_at DESC LIMIT $1",
                limit
            )
            .fetch_all(&self.pool)
            .await?
        };
        rows.into_iter().map(row_to_payout).collect()
    }

    async fn pricing_profile_for_merchant(
        &self,
        merchant_id: banzami_types::MerchantId,
    ) -> Result<Option<String>, PayoutError> {
        let code: Option<String> = sqlx::query_scalar(
            "SELECT p.code
               FROM merchants m
               JOIN pricing_profiles p ON p.id = m.pricing_profile_id
              WHERE m.id = $1 AND p.enabled",
        )
        .bind(merchant_id.as_uuid())
        .fetch_optional(&self.pool)
        .await?
        .flatten();
        Ok(code)
    }

    async fn record_pricing(
        &self,
        id: PayoutId,
        pricing: &crate::WithdrawalPricing,
        net_minor: i64,
    ) -> Result<(), PayoutError> {
        // Written once, at the decision point. A later rule change must never
        // reach back and alter what a completed payout was charged.
        sqlx::query(
            "UPDATE payouts SET
                 fee_minor            = $2,
                 net_minor            = $3,
                 pricing_rule_id      = $4,
                 pricing_rule_version = $5,
                 pricing_rate_bps     = $6,
                 pricing_decided_at   = $7
               WHERE id = $1 AND pricing_decided_at IS NULL",
        )
        .bind(id.as_uuid())
        .bind(pricing.fee_minor)
        .bind(net_minor)
        .bind(pricing.rule_id.map(|r| r.as_uuid()))
        .bind(pricing.rule_version)
        .bind(pricing.rate_bps.map(|b| b as i32))
        .bind(pricing.decided_at)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn update_status(
        &self,
        id: PayoutId,
        status: PayoutStatus,
        ledger_posting_id: Option<LedgerPostingId>,
        failure_reason: Option<String>,
    ) -> Result<Payout, PayoutError> {
        let now = Utc::now();
        let row = sqlx::query_as!(
            PayoutRow,
            r#"
            UPDATE payouts SET
                status              = $2,
                ledger_posting_id   = COALESCE($3, ledger_posting_id),
                failure_reason      = COALESCE($4, failure_reason),
                sent_at             = CASE WHEN $2 = 'SENT'      THEN $5 ELSE sent_at      END,
                confirmed_at        = CASE WHEN $2 = 'CONFIRMED' THEN $5 ELSE confirmed_at  END,
                returned_at         = CASE WHEN $2 = 'RETURNED'  THEN $5 ELSE returned_at   END,
                failed_at           = CASE WHEN $2 = 'FAILED'    THEN $5 ELSE failed_at     END
            WHERE id = $1
            -- Named, not `RETURNING *`.
            --
            -- The wildcard compiled fine until payouts gained pricing snapshot
            -- columns, at which point query_as! found six fields PayoutRow does
            -- not have and the crate stopped building. A star in a typed query
            -- makes every future column an incompatible change to a struct that
            -- has nothing to do with it.
            RETURNING id, merchant_id, wallet_id, idempotency_key, status, environment,
                      amount_minor, currency, bank_account_number, bank_code,
                      account_holder_name, ledger_posting_id, failure_reason,
                      created_at, sent_at, confirmed_at, returned_at, failed_at
            "#,
            id.as_uuid(),
            status.as_str(),
            ledger_posting_id.map(|id| id.as_uuid()),
            failure_reason,
            now,
        )
        .fetch_optional(&self.pool)
        .await?
        .ok_or(PayoutError::NotFound(id))?;
        row_to_payout(row)
    }
}
