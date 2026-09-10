use chrono::{DateTime, Utc};
use sqlx::PgPool;
use uuid::Uuid;

use banzami_types::{MerchantId, PaymentLinkId};

use crate::{PaymentLink, PaymentLinkError, PaymentLinkStatus};

#[allow(async_fn_in_trait)]
pub trait PaymentLinkRepository: Send + Sync {
    async fn insert(&self, link: &PaymentLink) -> Result<(), PaymentLinkError>;
    async fn find_by_id(&self, id: PaymentLinkId) -> Result<Option<PaymentLink>, PaymentLinkError>;
    async fn find_by_slug(&self, slug: &str) -> Result<Option<PaymentLink>, PaymentLinkError>;
    async fn list_for_merchant(
        &self,
        merchant_id: MerchantId,
        limit: i64,
        cursor: Option<PaymentLinkId>,
    ) -> Result<Vec<PaymentLink>, PaymentLinkError>;
    async fn update_status(
        &self,
        id: PaymentLinkId,
        status: PaymentLinkStatus,
        paid_at: Option<DateTime<Utc>>,
    ) -> Result<PaymentLink, PaymentLinkError>;
    /// Atomically move an ACTIVE, unexpired link to USED.
    ///
    /// Returns `Some(link)` to the ONE caller whose UPDATE performed the
    /// transition and `None` to every other caller, so a race has a winner
    /// instead of several. The eligibility test lives in the WHERE clause
    /// precisely so it cannot be evaluated against a row that then changes.
    async fn claim_used(
        &self,
        id: PaymentLinkId,
        paid_at: DateTime<Utc>,
    ) -> Result<Option<PaymentLink>, PaymentLinkError>;
    /// Mark all ACTIVE links whose `expires_at` is in the past as EXPIRED.
    async fn expire_overdue(&self) -> Result<u64, PaymentLinkError>;
}

// ---------------------------------------------------------------------------
// Row type
// ---------------------------------------------------------------------------

#[derive(sqlx::FromRow)]
struct LinkRow {
    id: Uuid,
    slug: String,
    merchant_id: Uuid,
    wallet_id: Uuid,
    wallet_account_id: Option<Uuid>,
    amount_minor: Option<i64>,
    currency: String,
    description: Option<String>,
    status: String,
    expires_at: Option<DateTime<Utc>>,
    paid_at: Option<DateTime<Utc>>,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

impl LinkRow {
    fn into_link(self) -> PaymentLink {
        use banzami_types::WalletId;
        PaymentLink {
            id: PaymentLinkId::from_uuid(self.id),
            slug: self.slug,
            merchant_id: MerchantId::from_uuid(self.merchant_id),
            wallet_id: WalletId::from_uuid(self.wallet_id),
            wallet_account_id: self.wallet_account_id,
            amount_minor: self.amount_minor,
            currency: self.currency,
            description: self.description,
            status: PaymentLinkStatus::try_from_str(&self.status)
                .unwrap_or(PaymentLinkStatus::Active),
            expires_at: self.expires_at,
            paid_at: self.paid_at,
            created_at: self.created_at,
            updated_at: self.updated_at,
        }
    }
}

const SELECT: &str = "SELECT id, slug, merchant_id, wallet_id, wallet_account_id, amount_minor, currency, description,
            status, expires_at, paid_at, created_at, updated_at
     FROM payment_links";

// ---------------------------------------------------------------------------
// PostgreSQL implementation
// ---------------------------------------------------------------------------

pub struct PostgresPaymentLinkRepository {
    pool: PgPool,
    /// Resolved once from the process, never from a caller.
    environment: banzami_types::Environment,
}

impl PostgresPaymentLinkRepository {
    pub fn new(pool: PgPool) -> Self {
        Self { pool, environment: banzami_types::Environment::from_env() }
    }

    /// Construct with an explicit environment (tests).
    pub fn with_environment(pool: PgPool, environment: banzami_types::Environment) -> Self {
        Self { pool, environment }
    }
}

impl PaymentLinkRepository for PostgresPaymentLinkRepository {
    async fn insert(&self, link: &PaymentLink) -> Result<(), PaymentLinkError> {
        sqlx::query(
            "INSERT INTO payment_links
             (id, slug, merchant_id, wallet_id, wallet_account_id, amount_minor, currency, description,
              status, expires_at, environment, created_at, updated_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)",
        )
        .bind(link.id.as_uuid())
        .bind(&link.slug)
        .bind(link.merchant_id.as_uuid())
        .bind(link.wallet_id.as_uuid())
        .bind(link.wallet_account_id)
        .bind(link.amount_minor)
        .bind(&link.currency)
        .bind(&link.description)
        .bind(link.status.as_str())
        .bind(link.expires_at)
        // Explicit, never the column default. That default is 'LIVE', so a writer
        // that omits it does not fail — it silently records Sandbox activity as
        // real money, which is how 272 rows came to claim the wrong universe.
        .bind(self.environment.as_str())
        .bind(link.created_at)
        .bind(link.updated_at)
        .execute(&self.pool)
        .await
        .map_err(PaymentLinkError::Database)?;
        Ok(())
    }

