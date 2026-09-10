use chrono::{DateTime, Utc};
use sqlx::PgPool;
use uuid::Uuid;

use banzami_types::QrCodeId;

use crate::{QrCode, QrCodeStatus, QrCodeType, QrError, QrOwnerType};

#[allow(async_fn_in_trait)]
pub trait QrRepository: Send + Sync {
    async fn create(&self, qr: QrCode) -> Result<QrCode, QrError>;
    async fn get(&self, id: QrCodeId) -> Result<QrCode, QrError>;
    async fn update_status(&self, id: QrCodeId, status: QrCodeStatus) -> Result<QrCode, QrError>;

    /// Atomically claim a dynamic QR code for a one-time payment.
    ///
    /// Transitions `ACTIVE → USED` in a single conditional `UPDATE`, so that
    /// under concurrent scans of the same code exactly one caller wins and the
    /// rest get `AlreadyUsedOrExpired` — closing the double-spend window that a
    /// read-then-write (`get` + `update_status`) leaves open.
    async fn claim_dynamic_for_payment(&self, id: QrCodeId) -> Result<QrCode, QrError>;

    /// Release a previously-claimed dynamic QR code back to `ACTIVE`.
    ///
    /// Used by the payment orchestration to roll back a claim when settlement
    /// fails after the claim succeeded, so the payer can retry. Only a `USED`
    /// row is reopened (conditional `UPDATE`), and only the claim winner ever
    /// calls this for a given code.
    async fn release_dynamic_claim(&self, id: QrCodeId) -> Result<QrCode, QrError>;
}

// ---------------------------------------------------------------------------
// Row type
// ---------------------------------------------------------------------------

#[derive(sqlx::FromRow)]
struct QrRow {
    id: Uuid,
    owner_id: Uuid,
    owner_type: String,
    qr_type: String,
    currency: String,
    amount_minor: Option<i64>,
    status: String,
    expires_at: Option<DateTime<Utc>>,
    used_at: Option<DateTime<Utc>>,
    reference: Option<String>,
    wallet_account_id: Option<Uuid>,
    created_at: DateTime<Utc>,
}

// ---------------------------------------------------------------------------
// PostgreSQL implementation
// ---------------------------------------------------------------------------

pub struct PostgresQrRepository {
    pool: PgPool,
    /// Resolved once from the process, never from a caller.
    environment: banzami_types::Environment,
}

impl PostgresQrRepository {
    pub fn new(pool: PgPool) -> Self {
        Self { pool, environment: banzami_types::Environment::from_env() }
    }

    /// Construct with an explicit environment (tests).
    pub fn with_environment(pool: PgPool, environment: banzami_types::Environment) -> Self {
        Self { pool, environment }
    }
}

const SELECT: &str = "SELECT id, owner_id, owner_type, qr_type, currency, amount_minor, status,
            expires_at, used_at, reference, wallet_account_id, created_at
     FROM qr_codes";

impl QrRepository for PostgresQrRepository {
    async fn create(&self, qr: QrCode) -> Result<QrCode, QrError> {
        sqlx::query(
            "INSERT INTO qr_codes
             (id, owner_id, owner_type, qr_type, currency, amount_minor, status,
              expires_at, used_at, reference, wallet_account_id, environment, created_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)",
        )
        .bind(qr.id.as_uuid())
        .bind(qr.owner_id)
        .bind(qr.owner_type.as_str())
        .bind(qr.qr_type.as_str())
        .bind(qr.currency.code())
        .bind(qr.amount_minor)
        .bind(qr.status.as_str())
        .bind(qr.expires_at)
        .bind(qr.used_at)
        .bind(&qr.reference)
        .bind(qr.wallet_account_id)
        // Explicit, never the column default. That default is 'LIVE', so a writer
        // that omits it does not fail — it silently records Sandbox activity as
        // real money, which is how 272 rows came to claim the wrong universe.
        .bind(self.environment.as_str())
        .bind(qr.created_at)
        .execute(&self.pool)
        .await
        .map_err(QrError::Database)?;

        Ok(qr)
    }

