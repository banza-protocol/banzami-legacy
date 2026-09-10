//! Integration tests for refund (REF-001) and dispute (REF-002) financial
//! invariants. Each test runs against a fresh PostgreSQL database created by
//! `#[sqlx::test]` with all migrations applied — financial invariants are
//! asserted against real ledger postings, never mocks (CLAUDE.md §7).
//!
//! Scope is strictly internal: ledger / refund / dispute logic. No EMIS, bank,
//! or external withdrawal provider is assumed or exercised.

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    Json,
};
use sqlx::PgPool;
use uuid::Uuid;

use crate::routes::{disputes, refunds};
use crate::state::{AppState, CoreEnvironment};
use banzami_types::AccountId;

// ───────────────────────── seed helpers ─────────────────────────

async fn ledger_account(pool: &PgPool, account_type: &str, name: &str) -> Uuid {
    sqlx::query_scalar::<_, Uuid>(
        "INSERT INTO ledger_accounts (id, account_type, name, currency)
         VALUES ($1, $2, $3, 'AOA') RETURNING id",
    )
    .bind(Uuid::new_v4())
    .bind(account_type)
    .bind(name)
    .fetch_one(pool)
    .await
    .expect("create ledger account")
}

/// Builds a real `AppState` over the test pool with freshly-provisioned system
/// transit/bank ledger accounts (the simulated acquiring provider is used —
/// `ACQUIRING_PROVIDER` is unset in tests).
async fn build_state(pool: PgPool) -> AppState {
    let transit = ledger_account(&pool, "ASSET", "System — Transit").await;
    let bank = ledger_account(&pool, "ASSET", "System — Bank").await;
    let operator_fee = ledger_account(&pool, "REVENUE", "Operator — Fee Revenue").await;
    AppState::new(
        pool,
        AccountId::from_uuid(transit),
        AccountId::from_uuid(bank),
        AccountId::from_uuid(operator_fee),
        CoreEnvironment::Sandbox,
    )
}

struct Seed {
    merchant_id: Uuid,
    transaction_id: Uuid,
    merchant_account: Uuid,
}

/// Seeds a merchant wallet (backed by ledger accounts) and one CAPTURED
/// transaction of `amount` minor units, ready to be refunded or disputed.
async fn seed_captured_tx(pool: &PgPool, amount: i64) -> Seed {
    let merchant_id = Uuid::new_v4();
    let available = ledger_account(pool, "LIABILITY", "merchant-available").await;
    let reserved = ledger_account(pool, "LIABILITY", "merchant-reserved").await;
    let wallet_id = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO wallets (id, merchant_id, currency, status, available_account_id, reserved_account_id)
         VALUES ($1, $2, 'AOA', 'ACTIVE', $3, $4)",
    )
    .bind(wallet_id)
    .bind(merchant_id)
    .bind(available)
    .bind(reserved)
    .execute(pool)
    .await
    .expect("seed wallet");

    let transaction_id = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO transactions
            (id, idempotency_key, transaction_type, status, amount_minor, fee_minor, currency, merchant_id, wallet_id, environment)
         VALUES ($1, $2, 'PAYMENT', 'CAPTURED', $3, 0, 'AOA', $4, $5, 'SANDBOX')",
    )
    .bind(transaction_id)
    .bind(format!("seed-{transaction_id}"))
    .bind(amount)
    .bind(merchant_id)
    .bind(wallet_id)
    .execute(pool)
    .await
    .expect("seed transaction");

    // A CAPTURED payment means the merchant HOLDS the money. The seed used to
    // write the transaction row and no ledger entries, so every refund test ran
    // against a merchant with a zero balance. That was invisible while a refund
    // checked only the captured ceiling; now that it must also be funded, a seed
    // that skips the credit is describing a capture that never happened.
    let posting = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, 'seed capture', $2, now())",
    )
    .bind(posting)
    .bind(format!("seed-capture-{transaction_id}"))
    .execute(pool)
    .await
    .expect("seed capture posting");
    sqlx::query(
        "INSERT INTO ledger_entries
           (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'DEBIT',  $5, 'AOA', now()),
                ($4, $2, $6, 'CREDIT', $5, 'AOA', now())",
    )
    .bind(Uuid::new_v4())
    .bind(posting)
    .bind(ledger_account(pool, "ASSET", "seed-transit").await)
    .bind(Uuid::new_v4())
    .bind(amount)
    .bind(available)
    .execute(pool)
    .await
    .expect("seed capture entries");

    Seed {
        merchant_id,
        transaction_id,
        merchant_account: available,
    }
}

/// (debit_sum, credit_sum) for a ledger posting identified by its idempotency key.
async fn posting_sums(pool: &PgPool, idempotency_key: &str) -> (i64, i64) {
    let debit = sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(e.amount_minor),0)::BIGINT FROM ledger_entries e
         JOIN ledger_postings p ON p.id = e.posting_id
         WHERE p.idempotency_key = $1 AND e.entry_type = 'DEBIT'",
    )
    .bind(idempotency_key)
    .fetch_one(pool)
    .await
    .unwrap();
    let credit = sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(e.amount_minor),0)::BIGINT FROM ledger_entries e
         JOIN ledger_postings p ON p.id = e.posting_id
         WHERE p.idempotency_key = $1 AND e.entry_type = 'CREDIT'",
    )
    .bind(idempotency_key)
    .fetch_one(pool)
    .await
    .unwrap();
    (debit, credit)
}

async fn count_postings(pool: &PgPool, idempotency_key: &str) -> i64 {
    sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM ledger_postings WHERE idempotency_key = $1",
    )
    .bind(idempotency_key)
    .fetch_one(pool)
    .await
    .unwrap()
}

fn refund_body(seed: &Seed, amount: i64, key: &str) -> refunds::CreateRefundBody {
    refunds::CreateRefundBody {
        source_type: None, // defaults to TRANSACTION
        source_id: None,
        transaction_id: Some(seed.transaction_id.to_string()),
        merchant_id: seed.merchant_id.to_string(),
        amount_minor: amount,
        currency: Some("AOA".into()),
        reason: Some("test".into()),
        idempotency_key: key.to_string(),
    }
}

/// A seeded wallet-native merchant payment with both wallets provisioned.
struct WalletPaymentSeed {
    merchant_id: Uuid,
    merchant_account: Uuid,
    consumer_id: Uuid,
    consumer_account: Uuid,
    wallet_payment_id: Uuid,
}

