use chrono::{DateTime, Utc};
use sqlx::PgPool;

use banzami_types::{ConsumerId, LedgerEntryId, LedgerPostingId, Money, TransferId};

use crate::{
    repository::TransferRepository,
    transfer::{SendTransferRequest, Transfer},
    TransferError,
};

// ---------------------------------------------------------------------------
// Trait
// ---------------------------------------------------------------------------

#[allow(async_fn_in_trait)]
pub trait TransferEngine: Send + Sync {
    /// Atomically validate, post to ledger, and record an instant P2P transfer.
    ///
    /// Idempotent: re-submitting the same `idempotency_key` returns the original transfer.
    async fn send(&self, req: SendTransferRequest) -> Result<Transfer, TransferError>;

    async fn get(&self, id: TransferId) -> Result<Transfer, TransferError>;

    async fn list(
        &self,
        consumer_id: ConsumerId,
        limit: i64,
        before_ts: Option<DateTime<Utc>>,
        before_id: Option<TransferId>,
    ) -> Result<Vec<Transfer>, TransferError>;
}

// ---------------------------------------------------------------------------
// Production implementation
// ---------------------------------------------------------------------------

/// Executes transfers as a single PostgreSQL transaction that atomically:
/// 1. Locks the sender's consumer wallet row (`SELECT FOR UPDATE`).
/// 2. Derives the sender's available balance from `ledger_entries`.
/// 3. Checks sufficiency; returns `InsufficientFunds` if not.
/// 4. Inserts the `ledger_posting` + two `ledger_entries` (DR sender, CR recipient).
/// 5. Inserts the `transfers` record in COMPLETED state.
///
/// All five steps succeed or none do — no partial state is ever persisted.
pub struct PostgresTransferEngine<R: TransferRepository> {
    pool: PgPool,
    repo: R,
    /// Which financial universe rows written here belong to. Resolved once from
    /// the process, never from a caller: a payer must not be able to choose which
    /// universe their transfer is recorded in.
    environment: banzami_types::Environment,
}

impl<R: TransferRepository> PostgresTransferEngine<R> {
    pub fn new(pool: PgPool, repo: R) -> Self {
        Self::with_environment(pool, repo, banzami_types::Environment::from_env())
    }

    /// Construct with an explicit environment. Tests use this; production uses
    /// `new`, which reads the canonical process environment.
    pub fn with_environment(
        pool: PgPool,
        repo: R,
        environment: banzami_types::Environment,
    ) -> Self {
        Self {
            pool,
            repo,
            environment,
        }
    }
}