    async fn get(&self, id: QrCodeId) -> Result<QrCode, QrError> {
        let row = sqlx::query_as::<_, QrRow>(&format!("{SELECT} WHERE id = $1"))
            .bind(id.as_uuid())
            .fetch_optional(&self.pool)
            .await
            .map_err(QrError::Database)?
            .ok_or(QrError::NotFound(id))?;

        qr_from_row(row)
    }

    async fn update_status(&self, id: QrCodeId, status: QrCodeStatus) -> Result<QrCode, QrError> {
        let used_at = if status == QrCodeStatus::Used {
            Some(Utc::now())
        } else {
            None
        };

        sqlx::query(
            "UPDATE qr_codes SET status = $1, used_at = COALESCE($2, used_at) WHERE id = $3",
        )
        .bind(status.as_str())
        .bind(used_at)
        .bind(id.as_uuid())
        .execute(&self.pool)
        .await
        .map_err(QrError::Database)?;

        self.get(id).await
    }

    async fn claim_dynamic_for_payment(&self, id: QrCodeId) -> Result<QrCode, QrError> {
        // Single atomic conditional update: only an ACTIVE, unexpired DYNAMIC
        // row transitions to USED. RETURNING tells us whether we won the claim.
        let row = sqlx::query_as::<_, QrRow>(
            "UPDATE qr_codes
                SET status = 'USED', used_at = now()
              WHERE id = $1
                AND qr_type = 'DYNAMIC'
                AND status = 'ACTIVE'
                AND (expires_at IS NULL OR expires_at > now())
            RETURNING id, owner_id, owner_type, qr_type, currency, amount_minor,
                      status, expires_at, used_at, reference, wallet_account_id, created_at",
        )
        .bind(id.as_uuid())
        .fetch_optional(&self.pool)
        .await
        .map_err(QrError::Database)?;

        match row {
            Some(r) => qr_from_row(r),
            None => {
                // We did not win the claim — disambiguate why for a precise error.
                let existing = self.get(id).await?; // NotFound if it truly does not exist
                if existing.qr_type == QrCodeType::Static {
                    Err(QrError::CannotMarkStaticAsUsed)
                } else if existing.status != QrCodeStatus::Active {
                    // Terminal state — already USED or swept to EXPIRED.
                    Err(QrError::AlreadyUsedOrExpired)
                } else if existing.expires_at.is_some_and(|exp| exp <= Utc::now()) {
                    // Still ACTIVE but aged past expiry (the sweep has not run yet).
                    Err(QrError::AlreadyExpired)
                } else {
                    Err(QrError::AlreadyUsedOrExpired)
                }
            }
        }
    }

    async fn release_dynamic_claim(&self, id: QrCodeId) -> Result<QrCode, QrError> {
        sqlx::query(
            "UPDATE qr_codes
                SET status = 'ACTIVE', used_at = NULL
              WHERE id = $1 AND qr_type = 'DYNAMIC' AND status = 'USED'",
        )
        .bind(id.as_uuid())
        .execute(&self.pool)
        .await
        .map_err(QrError::Database)?;

        self.get(id).await
    }
}

// ---------------------------------------------------------------------------
// Row → domain
// ---------------------------------------------------------------------------

fn qr_from_row(row: QrRow) -> Result<QrCode, QrError> {
    let currency = banzami_types::Currency::from_code(&row.currency)
        .ok_or_else(|| QrError::UnknownCurrency(row.currency.clone()))?;
    let owner_type = QrOwnerType::try_from_str(&row.owner_type).ok_or_else(|| {
        QrError::InvalidPayload(format!("unknown owner_type: {}", row.owner_type))
    })?;
    let qr_type = QrCodeType::try_from_str(&row.qr_type)
        .ok_or_else(|| QrError::InvalidPayload(format!("unknown qr_type: {}", row.qr_type)))?;
    let status = QrCodeStatus::try_from_str(&row.status)
        .ok_or_else(|| QrError::InvalidPayload(format!("unknown status: {}", row.status)))?;

    Ok(QrCode {
        id: QrCodeId::from_uuid(row.id),
        owner_id: row.owner_id,
        owner_type,
        qr_type,
        currency,
        amount_minor: row.amount_minor,
        status,
        expires_at: row.expires_at,
        used_at: row.used_at,
        reference: row.reference,
        wallet_account_id: row.wallet_account_id,
        created_at: row.created_at,
    })
}