async fn seed_wallet_payment(pool: &PgPool, amount: i64) -> WalletPaymentSeed {
    // Merchant wallet.
    let merchant_id = Uuid::new_v4();
    let m_avail = ledger_account_typed(pool, "LIABILITY", "wp-merchant-available").await;
    let m_res = ledger_account_typed(pool, "LIABILITY", "wp-merchant-reserved").await;
    let m_wallet = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO wallets (id, merchant_id, currency, status, available_account_id, reserved_account_id)
         VALUES ($1, $2, 'AOA', 'ACTIVE', $3, $4)",
    )
    .bind(m_wallet).bind(merchant_id).bind(m_avail).bind(m_res)
    .execute(pool).await.unwrap();

    // Consumer + consumer wallet.
    let consumer_id = Uuid::new_v4();
    sqlx::query("INSERT INTO consumers (id, handle, status) VALUES ($1, $2, 'ACTIVE')")
        .bind(consumer_id)
        .bind(format!("c{}", &consumer_id.to_string()[..8]))
        .execute(pool)
        .await
        .unwrap();
    let c_avail = ledger_account_typed(pool, "LIABILITY", "wp-consumer-available").await;
    let c_res = ledger_account_typed(pool, "LIABILITY", "wp-consumer-reserved").await;
    sqlx::query(
        "INSERT INTO consumer_wallets (id, consumer_id, currency, status, available_account_id, reserved_account_id)
         VALUES ($1, $2, 'AOA', 'ACTIVE', $3, $4)",
    )
    .bind(Uuid::new_v4()).bind(consumer_id).bind(c_avail).bind(c_res)
    .execute(pool).await.unwrap();

    // The wallet-native merchant payment (COMPLETED).
    let wp_id = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO wallet_payments
            (id, transfer_id, merchant_id, consumer_id, amount_minor, currency, status, trace_id, environment)
         VALUES ($1, $2, $3, $4, $5, 'AOA', 'COMPLETED', 'trace', 'SANDBOX')",
    )
    .bind(wp_id).bind(Uuid::new_v4()).bind(merchant_id).bind(consumer_id).bind(amount)
    .execute(pool).await.unwrap();

    // A COMPLETED wallet payment means the merchant holds the money — same
    // reason as the acquiring seed above.
    let posting = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, 'seed wallet payment', $2, now())",
    )
    .bind(posting)
    .bind(format!("seed-wp-{wp_id}"))
    .execute(pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO ledger_entries
           (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'DEBIT',  $5, 'AOA', now()),
                ($4, $2, $6, 'CREDIT', $5, 'AOA', now())",
    )
    .bind(Uuid::new_v4())
    .bind(posting)
    .bind(c_avail)
    .bind(Uuid::new_v4())
    .bind(amount)
    .bind(m_avail)
    .execute(pool)
    .await
    .unwrap();

    WalletPaymentSeed {
        merchant_id,
        merchant_account: m_avail,
        consumer_id,
        consumer_account: c_avail,
        wallet_payment_id: wp_id,
    }
}

async fn ledger_account_typed(pool: &PgPool, atype: &str, name: &str) -> Uuid {
    sqlx::query_scalar::<_, Uuid>(
        "INSERT INTO ledger_accounts (id, account_type, name, currency)
         VALUES ($1, $2, $3, 'AOA') RETURNING id",
    )
    .bind(Uuid::new_v4())
    .bind(atype)
    .bind(name)
    .fetch_one(pool)
    .await
    .unwrap()
}

fn wp_refund_body(s: &WalletPaymentSeed, amount: i64, key: &str) -> refunds::CreateRefundBody {
    refunds::CreateRefundBody {
        source_type: Some("WALLET_PAYMENT".into()),
        source_id: Some(s.wallet_payment_id.to_string()),
        transaction_id: None,
        merchant_id: s.merchant_id.to_string(),
        amount_minor: amount,
        currency: Some("AOA".into()),
        reason: Some("wallet-native refund".into()),
        idempotency_key: key.to_string(),
    }
}

// ═══════════════════════════ REF-001 — refunds ═══════════════════════════

// INV-REF-001 (correct debit/credit + no money creation): a refund posts a
// balanced double entry — one DEBIT on the merchant account equal to one CREDIT
// on the transit account; debits == credits, so no value is created.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_posts_balanced_double_entry(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;

    let (status, Json(resp)) = refunds::create(State(state), Json(refund_body(&seed, 1_000, "r1")))
        .await
        .expect("refund should succeed");

    assert_eq!(status, axum::http::StatusCode::CREATED);
    assert_eq!(
        resp.status, "SUCCEEDED",
        "status transition PENDING→SUCCEEDED"
    );

    let key = format!("refund-{}", resp.id);
    let (debit, credit) = posting_sums(&pool, &key).await;
    assert_eq!(debit, 1_000, "merchant debited full amount");
    assert_eq!(credit, 1_000, "transit credited full amount");
    assert_eq!(debit, credit, "no money creation: debits == credits");

    // The debit must hit the merchant's available account.
    let merchant_debit = sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(e.amount_minor),0)::BIGINT FROM ledger_entries e
         JOIN ledger_postings p ON p.id = e.posting_id
         WHERE p.idempotency_key = $1 AND e.entry_type='DEBIT' AND e.account_id = $2",
    )
    .bind(&key)
    .bind(seed.merchant_account)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(
        merchant_debit, 1_000,
        "debit posted to merchant available account"
    );
}

// INV-REF-001-1 (refund ceiling): the sum of refunds may not exceed the
// captured amount; an over-refund is blocked at write time.
#[sqlx::test(migrations = "../../db/migrations")]
async fn partial_refunds_cannot_exceed_captured(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;

    // 600 ok
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 600, "p1")))
        .await
        .expect("first partial refund ok");

    // 600 + 500 = 1100 > 1000 → blocked
    let err = refunds::create(State(state.clone()), Json(refund_body(&seed, 500, "p2")))
        .await
        .expect_err("over-refund must be blocked");
    assert_eq!(err.code, "REFUND_EXCEEDS_CAPTURED");

    // 600 + 400 = 1000 → exactly the ceiling, ok
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 400, "p3")))
        .await
        .expect("refund up to the ceiling ok");

    // One more cent over the ceiling → blocked
    let err = refunds::create(State(state), Json(refund_body(&seed, 1, "p4")))
        .await
        .expect_err("any amount beyond the ceiling must be blocked");
    assert_eq!(err.code, "REFUND_EXCEEDS_CAPTURED");
}

// INV-REF-001-2 (idempotency / no double refund): replaying the same
// idempotency key returns the same refund and creates no duplicate ledger
// posting or entries.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_idempotent_replay_is_single_posting(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;

    let (_, Json(first)) = refunds::create(
        State(state.clone()),
        Json(refund_body(&seed, 700, "same-key")),
    )
    .await
    .expect("first refund");
    let (_, Json(second)) =
        refunds::create(State(state), Json(refund_body(&seed, 700, "same-key")))
            .await
            .expect("replay returns existing refund");

    assert_eq!(first.id, second.id, "replay returns the same refund id");

    // Exactly one refund row and one ledger posting / one DR-CR pair.
    let refund_rows = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM refunds WHERE idempotency_key = $1",
    )
    .bind("same-key")
    .fetch_one(&pool)
    .await
    .unwrap_or(0);
    assert_eq!(refund_rows, 1, "no duplicate refund row");

    let key = format!("refund-{}", first.id);
    assert_eq!(
        count_postings(&pool, &key).await,
        1,
        "single ledger posting"
    );
    let (debit, credit) = posting_sums(&pool, &key).await;
    assert_eq!(
        (debit, credit),
        (700, 700),
        "no double refund, still balanced"
    );
}