impl<R: TransferRepository> TransferEngine for PostgresTransferEngine<R> {
    async fn send(&self, req: SendTransferRequest) -> Result<Transfer, TransferError> {
        if req.amount_minor <= 0 {
            return Err(TransferError::InvalidAmount);
        }
        if req.sender_id == req.recipient_id {
            return Err(TransferError::SelfTransfer);
        }
        // The description is published on the receipt and through the public proof,
        // so it is constrained here — at the write boundary that owns the record —
        // rather than in whichever renderer happens to display it.
        crate::description::validate_description(req.description.as_deref())?;

        // Idempotency: return existing transfer for the same key.
        if let Some(existing) = self
            .repo
            .get_by_idempotency_key(&req.idempotency_key)
            .await?
        {
            return Ok(existing);
        }

        let transfer_id = TransferId::new();
        let posting_id = LedgerPostingId::new();
        let now = Utc::now();
        let idem_key = format!("transfer-{transfer_id}");
        let description = format!("P2P transfer {} → {}", req.sender_id, req.recipient_id);

        let mut db_tx = self.pool.begin().await.map_err(TransferError::Database)?;

        // Lock sender's consumer wallet row and fetch available_account_id.
        // This serializes concurrent sends from the same consumer.
        let sender_wallet = sqlx::query_as::<_, (uuid::Uuid, uuid::Uuid, String)>(
            "SELECT id, available_account_id, status
             FROM consumer_wallets
             WHERE consumer_id = $1 AND currency = $2 AND status != 'CLOSED'
             ORDER BY created_at ASC
             LIMIT 1
             FOR UPDATE",
        )
        .bind(req.sender_id.as_uuid())
        .bind(req.currency.code())
        .fetch_optional(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?
        .ok_or_else(|| TransferError::WalletNotFound {
            consumer_id: req.sender_id,
            currency: req.currency,
        })?;

        let (_, sender_available_acct, sender_status) = &sender_wallet;

        if sender_status != "ACTIVE" {
            return Err(TransferError::WalletNotActive(req.sender_id));
        }

        // Fetch recipient's available_account_id (no lock needed — we're only crediting).
        // Try consumer_wallets first (P2P transfers); fall back to merchant wallets
        // (payment link payments where recipient_id is the merchant wallet UUID).
        // `is_merchant_recipient` gates the ADR-042 segregated-account routing below.
        let (recipient_available_acct, is_merchant_recipient): (uuid::Uuid, bool) = {
            let consumer_acct: Option<uuid::Uuid> = sqlx::query_scalar(
                "SELECT available_account_id
                 FROM consumer_wallets
                 WHERE consumer_id = $1 AND currency = $2 AND status = 'ACTIVE'
                 ORDER BY created_at ASC
                 LIMIT 1",
            )
            .bind(req.recipient_id.as_uuid())
            .bind(req.currency.code())
            .fetch_optional(&mut *db_tx)
            .await
            .map_err(TransferError::Database)?;

            if let Some(acct) = consumer_acct {
                (acct, false)
            } else {
                // Recipient is a merchant wallet — look up by wallet UUID directly.
                let acct = sqlx::query_scalar(
                    "SELECT available_account_id
                     FROM wallets
                     WHERE id = $1 AND currency = $2 AND status = 'ACTIVE'",
                )
                .bind(req.recipient_id.as_uuid())
                .bind(req.currency.code())
                .fetch_optional(&mut *db_tx)
                .await
                .map_err(TransferError::Database)?
                .ok_or_else(|| TransferError::WalletNotFound {
                    consumer_id: req.recipient_id,
                    currency: req.currency,
                })?;
                (acct, true)
            }
        };

        // ADR-042: optionally route the CREDIT to a segregated wallet account.
        // The account MUST belong to the recipient merchant wallet, be ACTIVE, and
        // match the currency. Routing is rejected for consumer (P2P) recipients.
        // `None` ⇒ credit the wallet's default available account (unchanged).
        let recipient_credit_acct: uuid::Uuid = match req.recipient_account_id {
            None => recipient_available_acct,
            Some(wa_id) => {
                if !is_merchant_recipient {
                    return Err(TransferError::InvalidWalletAccount);
                }
                sqlx::query_scalar(
                    "SELECT account_id
                     FROM wallet_accounts
                     WHERE id = $1 AND wallet_id = $2 AND currency = $3 AND status = 'ACTIVE'",
                )
                .bind(wa_id)
                .bind(req.recipient_id.as_uuid())
                .bind(req.currency.code())
                .fetch_optional(&mut *db_tx)
                .await
                .map_err(TransferError::Database)?
                .ok_or(TransferError::InvalidWalletAccount)?
            }
        };

        // Derive sender's available balance from ledger entries.
        // LIABILITY account: balance = -(sum of signed_minor_units) = sum(credits) - sum(debits).
        // SUM(BIGINT) returns NUMERIC in PostgreSQL; cast back to BIGINT so sqlx
        // can decode it as i64 without a type mismatch error.
        let raw_balance: i64 = sqlx::query_scalar(
            "SELECT COALESCE(
                 SUM(CASE entry_type
                     WHEN 'DEBIT'  THEN -amount_minor
                     WHEN 'CREDIT' THEN  amount_minor
                     END),
                 0)::BIGINT
             FROM ledger_entries
             WHERE account_id = $1",
        )
        .bind(sender_available_acct)
        .fetch_one(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?;

        if raw_balance < req.amount_minor {
            db_tx.rollback().await.map_err(TransferError::Database)?;
            return Err(TransferError::InsufficientFunds {
                available: Money::new(raw_balance.max(0), req.currency),
                requested: Money::new(req.amount_minor, req.currency),
            });
        }

        // Insert ledger posting.
        sqlx::query(
            "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
             VALUES ($1, $2, $3, $4)",
        )
        .bind(posting_id.as_uuid())
        .bind(&description)
        .bind(&idem_key)
        .bind(now)
        .execute(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?;

        // DR sender:available  (LIABILITY ↓ — we owe the sender less)
        sqlx::query(
            "INSERT INTO ledger_entries
             (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
             VALUES ($1, $2, $3, 'DEBIT', $4, $5, $6)",
        )
        .bind(LedgerEntryId::new().as_uuid())
        .bind(posting_id.as_uuid())
        .bind(sender_available_acct)
        .bind(req.amount_minor)
        .bind(req.currency.code())
        .bind(now)
        .execute(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?;

        // CR recipient:available  (LIABILITY ↑ — we owe the recipient more)
        sqlx::query(
            "INSERT INTO ledger_entries
             (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
             VALUES ($1, $2, $3, 'CREDIT', $4, $5, $6)",
        )
        .bind(LedgerEntryId::new().as_uuid())
        .bind(posting_id.as_uuid())
        .bind(recipient_credit_acct)
        .bind(req.amount_minor)
        .bind(req.currency.code())
        .bind(now)
        .execute(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?;

        // Insert transfer record in COMPLETED state.
        sqlx::query(
            "INSERT INTO transfers
             (id, idempotency_key, sender_id, recipient_id, amount_minor, currency,
              status, description, failure_reason, ledger_posting_id, recipient_handle,
              environment, created_at, updated_at)
             VALUES ($1, $2, $3, $4, $5, $6, 'COMPLETED', $7, NULL, $8, $9, $10, $11, $11)",
        )
        .bind(transfer_id.as_uuid())
        .bind(&req.idempotency_key)
        .bind(req.sender_id.as_uuid())
        .bind(req.recipient_id.as_uuid())
        .bind(req.amount_minor)
        .bind(req.currency.code())
        .bind(&req.description)
        .bind(posting_id.as_uuid())
        .bind(req.recipient_handle.as_deref())
        // Explicit, never the column default. The default is 'LIVE', so omitting
        // this made every Sandbox transfer assert it was real money — 41 rows
        // before it was noticed, and invisible because nothing failed.
        .bind(self.environment.as_str())
        .bind(now)
        .execute(&mut *db_tx)
        .await
        .map_err(TransferError::Database)?;

        db_tx.commit().await.map_err(TransferError::Database)?;

        tracing::info!(
            transfer_id = %transfer_id,
            sender_id   = %req.sender_id,
            recipient_id = %req.recipient_id,
            amount_minor = req.amount_minor,
            currency     = req.currency.code(),
            "P2P transfer completed"
        );

        self.repo.get(transfer_id).await
    }

    async fn get(&self, id: TransferId) -> Result<Transfer, TransferError> {
        self.repo.get(id).await
    }

    async fn list(
        &self,
        consumer_id: ConsumerId,
        limit: i64,
        before_ts: Option<DateTime<Utc>>,
        before_id: Option<TransferId>,
    ) -> Result<Vec<Transfer>, TransferError> {
        self.repo
            .list_for_consumer(consumer_id, limit, before_ts, before_id)
            .await
    }
}
