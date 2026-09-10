//! A transfer records the universe it happened in — and the payer does not choose it.
//!
//! `transfers.environment` carries `DEFAULT 'LIVE'`. The engine used to omit the
//! column, so every Sandbox transfer was written as real money: invisible to the
//! Sandbox proof lookup that should have found it, and counted by anything that
//! filters on LIVE. The syntactic guard in tests/ops proves the column is named;
//! this proves the value that arrives in the row.
//!
//! INV-ENV-1  a Sandbox engine writes SANDBOX, never the column default
//! INV-ENV-2  a Live engine writes LIVE
//! INV-ENV-3  nothing a caller can put in a request changes it

use std::sync::Arc;

use sqlx::PgPool;

use banzami_consumer_wallets::{
    CompleteOnboardingRequest, ConsumerWalletEngine, PostgresConsumerWalletEngine,
    PostgresConsumerWalletRepository, PostgresOnboardingRepository, StartOnboardingRequest,
    VerifyOtpRequest,
};
use banzami_ledger::PostgresLedgerRepository;
use banzami_transfers::{
    PostgresTransferEngine, PostgresTransferRepository, SendTransferRequest, TransferEngine,
};
use banzami_types::{Currency, Environment};

fn cw_engine(pool: PgPool) -> impl ConsumerWalletEngine {
    let ledger = Arc::new(PostgresLedgerRepository::new(pool.clone()));
    PostgresConsumerWalletEngine::with_pool(
        pool.clone(),
        ledger,
        PostgresOnboardingRepository::new(pool.clone()),
        PostgresConsumerWalletRepository::new(pool),
    )
}

fn engine_in(pool: PgPool, environment: Environment) -> impl TransferEngine {
    let repo = PostgresTransferRepository::new(pool.clone());
    PostgresTransferEngine::with_environment(pool, repo, environment)
}

async fn activate(eng: &impl ConsumerWalletEngine, phone: &str, handle: &str) {
    let session = eng
        .start_onboarding(StartOnboardingRequest {
            phone_number: phone.into(),
            currency: Currency::AOA,
            otp_plaintext_for_test: Some("123456".into()),
        })
        .await
        .unwrap();
    eng.verify_otp(VerifyOtpRequest {
        session_id: session.id,
        otp_code: "123456".into(),
    })
    .await
    .unwrap();
    eng.complete_onboarding(CompleteOnboardingRequest {
        session_id: session.id,
        banza_handle: handle.into(),
        pin: "0000".into(),
    })
    .await
    .unwrap();
}

async fn consumer_id(pool: &PgPool, handle: &str) -> uuid::Uuid {
    sqlx::query_scalar("SELECT id FROM consumers WHERE handle = $1")
        .bind(handle)
        .fetch_one(pool)
        .await
        .unwrap()
}

async fn seed(pool: &PgPool, handle: &str, minor: i64) {
    let account_id: uuid::Uuid = sqlx::query_scalar(
        "SELECT w.available_account_id FROM consumer_wallets w
         JOIN consumers c ON c.id = w.consumer_id
         WHERE c.handle = $1 AND w.status = 'ACTIVE' LIMIT 1",
    )
    .bind(handle)
    .fetch_one(pool)
    .await
    .unwrap();
    let posting = uuid::Uuid::new_v4();
    sqlx::query(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, 'env test seed', $2, NOW())",
    )
    .bind(posting)
    .bind(format!("env-seed-{handle}-{minor}"))
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO ledger_entries (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'CREDIT', $4, 'AOA', NOW())",
    )
    .bind(uuid::Uuid::new_v4())
    .bind(posting)
    .bind(account_id)
    .bind(minor)
    .execute(pool)
    .await
    .unwrap();
}

/// Runs one transfer through an engine configured for `environment` and returns
/// the environment string that actually landed in the row.
async fn recorded_environment(
    pool: &PgPool,
    environment: Environment,
    description: Option<String>,
    suffix: &str,
) -> String {
    let cw = cw_engine(pool.clone());
    let sender_handle = format!("env_s_{suffix}");
    let recipient_handle = format!("env_r_{suffix}");
    activate(&cw, &format!("+2449{suffix}0001"), &sender_handle).await;
    activate(&cw, &format!("+2449{suffix}0002"), &recipient_handle).await;
    seed(pool, &sender_handle, 100_000).await;

    let transfer = engine_in(pool.clone(), environment)
        .send(SendTransferRequest {
            idempotency_key: format!("env-{suffix}"),
            sender_id: banzami_types::ConsumerId::from_uuid(consumer_id(pool, &sender_handle).await),
            recipient_id: banzami_types::ConsumerId::from_uuid(
                consumer_id(pool, &recipient_handle).await,
            ),
            amount_minor: 1_000,
            currency: Currency::AOA,
            description,
            recipient_handle: Some(recipient_handle.clone()),
            recipient_account_id: None,
        })
        .await
        .unwrap();

    sqlx::query_scalar("SELECT environment FROM transfers WHERE id = $1")
        .bind(transfer.id.as_uuid())
        .fetch_one(pool)
        .await
        .unwrap()
}

// INV-ENV-1
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_sandbox_engine_does_not_inherit_the_live_column_default(pool: PgPool) {
    assert_eq!(
        recorded_environment(&pool, Environment::Sandbox, None, "sbx").await,
        "SANDBOX",
        "a Sandbox transfer was recorded as real money",
    );
}

// INV-ENV-2
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_live_engine_records_live(pool: PgPool) {
    assert_eq!(
        recorded_environment(&pool, Environment::Live, None, "liv").await,
        "LIVE",
    );
}

// INV-ENV-3. The description is the only free-text field a payer controls and it
// reaches the same INSERT, so it is the realistic vehicle for an attempt to
// influence a neighbouring column. It must be inert.
#[sqlx::test(migrations = "../../db/migrations")]
async fn payer_supplied_text_cannot_influence_the_environment(pool: PgPool) {
    for (i, attempt) in [
        "LIVE",
        "', environment='LIVE",
        "environment=LIVE",
        "SANDBOX'--",
    ]
    .iter()
    .enumerate()
    {
        let got = recorded_environment(
            &pool,
            Environment::Sandbox,
            Some((*attempt).to_string()),
            &format!("inj{i}"),
        )
        .await;
        assert_eq!(
            got, "SANDBOX",
            "payer text {attempt:?} changed the recorded environment to {got}",
        );
    }
}