// Status-transition guard: only CAPTURED/SETTLED transactions are refundable.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_rejected_on_non_captured_transaction(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    sqlx::query("UPDATE transactions SET status = 'PENDING' WHERE id = $1")
        .bind(seed.transaction_id)
        .execute(&pool)
        .await
        .unwrap();

    let err = refunds::create(State(state), Json(refund_body(&seed, 100, "x")))
        .await
        .expect_err("non-captured transaction is not refundable");
    assert_eq!(err.code, "INVALID_TRANSACTION_STATUS");
}

// Auditability/traceability: a successful refund writes a refund event and an
// immutable audit-log entry.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_is_traceable_via_event_and_audit_log(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let (_, Json(resp)) = refunds::create(State(state), Json(refund_body(&seed, 500, "t1")))
        .await
        .expect("refund");

    let events = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM refund_events WHERE refund_id = $1 AND event_type = 'refund.succeeded'",
    )
    .bind(Uuid::parse_str(&resp.id).unwrap())
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(events, 1, "refund.succeeded event recorded");

    let audits = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM audit_log WHERE action = 'REFUND_PROCESSED' AND subject = $1",
    )
    .bind(format!("transaction:{}", seed.transaction_id))
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(audits, 1, "immutable audit-log entry recorded");
}

// ── REF-001 (Step 2B) — wallet-native (source_type = WALLET_PAYMENT) ─────────

// A wallet-native refund credits the original payer's consumer wallet (not
// transit) with a balanced double entry: merchant.available DR == consumer CR.
#[sqlx::test(migrations = "../../db/migrations")]
async fn wallet_native_refund_credits_consumer(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let s = seed_wallet_payment(&pool, 2_000).await;

    let (_, Json(resp)) = refunds::create(State(state), Json(wp_refund_body(&s, 2_000, "wn1")))
        .await
        .expect("wallet-native refund should succeed");
    assert_eq!(resp.status, "SUCCEEDED");
    assert_eq!(resp.source_type, "WALLET_PAYMENT");
    assert_eq!(
        resp.consumer_id.as_deref(),
        Some(s.consumer_id.to_string().as_str())
    );

    let key = format!("refund-{}", resp.id);
    let (debit, credit) = posting_sums(&pool, &key).await;
    assert_eq!(debit, 2_000, "merchant debited");
    assert_eq!(credit, 2_000, "consumer credited");
    assert_eq!(debit, credit, "no money creation");

    // The DR hits the merchant account and the CR hits the consumer account.
    let merchant_dr = entry_on_account(&pool, &key, "DEBIT", s.merchant_account).await;
    let consumer_cr = entry_on_account(&pool, &key, "CREDIT", s.consumer_account).await;
    assert_eq!(merchant_dr, 2_000, "merchant available debited");
    assert_eq!(
        consumer_cr, 2_000,
        "consumer available credited (not transit)"
    );
}

// Over-refund is rejected and partials aggregate by the wallet-payment source.
#[sqlx::test(migrations = "../../db/migrations")]
async fn wallet_native_ceiling_by_source(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let s = seed_wallet_payment(&pool, 1_000).await;

    let _ = refunds::create(State(state.clone()), Json(wp_refund_body(&s, 600, "c1")))
        .await
        .expect("partial ok");
    let err = refunds::create(State(state.clone()), Json(wp_refund_body(&s, 500, "c2")))
        .await
        .expect_err("over-refund blocked");
    assert_eq!(err.code, "REFUND_EXCEEDS_CAPTURED");
    let _ = refunds::create(State(state), Json(wp_refund_body(&s, 400, "c3")))
        .await
        .expect("refund to the ceiling ok");
}

// Idempotent replay of a wallet-native refund creates a single posting.
#[sqlx::test(migrations = "../../db/migrations")]
async fn wallet_native_refund_idempotent(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let s = seed_wallet_payment(&pool, 1_000).await;
    let (_, Json(a)) = refunds::create(State(state.clone()), Json(wp_refund_body(&s, 700, "same")))
        .await
        .unwrap();
    let (_, Json(b)) = refunds::create(State(state), Json(wp_refund_body(&s, 700, "same")))
        .await
        .unwrap();
    assert_eq!(a.id, b.id, "replay returns same refund");
    assert_eq!(count_postings(&pool, &format!("refund-{}", a.id)).await, 1);
}

// Only the owning merchant may refund a wallet payment. Cross-tenant access is
// indistinguishable from an unknown source (F2): NOT_FOUND, "refund source not
// found" — never REFUND_NOT_AUTHORIZED or any ownership-revealing message.
#[sqlx::test(migrations = "../../db/migrations")]
async fn wrong_merchant_cannot_refund_wallet_payment(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let s = seed_wallet_payment(&pool, 1_000).await;
    let mut body = wp_refund_body(&s, 500, "x");
    body.merchant_id = Uuid::new_v4().to_string(); // a different merchant
    let err = refunds::create(State(state), Json(body))
        .await
        .expect_err("foreign merchant cannot refund");
    assert_eq!(err.code, "NOT_FOUND");
    assert_eq!(err.message, "refund source not found");
    assert_ne!(err.code, "REFUND_NOT_AUTHORIZED");
}

// A P2P transfer (no wallet_payment row) is not refundable through this path.
#[sqlx::test(migrations = "../../db/migrations")]
async fn p2p_transfer_is_not_refundable(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let body = refunds::CreateRefundBody {
        source_type: Some("WALLET_PAYMENT".into()),
        source_id: Some(Uuid::new_v4().to_string()), // a transfer/unknown id
        transaction_id: None,
        merchant_id: Uuid::new_v4().to_string(),
        amount_minor: 100,
        currency: Some("AOA".into()),
        reason: None,
        idempotency_key: "p2p".into(),
    };
    let err = refunds::create(State(state), Json(body))
        .await
        .expect_err("no wallet payment exists");
    assert_eq!(err.code, "NOT_FOUND");
}

// The source wallet_payment is never mutated by a refund.
#[sqlx::test(migrations = "../../db/migrations")]
async fn wallet_payment_source_not_modified(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let s = seed_wallet_payment(&pool, 2_000).await;
    let _ = refunds::create(State(state), Json(wp_refund_body(&s, 1_000, "nm")))
        .await
        .unwrap();
    let (status, amount) = sqlx::query_as::<_, (String, i64)>(
        "SELECT status, amount_minor FROM wallet_payments WHERE id = $1",
    )
    .bind(s.wallet_payment_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(status, "COMPLETED", "source status unchanged");
    assert_eq!(amount, 2_000, "source amount unchanged");
}

async fn webhook_events_for(pool: &PgPool, event_type: &str, merchant: Uuid) -> i64 {
    sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM webhook_events
         WHERE event_type = $1 AND merchant_id = $2",
    )
    .bind(event_type)
    .bind(merchant)
    .fetch_one(pool)
    .await
    .unwrap()
}