    async fn find_by_id(&self, id: PaymentLinkId) -> Result<Option<PaymentLink>, PaymentLinkError> {
        let row = sqlx::query_as::<_, LinkRow>(&format!("{SELECT} WHERE id = $1"))
            .bind(id.as_uuid())
            .fetch_optional(&self.pool)
            .await
            .map_err(PaymentLinkError::Database)?;
        Ok(row.map(LinkRow::into_link))
    }

    async fn find_by_slug(&self, slug: &str) -> Result<Option<PaymentLink>, PaymentLinkError> {
        let row = sqlx::query_as::<_, LinkRow>(&format!("{SELECT} WHERE slug = $1"))
            .bind(slug)
            .fetch_optional(&self.pool)
            .await
            .map_err(PaymentLinkError::Database)?;
        Ok(row.map(LinkRow::into_link))
    }

    async fn list_for_merchant(
        &self,
        merchant_id: MerchantId,
        limit: i64,
        cursor: Option<PaymentLinkId>,
    ) -> Result<Vec<PaymentLink>, PaymentLinkError> {
        let rows = if let Some(c) = cursor {
            sqlx::query_as::<_, LinkRow>(&format!(
                "{SELECT} WHERE merchant_id = $1 AND created_at < \
                 (SELECT created_at FROM payment_links WHERE id = $2) \
                 ORDER BY created_at DESC LIMIT $3"
            ))
            .bind(merchant_id.as_uuid())
            .bind(c.as_uuid())
            .bind(limit)
            .fetch_all(&self.pool)
            .await
        } else {
            sqlx::query_as::<_, LinkRow>(&format!(
                "{SELECT} WHERE merchant_id = $1 ORDER BY created_at DESC LIMIT $2"
            ))
            .bind(merchant_id.as_uuid())
            .bind(limit)
            .fetch_all(&self.pool)
            .await
        }
        .map_err(PaymentLinkError::Database)?;

        Ok(rows.into_iter().map(LinkRow::into_link).collect())
    }

    async fn update_status(
        &self,
        id: PaymentLinkId,
        status: PaymentLinkStatus,
        paid_at: Option<DateTime<Utc>>,
    ) -> Result<PaymentLink, PaymentLinkError> {
        let row = sqlx::query_as::<_, LinkRow>(
            "UPDATE payment_links
             SET status = $2, paid_at = $3, updated_at = NOW()
             WHERE id = $1
             RETURNING id, slug, merchant_id, wallet_id, wallet_account_id, amount_minor, currency,
                       description, status, expires_at, paid_at, created_at, updated_at",
        )
        .bind(id.as_uuid())
        .bind(status.as_str())
        .bind(paid_at)
        .fetch_one(&self.pool)
        .await
        .map_err(PaymentLinkError::Database)?;
        Ok(row.into_link())
    }

    async fn claim_used(
        &self,
        id: PaymentLinkId,
        paid_at: DateTime<Utc>,
    ) -> Result<Option<PaymentLink>, PaymentLinkError> {
        // ACTIVE and unexpired are conditions of the UPDATE itself. Checking them
        // in Rust first and updating second is the read-then-write race this
        // replaces: every concurrent caller read ACTIVE, every one passed, and
        // every one wrote.
        let row = sqlx::query_as::<_, LinkRow>(
            "UPDATE payment_links
                SET status = 'USED', paid_at = $2, updated_at = NOW()
              WHERE id = $1
                AND status = 'ACTIVE'
                AND (expires_at IS NULL OR expires_at > NOW())
             RETURNING id, slug, merchant_id, wallet_id, wallet_account_id, amount_minor, currency,
                       description, status, expires_at, paid_at, created_at, updated_at",
        )
        .bind(id.as_uuid())
        .bind(paid_at)
        .fetch_optional(&self.pool)
        .await
        .map_err(PaymentLinkError::Database)?;
        Ok(row.map(|r| r.into_link()))
    }

    async fn expire_overdue(&self) -> Result<u64, PaymentLinkError> {
        let result = sqlx::query(
            "UPDATE payment_links SET status = 'EXPIRED', updated_at = NOW()
             WHERE status = 'ACTIVE' AND expires_at IS NOT NULL AND expires_at <= NOW()",
        )
        .execute(&self.pool)
        .await
        .map_err(PaymentLinkError::Database)?;
        Ok(result.rows_affected())
    }
}