// refund.completed is emitted to the outbox for the source merchant, once.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_emits_completed_event_idempotently(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;

    for _ in 0..2 {
        let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 500, "rk")))
            .await
            .expect("refund");
    }
    // Idempotent replay → exactly one event, for the right merchant.
    assert_eq!(
        webhook_events_for(&pool, "refund.completed", seed.merchant_id).await,
        1,
        "one refund.completed for the source merchant"
    );
    // Not delivered to an unrelated merchant.
    assert_eq!(
        webhook_events_for(&pool, "refund.completed", Uuid::new_v4()).await,
        0
    );
    // Left undispatched for the gateway fan-out worker.
    let undispatched = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM webhook_events
         WHERE event_type = 'refund.completed' AND dispatched_at IS NULL",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(undispatched, 1, "outbox row awaits fan-out");
}

async fn entry_on_account(pool: &PgPool, key: &str, etype: &str, account: Uuid) -> i64 {
    sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(e.amount_minor),0)::BIGINT FROM ledger_entries e
         JOIN ledger_postings p ON p.id = e.posting_id
         WHERE p.idempotency_key = $1 AND e.entry_type = $2 AND e.account_id = $3",
    )
    .bind(key)
    .bind(etype)
    .bind(account)
    .fetch_one(pool)
    .await
    .unwrap()
}

// ═══════════════════════════ REF-002 — disputes ═══════════════════════════

async fn open_dispute(state: &AppState, tx: Uuid, consumer: Uuid) -> disputes::DisputeResponse {
    let (_, Json(d)) = disputes::open(
        State(state.clone()),
        Json(disputes::OpenDisputeBody {
            transaction_id: tx.to_string(),
            consumer_id: consumer.to_string(),
            reason: "item not received".into(),
        }),
    )
    .await
    .expect("open dispute");
    d
}

// Lifecycle consistency: OPEN → UNDER_REVIEW (on evidence) → terminal (resolve).
#[sqlx::test(migrations = "../../db/migrations")]
async fn dispute_lifecycle_open_review_resolve(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let consumer = Uuid::new_v4();

    let d = open_dispute(&state, seed.transaction_id, consumer).await;
    assert_eq!(d.status, "OPEN");

    let did = Uuid::parse_str(&d.id).unwrap();
    let _ = disputes::submit_evidence(
        State(state.clone()),
        Path(d.id.clone()),
        Json(disputes::SubmitEvidenceBody {
            submitted_by: seed.merchant_id.to_string(),
            party: "MERCHANT".into(),
            description: "proof of delivery".into(),
            file_url: None,
        }),
    )
    .await
    .expect("submit evidence");

    let status = sqlx::query_scalar::<_, String>("SELECT status FROM disputes WHERE id = $1")
        .bind(did)
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(status, "UNDER_REVIEW", "evidence moves OPEN→UNDER_REVIEW");

    let Json(resolved) = disputes::resolve(
        State(state),
        Path(d.id.clone()),
        Json(disputes::ResolveDisputeBody {
            outcome: "WON_BY_MERCHANT".into(),
            resolution_notes: Some("evidence accepted".into()),
            resolved_by: Uuid::new_v4().to_string(),
        }),
    )
    .await
    .expect("resolve");
    assert_eq!(resolved.status, "WON_BY_MERCHANT");
    assert!(resolved.resolved_at.is_some(), "resolved_at stamped");
}

// A second open dispute on the same transaction is rejected.
#[sqlx::test(migrations = "../../db/migrations")]
async fn duplicate_open_dispute_rejected(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let consumer = Uuid::new_v4();
    open_dispute(&state, seed.transaction_id, consumer).await;

    let err = disputes::open(
        State(state),
        Json(disputes::OpenDisputeBody {
            transaction_id: seed.transaction_id.to_string(),
            consumer_id: consumer.to_string(),
            reason: "again".into(),
        }),
    )
    .await
    .expect_err("duplicate open dispute must be rejected");
    assert_eq!(err.code, "DISPUTE_ALREADY_OPEN");
}

// Resolve idempotency: a resolved dispute cannot be resolved again.
#[sqlx::test(migrations = "../../db/migrations")]
async fn resolve_is_not_repeatable(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;

    let body = || disputes::ResolveDisputeBody {
        outcome: "WON_BY_MERCHANT".into(),
        resolution_notes: None,
        resolved_by: Uuid::new_v4().to_string(),
    };
    let _ = disputes::resolve(State(state.clone()), Path(d.id.clone()), Json(body()))
        .await
        .expect("first resolve");
    let err = disputes::resolve(State(state), Path(d.id.clone()), Json(body()))
        .await
        .expect_err("second resolve must be rejected");
    assert_eq!(err.code, "DISPUTE_ALREADY_RESOLVED");
}

// Evidence cannot be submitted to a closed/resolved dispute.
#[sqlx::test(migrations = "../../db/migrations")]
async fn evidence_rejected_after_resolution(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;

    let _ = disputes::resolve(
        State(state.clone()),
        Path(d.id.clone()),
        Json(disputes::ResolveDisputeBody {
            outcome: "CLOSED".into(),
            resolution_notes: None,
            resolved_by: Uuid::new_v4().to_string(),
        }),
    )
    .await
    .expect("resolve");

    let err = disputes::submit_evidence(
        State(state),
        Path(d.id.clone()),
        Json(disputes::SubmitEvidenceBody {
            submitted_by: seed.merchant_id.to_string(),
            party: "MERCHANT".into(),
            description: "late".into(),
            file_url: None,
        }),
    )
    .await
    .expect_err("cannot submit evidence to a closed dispute");
    assert_eq!(err.code, "DISPUTE_CLOSED");
}

// WON_BY_CONSUMER issues a balanced refund posting (merchant DR == consumer CR)
// with no money creation, and is idempotent on the posting key.
#[sqlx::test(migrations = "../../db/migrations")]
async fn won_by_consumer_posts_balanced_refund(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;

    // Seed an active consumer + consumer wallet so the resolve posting can land.
    let consumer = Uuid::new_v4();
    sqlx::query("INSERT INTO consumers (id, handle, status) VALUES ($1, $2, 'ACTIVE')")
        .bind(consumer)
        .bind(format!("c{}", &consumer.to_string()[..8]))
        .execute(&pool)
        .await
        .unwrap();
    let c_avail = ledger_account(&pool, "LIABILITY", "consumer-available").await;
    let c_reserved = ledger_account(&pool, "LIABILITY", "consumer-reserved").await;
    sqlx::query(
        "INSERT INTO consumer_wallets (id, consumer_id, currency, status, available_account_id, reserved_account_id)
         VALUES ($1, $2, 'AOA', 'ACTIVE', $3, $4)",
    )
    .bind(Uuid::new_v4())
    .bind(consumer)
    .bind(c_avail)
    .bind(c_reserved)
    .execute(&pool)
    .await
    .unwrap();

    let d = open_dispute(&state, seed.transaction_id, consumer).await;
    let did = Uuid::parse_str(&d.id).unwrap();
    // ADR-030 §5: an acquiring (transaction) restitution credits transit, not a wallet.
    let transit = state.transit_account_id.as_uuid();

    let _ = disputes::resolve(
        State(state),
        Path(d.id.clone()),
        Json(disputes::ResolveDisputeBody {
            outcome: "WON_BY_CONSUMER".into(),
            resolution_notes: Some("consumer wins".into()),
            resolved_by: Uuid::new_v4().to_string(),
        }),
    )
    .await
    .expect("resolve won-by-consumer");

    let key = format!("dispute-refund-{did}");
    assert_eq!(
        count_postings(&pool, &key).await,
        1,
        "single dispute-refund posting"
    );
    let (debit, credit) = posting_sums(&pool, &key).await;
    assert_eq!(debit, 2_000, "merchant debited the disputed amount");
    assert_eq!(credit, 2_000, "consumer credited the disputed amount");
    assert_eq!(debit, credit, "no money creation: debits == credits");

    // Source-aware (ADR-030 §5): the acquiring restitution credit lands on transit.
    let transit_credit = sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(e.amount_minor),0)::BIGINT FROM ledger_entries e
         JOIN ledger_postings p ON p.id = e.posting_id
         WHERE p.idempotency_key = $1 AND e.entry_type='CREDIT' AND e.account_id = $2",
    )
    .bind(&key)
    .bind(transit)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(
        transit_credit, 2_000,
        "acquiring restitution credit lands on transit"
    );
    let _ = (c_avail, c_reserved);
}

// Auditability: opening and resolving a dispute both write immutable audit-log
// entries.
#[sqlx::test(migrations = "../../db/migrations")]
async fn dispute_open_and_resolve_are_audited(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let _ = disputes::resolve(
        State(state),
        Path(d.id.clone()),
        Json(disputes::ResolveDisputeBody {
            outcome: "WON_BY_MERCHANT".into(),
            resolution_notes: None,
            resolved_by: Uuid::new_v4().to_string(),
        }),
    )
    .await
    .expect("resolve");

    let opened = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM audit_log WHERE action = 'DISPUTE_OPENED' AND subject = $1",
    )
    .bind(format!("transaction:{}", seed.transaction_id))
    .fetch_one(&pool)
    .await
    .unwrap();
    let resolved = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM audit_log WHERE action = 'DISPUTE_RESOLVED' AND subject = $1",
    )
    .bind(format!("dispute:{}", d.id))
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(opened, 1, "DISPUTE_OPENED audited");
    assert_eq!(resolved, 1, "DISPUTE_RESOLVED audited");
}

// dispute.opened and dispute.resolved are emitted to the outbox for the affected
// merchant, each once (resolve is idempotent — a second resolve is rejected).
#[sqlx::test(migrations = "../../db/migrations")]
async fn dispute_emits_opened_and_resolved_events(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 2_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;

    let _ = disputes::resolve(
        State(state),
        Path(d.id.clone()),
        Json(disputes::ResolveDisputeBody {
            outcome: "WON_BY_MERCHANT".into(),
            resolution_notes: None,
            resolved_by: Uuid::new_v4().to_string(),
        }),
    )
    .await
    .expect("resolve");

    assert_eq!(
        webhook_events_for(&pool, "dispute.opened", seed.merchant_id).await,
        1,
        "one dispute.opened for the merchant"
    );
    assert_eq!(
        webhook_events_for(&pool, "dispute.resolved", seed.merchant_id).await,
        1,
        "one dispute.resolved for the merchant"
    );
    // Nothing leaks to an unrelated merchant.
    assert_eq!(
        webhook_events_for(&pool, "dispute.opened", Uuid::new_v4()).await,
        0
    );
}

// ═══════════════ ADR-034 — shared restitution ceiling (Workstream B) ═══════════════

async fn alloc_sum(pool: &PgPool, st: &str, sid: Uuid) -> i64 {
    sqlx::query_scalar::<_, i64>(
        "SELECT COALESCE(SUM(amount_minor),0)::BIGINT FROM restitution_allocations
         WHERE source_type = $1 AND source_id = $2",
    )
    .bind(st)
    .bind(sid)
    .fetch_one(pool)
    .await
    .unwrap()
}

async fn seed_proof(pool: &PgPool, tx: Uuid) {
    sqlx::query(
        "INSERT INTO transaction_proofs (proof_reference, transaction_id, environment, amount_minor, currency, status)
         VALUES ($1, $2, 'SANDBOX', 1000, 'AOA', 'CONFIRMED')",
    )
    .bind(format!("BZM-{}", &Uuid::new_v4().to_string()[..8]))
    .bind(tx.to_string())
    .execute(pool)
    .await
    .unwrap();
}

async fn proof_status(pool: &PgPool, tx: Uuid) -> String {
    sqlx::query_scalar::<_, String>(
        "SELECT status FROM transaction_proofs WHERE transaction_id = $1",
    )
    .bind(tx.to_string())
    .fetch_one(pool)
    .await
    .unwrap()
}

fn resolve_body() -> disputes::ResolveDisputeBody {
    disputes::ResolveDisputeBody {
        outcome: "WON_BY_CONSUMER".into(),
        resolution_notes: None,
        resolved_by: Uuid::new_v4().to_string(),
    }
}

async fn dispute_restitution(pool: &PgPool, did: Uuid) -> (Option<i64>, Option<String>, String) {
    sqlx::query_as::<_, (Option<i64>, Option<String>, String)>(
        "SELECT restitution_amount_minor, restitution_reason, status FROM disputes WHERE id = $1",
    )
    .bind(did)
    .fetch_one(pool)
    .await
    .unwrap()
}

// Source-derived currency: a supplied currency that differs from the source is rejected.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_rejects_currency_mismatch(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let mut body = refund_body(&seed, 500, "cm");
    body.currency = Some("USD".into());
    let err = refunds::create(State(state), Json(body))
        .await
        .expect_err("mismatch");
    assert_eq!(err.code, "CURRENCY_MISMATCH");
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        0,
        "no allocation on rejection"
    );
}

// A source_type of TRANSFER is never a valid refund source.
#[sqlx::test(migrations = "../../db/migrations")]
async fn transfer_source_type_rejected(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let mut body = refund_body(&seed, 500, "tf");
    body.source_type = Some("TRANSFER".into());
    body.source_id = Some(seed.transaction_id.to_string());
    let err = refunds::create(State(state), Json(body))
        .await
        .expect_err("transfer rejected");
    assert_eq!(err.code, "BAD_REQUEST");
}

// Reusing an idempotency key with a different amount is a conflict (not a silent replay).
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_idempotency_conflict_on_incompatible_reuse(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 400, "dup")))
        .await
        .unwrap();
    let err = refunds::create(State(state), Json(refund_body(&seed, 500, "dup")))
        .await
        .expect_err("conflict");
    assert_eq!(err.code, "IDEMPOTENCY_KEY_CONFLICT");
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        400,
        "only the original allocation"
    );
}

// A rejected over-refund leaves NO allocation, NO posting and NO refund row.
#[sqlx::test(migrations = "../../db/migrations")]
async fn rejected_refund_leaves_no_partial_mutation(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let err = refunds::create(State(state), Json(refund_body(&seed, 1_500, "over")))
        .await
        .expect_err("over");
    assert_eq!(err.code, "REFUND_EXCEEDS_CAPTURED");
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        0
    );
    let refunds_n: i64 =
        sqlx::query_scalar::<_, i64>("SELECT COUNT(*)::BIGINT FROM refunds WHERE source_id = $1")
            .bind(seed.transaction_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(refunds_n, 0, "no refund row");
}

// Combined ceiling: refund then dispute — the dispute restitutes only the remaining.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_then_dispute_shares_ceiling(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 600, "r")))
        .await
        .unwrap();
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let did = Uuid::parse_str(&d.id).unwrap();
    let _ = disputes::resolve(State(state), Path(d.id.clone()), Json(resolve_body()))
        .await
        .unwrap();
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        1_000,
        "combined never exceeds captured"
    );
    let (ra, reason, status) = dispute_restitution(&pool, did).await;
    assert_eq!(ra, Some(400), "dispute restitutes the remaining 400");
    assert_eq!(reason.as_deref(), Some("PARTIALLY_REFUNDED_NET_SETTLED"));
    assert_eq!(status, "WON_BY_CONSUMER");
}

// Fully refunded, then consumer wins the dispute: zero additional restitution, still WON.
#[sqlx::test(migrations = "../../db/migrations")]
async fn fully_refunded_then_dispute_zero_restitution(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let _ = refunds::create(
        State(state.clone()),
        Json(refund_body(&seed, 1_000, "full")),
    )
    .await
    .unwrap();
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let did = Uuid::parse_str(&d.id).unwrap();
    let _ = disputes::resolve(State(state), Path(d.id.clone()), Json(resolve_body()))
        .await
        .unwrap();

    // (1) NO restitution_allocations row for this dispute.
    let dispute_allocs: i64 = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*)::BIGINT FROM restitution_allocations WHERE origin='DISPUTE' AND origin_id=$1",
    )
    .bind(did).fetch_one(&pool).await.unwrap();
    assert_eq!(
        dispute_allocs, 0,
        "zero-restitution dispute creates NO allocation row"
    );

    // (2) NO financial posting.
    assert_eq!(
        count_postings(&pool, &format!("dispute-refund-{did}")).await,
        0,
        "no dispute posting"
    );

    // (3)+(4)+(5) restitution_amount_minor = 0, ALREADY_MADE_WHOLE, outcome persisted
    // atomically (all three fields consistent in the single committed dispute row).
    let (ra, reason, status) = dispute_restitution(&pool, did).await;
    assert_eq!(ra, Some(0), "restitution_amount_minor = 0");
    assert_eq!(reason.as_deref(), Some("ALREADY_MADE_WHOLE"));
    assert_eq!(
        status, "WON_BY_CONSUMER",
        "dispute outcome persisted, not misrepresented as failed"
    );

    // The combined ceiling still holds — only the earlier full refund counts.
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        1_000,
        "≤ captured, refund only"
    );
}

// Dispute won (full), then a later refund is rejected — nothing remains.
#[sqlx::test(migrations = "../../db/migrations")]
async fn dispute_then_refund_rejected(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let _ = disputes::resolve(
        State(state.clone()),
        Path(d.id.clone()),
        Json(resolve_body()),
    )
    .await
    .unwrap();
    let err = refunds::create(State(state), Json(refund_body(&seed, 100, "late")))
        .await
        .expect_err("nothing left");
    assert_eq!(err.code, "REFUND_EXCEEDS_CAPTURED");
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        1_000
    );
}

// Concurrency: two parallel refunds can never over-restitute (advisory lock serializes).
#[sqlx::test(migrations = "../../db/migrations")]
async fn concurrent_refunds_never_over_restitute(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let f1 = refunds::create(State(state.clone()), Json(refund_body(&seed, 600, "a")));
    let f2 = refunds::create(State(state.clone()), Json(refund_body(&seed, 600, "b")));
    let (r1, r2) = tokio::join!(f1, f2);
    let oks = [r1.is_ok(), r2.is_ok()].iter().filter(|x| **x).count();
    assert_eq!(
        oks, 1,
        "exactly one of two 600 refunds succeeds against a 1000 cap"
    );
    assert!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await <= 1_000,
        "never over captured"
    );
}

// Concurrency: a refund and a dispute race on one source — combined stays ≤ captured.
#[sqlx::test(migrations = "../../db/migrations")]
async fn concurrent_refund_and_dispute_share_ceiling(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let f_refund = refunds::create(State(state.clone()), Json(refund_body(&seed, 700, "rc")));
    let f_dispute = disputes::resolve(
        State(state.clone()),
        Path(d.id.clone()),
        Json(resolve_body()),
    );
    let (_r, _d) = tokio::join!(f_refund, f_dispute);
    assert!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await <= 1_000,
        "combined never exceeds captured"
    );
}

// Concurrency: two parallel resolves of one dispute produce a single posting.
#[sqlx::test(migrations = "../../db/migrations")]
async fn concurrent_dispute_resolves_single_posting(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let did = Uuid::parse_str(&d.id).unwrap();
    let f1 = disputes::resolve(
        State(state.clone()),
        Path(d.id.clone()),
        Json(resolve_body()),
    );
    let f2 = disputes::resolve(
        State(state.clone()),
        Path(d.id.clone()),
        Json(resolve_body()),
    );
    let _ = tokio::join!(f1, f2);
    assert_eq!(
        count_postings(&pool, &format!("dispute-refund-{did}")).await,
        1,
        "single dispute posting"
    );
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        1_000
    );
}

// Proof: partial restitution keeps CONFIRMED; full cumulative → REVERSED (no PARTIALLY_REVERSED).
#[sqlx::test(migrations = "../../db/migrations")]
async fn proof_confirmed_on_partial_reversed_on_full(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    seed_proof(&pool, seed.transaction_id).await;

    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 400, "p1")))
        .await
        .unwrap();
    assert_eq!(
        proof_status(&pool, seed.transaction_id).await,
        "CONFIRMED",
        "partial keeps CONFIRMED"
    );

    let _ = refunds::create(State(state), Json(refund_body(&seed, 600, "p2")))
        .await
        .unwrap();
    assert_eq!(
        proof_status(&pool, seed.transaction_id).await,
        "REVERSED",
        "full cumulative → REVERSED"
    );
}

// Operator-facing separation: a refund is a refund, a dispute is a dispute — never crossed.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_and_dispute_remain_separate(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 500, "sep")))
        .await
        .unwrap();
    let d = open_dispute(&state, seed.transaction_id, Uuid::new_v4()).await;
    let _ = disputes::resolve(State(state), Path(d.id.clone()), Json(resolve_body()))
        .await
        .unwrap();

    let refunds_n: i64 =
        sqlx::query_scalar::<_, i64>("SELECT COUNT(*)::BIGINT FROM refunds WHERE source_id = $1")
            .bind(seed.transaction_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    let disputes_n: i64 =
        sqlx::query_scalar::<_, i64>("SELECT COUNT(*)::BIGINT FROM disputes WHERE source_id = $1")
            .bind(seed.transaction_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    let alloc_refund: i64 = sqlx::query_scalar::<_, i64>("SELECT COUNT(*)::BIGINT FROM restitution_allocations WHERE origin='REFUND' AND source_id=$1")
        .bind(seed.transaction_id).fetch_one(&pool).await.unwrap();
    let alloc_dispute: i64 = sqlx::query_scalar::<_, i64>("SELECT COUNT(*)::BIGINT FROM restitution_allocations WHERE origin='DISPUTE' AND source_id=$1")
        .bind(seed.transaction_id).fetch_one(&pool).await.unwrap();
    assert_eq!(refunds_n, 1, "one refund object");
    assert_eq!(disputes_n, 1, "one dispute object");
    assert_eq!(alloc_refund, 1, "one REFUND allocation");
    assert_eq!(alloc_dispute, 1, "one DISPUTE allocation");
    assert_eq!(
        alloc_sum(&pool, "TRANSACTION", seed.transaction_id).await,
        1_000,
        "combined ≤ captured"
    );
}

// ═══════════════ D0/F1+F3 — tenant-scoped refund reads ═══════════════

// F1: refund GET is tenant-scoped. The owner reads it; a different merchant, a
// missing merchant context, and an unknown id all return the SAME controlled
// 404 "refund not found" — externally indistinguishable, no enumeration.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_get_is_tenant_scoped_and_indistinguishable(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let seed = seed_captured_tx(&pool, 1_000).await;
    let (_, Json(created)) =
        refunds::create(State(state.clone()), Json(refund_body(&seed, 400, "g")))
            .await
            .unwrap();
    let rid = created.id.clone();

    // Owner → ok.
    let owner = refunds::get(
        State(state.clone()),
        Path(rid.clone()),
        Query(refunds::GetRefundQuery {
            merchant_id: Some(seed.merchant_id.to_string()),
        }),
    )
    .await;
    assert!(owner.is_ok(), "owner can read its refund");

    // Cross-tenant → NOT_FOUND.
    let other = refunds::get(
        State(state.clone()),
        Path(rid.clone()),
        Query(refunds::GetRefundQuery {
            merchant_id: Some(Uuid::new_v4().to_string()),
        }),
    )
    .await
    .expect_err("cross-tenant read blocked");
    assert_eq!(other.code, "NOT_FOUND");
    assert_eq!(other.message, "refund not found");

    // Missing merchant context → NOT_FOUND (fail closed, indistinguishable).
    let nomid = refunds::get(
        State(state.clone()),
        Path(rid.clone()),
        Query(refunds::GetRefundQuery { merchant_id: None }),
    )
    .await
    .expect_err("no merchant context");
    assert_eq!(nomid.code, "NOT_FOUND");

    // Unknown id (valid merchant) → byte-identical NOT_FOUND to the cross-tenant case.
    let unknown = refunds::get(
        State(state),
        Path(Uuid::new_v4().to_string()),
        Query(refunds::GetRefundQuery {
            merchant_id: Some(seed.merchant_id.to_string()),
        }),
    )
    .await
    .expect_err("unknown refund");
    assert_eq!(
        (unknown.code, unknown.message.clone()),
        (other.code, other.message.clone())
    );
}

// F3: refund LIST fails closed — no merchant context can never enumerate tenants.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_list_fails_closed_without_merchant(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    // Seed a refund so the table is non-empty — a fail-open bug would leak it.
    let seed = seed_captured_tx(&pool, 1_000).await;
    let _ = refunds::create(State(state.clone()), Json(refund_body(&seed, 100, "l")))
        .await
        .unwrap();

    // No merchant_id → rejected, never enumerates.
    let err = refunds::list(
        State(state.clone()),
        Query(refunds::ListRefundsQuery {
            source_id: None,
            transaction_id: None,
            merchant_id: None,
            limit: None,
        }),
    )
    .await
    .expect_err("list must fail closed");
    assert_eq!(err.code, "BAD_REQUEST");

    // A different merchant → scoped result never contains the seed's refund.
    let Json(other) = refunds::list(
        State(state),
        Query(refunds::ListRefundsQuery {
            source_id: None,
            transaction_id: None,
            merchant_id: Some(Uuid::new_v4().to_string()),
            limit: None,
        }),
    )
    .await
    .unwrap();
    assert_eq!(
        other["data"].as_array().unwrap().len(),
        0,
        "other merchant sees nothing"
    );
}

// ═══════════════ D0 — source-scoped refund idempotency (0098) ═══════════════

/// Insert a second CAPTURED acquiring source under an EXISTING merchant, reusing
/// the merchant's single per-currency wallet (wallets are unique on merchant+currency).
async fn seed_extra_tx(pool: &PgPool, merchant_id: Uuid, amount: i64) -> Uuid {
    let wallet_id: Uuid = sqlx::query_scalar(
        "SELECT id FROM wallets WHERE merchant_id = $1 AND currency = 'AOA' LIMIT 1",
    )
    .bind(merchant_id)
    .fetch_one(pool)
    .await
    .unwrap();
    let tx = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO transactions (id, idempotency_key, transaction_type, status, amount_minor, fee_minor, currency, merchant_id, wallet_id, environment)
         VALUES ($1, $2, 'PAYMENT', 'CAPTURED', $3, 0, 'AOA', $4, $5, 'SANDBOX')",
    )
    .bind(tx).bind(format!("extra-{tx}")).bind(amount).bind(merchant_id).bind(wallet_id)
    .execute(pool).await.unwrap();
    tx
}

fn typed_refund_body(
    mid: Uuid,
    stype: Option<&str>,
    sid: Uuid,
    amount: i64,
    key: &str,
) -> refunds::CreateRefundBody {
    refunds::CreateRefundBody {
        source_type: stype.map(String::from),
        source_id: Some(sid.to_string()),
        transaction_id: None,
        merchant_id: mid.to_string(),
        amount_minor: amount,
        currency: Some("AOA".into()),
        reason: None,
        idempotency_key: key.to_string(),
    }
}

// Same idempotency key on DIFFERENT sources — same merchant, another merchant,
// and a different source TYPE — is INDEPENDENT (no global collision, no 500, no
// cross-tenant key-usage signal). Pre-0098 the 2nd+ of these 500'd.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_idempotency_is_source_scoped_across_sources_and_merchants(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let a = seed_captured_tx(&pool, 1_000).await; // merchant A, acquiring A1
    let a2 = seed_extra_tx(&pool, a.merchant_id, 1_000).await; // same merchant A, source A2
    let b = seed_captured_tx(&pool, 1_000).await; // merchant B, acquiring B1
    let wp = seed_wallet_payment(&pool, 1_000).await; // wallet-native source

    let key = "SHARED-IDEMPOTENCY-KEY";
    let cases: Vec<(&str, Uuid, Option<&str>, Uuid)> = vec![
        ("A1 acquiring", a.merchant_id, None, a.transaction_id),
        ("A2 same-merchant diff-source", a.merchant_id, None, a2),
        ("B1 another merchant", b.merchant_id, None, b.transaction_id),
        (
            "WP wallet source",
            wp.merchant_id,
            Some("WALLET_PAYMENT"),
            wp.wallet_payment_id,
        ),
    ];
    for (label, mid, stype, sid) in cases {
        match refunds::create(
            State(state.clone()),
            Json(typed_refund_body(mid, stype, sid, 200, key)),
        )
        .await
        {
            Ok((code, _)) => assert_eq!(code, StatusCode::CREATED, "{label} should succeed"),
            Err(e) => panic!(
                "{label} must be independent success, got {} {}",
                e.code, e.message
            ),
        }
    }
    // Four independent refunds, all sharing the same key, on different sources.
    let n: i64 =
        sqlx::query_scalar("SELECT COUNT(*)::BIGINT FROM refunds WHERE idempotency_key = $1")
            .bind(key)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(
        n, 4,
        "same key on 4 distinct sources → 4 independent refunds"
    );
}

// Concurrent use of the same key by two merchants on their own sources: both
// succeed, no global collision, all postings balanced.
#[sqlx::test(migrations = "../../db/migrations")]
async fn concurrent_same_key_different_sources_both_succeed(pool: PgPool) {
    let state = build_state(pool.clone()).await;
    let a = seed_captured_tx(&pool, 1_000).await;
    let b = seed_captured_tx(&pool, 1_000).await;
    let key = "CONCURRENT-SHARED-KEY";
    let f1 = refunds::create(State(state.clone()), Json(refund_body(&a, 300, key)));
    let f2 = refunds::create(State(state.clone()), Json(refund_body(&b, 300, key)));
    let (r1, r2) = tokio::join!(f1, f2);
    assert!(
        r1.is_ok(),
        "merchant A concurrent refund failed: {:?}",
        r1.err().map(|e| e.code)
    );
    assert!(
        r2.is_ok(),
        "merchant B concurrent refund failed: {:?}",
        r2.err().map(|e| e.code)
    );
    // both postings balanced (global check over refund postings)
    let unbal: i64 = sqlx::query_scalar(
        "SELECT COUNT(*)::BIGINT FROM (
           SELECT p.id FROM ledger_postings p JOIN ledger_entries e ON e.posting_id=p.id
           WHERE p.idempotency_key LIKE 'refund-%'
           GROUP BY p.id HAVING SUM(CASE WHEN e.entry_type='DEBIT' THEN e.amount_minor ELSE -e.amount_minor END) <> 0) x")
        .fetch_one(&pool).await.unwrap();
    assert_eq!(unbal, 0, "all refund postings balanced");
}

// The refund write mapper resolves a real uniqueness collision to the controlled
// 409 and NEVER leaks constraint/table/SQL/DB text; other DB errors → neutral.
#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_write_err_maps_unique_to_409_and_never_leaks(pool: PgPool) {
    let mid = Uuid::new_v4();
    let sid = Uuid::new_v4();
    let wid = Uuid::new_v4();
    let insert = |id: Uuid| {
        sqlx::query(
            "INSERT INTO refunds (id, source_type, source_id, merchant_id, wallet_id, amount_minor, currency, status, idempotency_key, processed_at, updated_at)
             VALUES ($1, 'TRANSACTION', $2, $3, $4, 100, 'AOA', 'SUCCEEDED', 'dupkey', now(), now())",
        )
        .bind(id).bind(sid).bind(mid).bind(wid)
    };
    insert(Uuid::new_v4())
        .execute(&pool)
        .await
        .expect("first insert ok");
    // duplicate (source_type, source_id, idempotency_key) → real 23505
    let dup = insert(Uuid::new_v4())
        .execute(&pool)
        .await
        .expect_err("duplicate must fail");
    let api = refunds::refund_write_err(dup);
    assert_eq!(api.status, StatusCode::CONFLICT);
    assert_eq!(api.code, "IDEMPOTENCY_KEY_CONFLICT");

    let leak = api.message.to_lowercase();
    for bad in [
        "refunds_idempotency_key_key",
        "refunds_source_idem_unique",
        "postgres",
        "sqlx",
        "duplicate key",
        "constraint",
        "insert ",
        "23505",
        "relation",
        "refunds",
    ] {
        assert!(
            !leak.contains(bad),
            "409 message leaked '{bad}': {}",
            api.message
        );
    }

    // A non-unique DB failure (NOT NULL violation) → neutral internal, no leak.
    let generic = sqlx::query("INSERT INTO refunds (id) VALUES ($1)")
        .bind(Uuid::new_v4())
        .execute(&pool)
        .await
        .expect_err("not-null violation");
    let api2 = refunds::refund_write_err(generic);
    assert_eq!(api2.code, "INTERNAL_ERROR");
    assert_eq!(api2.message, "refund could not be processed");
    let leak2 = api2.message.to_lowercase();
    for bad in [
        "null value",
        "not-null",
        "column",
        "postgres",
        "sqlx",
        "refunds",
    ] {
        assert!(
            !leak2.contains(bad),
            "internal message leaked '{bad}': {}",
            api2.message
        );
    }
}

// ─── entitlement is not fundability ──────────────────────────────────────────
//
// Refundability asks "how much of this payment has not been returned yet". It is
// a fact about history, and it survives the money leaving. Whether the money is
// still here is a different question, and nothing asked it.
//
// On the deployed Sandbox, a payment of 100 000 was captured, settled in full,
// and then refunded in full. The refund was accepted with 201 and drove the
// merchant's liability account to -98 000: the operator returned money it did
// not hold. Both ledger legs were present, so every per-posting balance check
// stayed green — the invariant that catches this is "no account below zero", and
// nothing asked it at the point of the write.

#[sqlx::test(migrations = "../../db/migrations")]
async fn refund_is_refused_when_the_merchant_no_longer_holds_the_funds(pool: PgPool) {
    let seed = seed_captured_tx(&pool, 100_000).await;
    let state = build_state(pool.clone()).await;

    // The value leaves, as a settlement or a payout would take it.
    let elsewhere = ledger_account(&pool, "LIABILITY", "somewhere-else").await;
    let posting = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
         VALUES ($1, 'value settled away', 'settle-away', now())",
    )
    .bind(posting)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO ledger_entries
           (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
         VALUES ($1, $2, $3, 'DEBIT',  100000, 'AOA', now()),
                ($4, $2, $5, 'CREDIT', 100000, 'AOA', now())",
    )
    .bind(Uuid::new_v4())
    .bind(posting)
    .bind(seed.merchant_account)
    .bind(Uuid::new_v4())
    .bind(elsewhere)
    .execute(&pool)
    .await
    .unwrap();

    let err = refunds::create(
        axum::extract::State(state),
        axum::Json(refund_body(&seed, 100_000, "after-settlement")),
    )
    .await
    .expect_err("a refund with no funds behind it must be refused");
    assert_eq!(
        err.code, "REFUND_NOT_FUNDABLE",
        "expected REFUND_NOT_FUNDABLE, got {}",
        err.code
    );

    // The account that would have funded it is untouched, and nothing anywhere
    // went below zero.
    let balance: i64 = sqlx::query_scalar(
        "SELECT COALESCE(SUM(CASE WHEN entry_type='CREDIT' THEN amount_minor
                                  ELSE -amount_minor END), 0)::BIGINT
           FROM ledger_entries WHERE account_id = $1",
    )
    .bind(seed.merchant_account)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(balance, 0, "the merchant account moved");
}
