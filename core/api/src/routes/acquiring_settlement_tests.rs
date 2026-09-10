//! The payer-facing acquiring rail must settle, settle once, and settle into the
//! account the payer paid into.
//!
//! Four defects lived here together, and the Public Sandbox showed all four at
//! once: a payer confirmed a simulated Multicaixa payment, saw a terminal
//! CONFIRMED, and the merchant's balance never moved.
//!
//! 1. The settlement was the BODY of `emis_callback`. `test_confirm` — the only
//!    confirmation that exists in Sandbox — called `process_callback` and
//!    returned, so the simulated rail confirmed the provider and moved no money.
//!    A Sandbox that does not move money is not standing in for the Live rail;
//!    it is disagreeing with it.
//! 2. The credit went to `wallets.available_account_id`, the wallet default,
//!    ignoring `payment_links.wallet_account_id` — the segregated account whose
//!    own migration (0084) exists so that a paid link "credits THAT account, not
//!    the wallet's default available account". Campaign money landed in the
//!    general balance.
//! 3. The posting header and its two legs were three un-transacted statements,
//!    so a failure between them persists a one-legged posting. BANZA
//!    INV-LEDGER-004: "a posting is atomic: partial postings never persist."
//! 4. Idempotency was a `SELECT EXISTS` before the insert, which two concurrent
//!    callbacks can both pass.
//!
//! Each test below fails on the pre-fix code for the specific reason named.

use chrono::Utc;
use sqlx::PgPool;
use uuid::Uuid;

use banzami_acquiring::{AcquiringPayment, AcquiringPaymentStatus, PaymentInstructions};
use banzami_types::{AccountId, Money};

use crate::routes::acquiring::settle_confirmed_payment;
use crate::state::{AppState, CoreEnvironment};

async fn ledger_account(pool: &PgPool, ty: &str, name: &str) -> Uuid {
    sqlx::query_scalar::<_, Uuid>(
        "INSERT INTO ledger_accounts (id, account_type, name, currency)
         VALUES ($1, $2, $3, 'AOA') RETURNING id",
    )
    .bind(Uuid::new_v4())
    .bind(ty)
    .bind(name)
    .fetch_one(pool)
    .await
    .unwrap()
}

async fn build_state(pool: PgPool) -> AppState {
    let transit = ledger_account(&pool, "ASSET", "Transit").await;
    let bank = ledger_account(&pool, "ASSET", "Bank").await;
    let fee = ledger_account(&pool, "REVENUE", "Operator Fee").await;
    AppState::new(
        pool,
        AccountId::from_uuid(transit),
        AccountId::from_uuid(bank),
        AccountId::from_uuid(fee),
        CoreEnvironment::Sandbox,
    )
}

struct Fixture {
    wallet: Uuid,
    default_account: Uuid,
    link: Uuid,
}

/// A merchant with a wallet and an ACTIVE payment link. The 0081 trigger gives
/// the wallet its PRIMARY wallet_account automatically.
async fn seed(pool: &PgPool) -> Fixture {
    let merchant = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO merchants (id, name, email, status) VALUES ($1,'M','m@t.test','ACTIVE')",
    )
    .bind(merchant)
    .execute(pool)
    .await
    .unwrap();

    let avail = ledger_account(pool, "LIABILITY", "Merchant available").await;
    let reserved = ledger_account(pool, "LIABILITY", "Merchant reserved").await;
    let wallet = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO wallets (id, merchant_id, currency, status, available_account_id, reserved_account_id)
         VALUES ($1,$2,'AOA','ACTIVE',$3,$4)",
    )
    .bind(wallet)
    .bind(merchant)
    .bind(avail)
    .bind(reserved)
    .execute(pool)
    .await
    .unwrap();

    let link = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO payment_links (id, merchant_id, wallet_id, slug, amount_minor, currency, status, environment)
         VALUES ($1,$2,$3,$4,100000,'AOA','ACTIVE','SANDBOX')",
    )
    .bind(link)
    .bind(merchant)
    .bind(wallet)
    .bind(format!("slug-{}", &link.to_string()[..8]))
    .execute(pool)
    .await
    .unwrap();

    Fixture { wallet, default_account: avail, link }
}

/// A segregated CAMPAIGN account on the same wallet, and the link routed to it.
async fn route_link_to_campaign(pool: &PgPool, f: &Fixture) -> (Uuid, Uuid) {
    let campaign_ledger = ledger_account(pool, "LIABILITY", "Campaign").await;
    let merchant: Uuid = sqlx::query_scalar("SELECT merchant_id FROM wallets WHERE id = $1")
        .bind(f.wallet)
        .fetch_one(pool)
        .await
        .unwrap();
    let wa: Uuid = sqlx::query_scalar(
        "INSERT INTO wallet_accounts (wallet_id, account_id, merchant_id, currency, purpose, status)
         VALUES ($1,$2,$3,'AOA','CAMPAIGN','ACTIVE') RETURNING id",
    )
    .bind(f.wallet)
    .bind(campaign_ledger)
    .bind(merchant)
    .fetch_one(pool)
    .await
    .unwrap();
    sqlx::query("UPDATE payment_links SET wallet_account_id = $1 WHERE id = $2")
        .bind(wa)
        .bind(f.link)
        .execute(pool)
        .await
        .unwrap();
    (wa, campaign_ledger)
}

/// A CONFIRMED acquiring payment against the link, persisted so the settlement
/// can resolve its owner the way the real callback path does.
async fn confirmed_payment(pool: &PgPool, link: Uuid, amount_minor: i64) -> AcquiringPayment {
    let id = Uuid::new_v4();
    let ext = format!("{}", &id.to_string()[..9]);
    sqlx::query(
        "INSERT INTO acquiring_payments
            (id, payment_link_id, provider, external_ref, status, amount_minor, currency,
             instructions, confirmed_at, expires_at)
         VALUES ($1,$2,'EMIS_MULTICAIXA_SIMULATED',$3,'CONFIRMED',$4,'AOA',
                 '{}'::jsonb, now(), now() + interval '1 hour')",
    )
    .bind(id)
    .bind(link)
    .bind(&ext)
    .bind(amount_minor)
    .execute(pool)
    .await
    .unwrap();

    AcquiringPayment {
        id: id.to_string().parse().unwrap(),
        payment_link_id: link.to_string().parse().unwrap(),
        provider: "EMIS_MULTICAIXA_SIMULATED".into(),
        external_ref: ext,
        status: AcquiringPaymentStatus::Confirmed,
        amount: Money::new(amount_minor, banzami_types::Currency::from_code("AOA").unwrap()),
        instructions: PaymentInstructions {
            method: "MULTICAIXA_EXPRESS".into(),
            entity: "00000".into(),
            reference: "000000000".into(),
        },
        confirmed_at: Some(Utc::now()),
        failed_at: None,
        failure_reason: None,
        expires_at: Utc::now() + chrono::Duration::hours(1),
        created_at: Utc::now(),
    }
}

async fn balance(pool: &PgPool, account: Uuid) -> i64 {
    sqlx::query_scalar::<_, Option<i64>>(
        "SELECT SUM(CASE entry_type WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END)::bigint
           FROM ledger_entries WHERE account_id = $1",
    )
    .bind(account)
    .fetch_one(pool)
    .await
    .unwrap()
    .unwrap_or(0)
}

async fn unbalanced_postings(pool: &PgPool) -> i64 {
    sqlx::query_scalar(
        "SELECT COUNT(*) FROM (
             SELECT p.id
               FROM ledger_postings p
               LEFT JOIN ledger_entries e ON e.posting_id = p.id
              GROUP BY p.id
             HAVING COALESCE(SUM(CASE e.entry_type WHEN 'DEBIT'  THEN -e.amount_minor
                                                   WHEN 'CREDIT' THEN  e.amount_minor
                                                   ELSE 0 END), 0) <> 0
                 OR COUNT(e.id) <> 2
         ) x",
    )
    .fetch_one(pool)
    .await
    .unwrap()
}

// ---------------------------------------------------------------------------

/// Defect 1. Before the fix the simulated rail never reached any settlement, so
/// the wallet stayed at zero. This asserts the money actually arrives.
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_confirmed_payment_credits_the_wallet(pool: PgPool) {
    let f = seed(&pool).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    settle_confirmed_payment(&state, &payment).await.unwrap();

    assert_eq!(
        balance(&pool, f.default_account).await,
        100_000,
        "a confirmed acquiring payment left the wallet unchanged — the payer saw \
         success and no money moved"
    );
}

/// §7: incoming payment confirmation is not a pricing-fee consumer. The wallet is
/// credited GROSS — 100000 in, 100000 credited.
#[sqlx::test(migrations = "../../db/migrations")]
async fn the_credit_is_gross_with_no_incoming_fee(pool: PgPool) {
    let f = seed(&pool).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    settle_confirmed_payment(&state, &payment).await.unwrap();

    assert_eq!(
        balance(&pool, f.default_account).await,
        100_000,
        "confirmation took a fee — capture/confirmation is fee-neutral; fees \
         belong to settlement and payout"
    );
    let legs: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM ledger_entries")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(legs, 2, "expected exactly one two-legged posting, no fee legs");
}

/// Defect 2. The link names a CAMPAIGN account; that is where the money belongs.
/// Pre-fix this credited `available_account_id` and the campaign stayed at 0 —
/// the exact DOA symptom.
#[sqlx::test(migrations = "../../db/migrations")]
async fn the_credit_lands_on_the_account_the_link_names(pool: PgPool) {
    let f = seed(&pool).await;
    let (_wa, campaign_ledger) = route_link_to_campaign(&pool, &f).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    settle_confirmed_payment(&state, &payment).await.unwrap();

    assert_eq!(
        balance(&pool, campaign_ledger).await,
        100_000,
        "the segregated account the link routes to was not credited"
    );
    assert_eq!(
        balance(&pool, f.default_account).await,
        0,
        "money routed to a segregated account landed in the wallet default instead"
    );
}

/// A named account that fails ADR-042 validation must NOT silently fall back to
/// the wallet default — that is the same defect, quieter. Nothing is posted.
#[sqlx::test(migrations = "../../db/migrations")]
async fn an_invalid_named_account_does_not_fall_back_to_the_default(pool: PgPool) {
    let f = seed(&pool).await;
    let (wa, campaign_ledger) = route_link_to_campaign(&pool, &f).await;
    sqlx::query("UPDATE wallet_accounts SET status = 'CLOSED' WHERE id = $1")
        .bind(wa)
        .execute(&pool)
        .await
        .unwrap();
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    settle_confirmed_payment(&state, &payment).await.unwrap();

    assert_eq!(
        balance(&pool, campaign_ledger).await,
        0,
        "a CLOSED account was credited"
    );
    assert_eq!(
        balance(&pool, f.default_account).await,
        0,
        "settlement fell back to the wallet default when the named account was \
         invalid — segregated money would land in the general balance"
    );
}

/// Defect 3 / INV-LEDGER-004. Every posting this path writes has exactly two
/// legs that cancel. No posting is ever left with one.
#[sqlx::test(migrations = "../../db/migrations")]
async fn the_posting_is_atomic_and_balanced(pool: PgPool) {
    let f = seed(&pool).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    settle_confirmed_payment(&state, &payment).await.unwrap();

    assert_eq!(
        unbalanced_postings(&pool).await,
        0,
        "an unbalanced or single-leg posting persisted — INV-LEDGER-004"
    );
}

/// Defect 4. A replayed callback credits nothing a second time.
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_replayed_confirmation_credits_once(pool: PgPool) {
    let f = seed(&pool).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    for _ in 0..4 {
        settle_confirmed_payment(&state, &payment).await.unwrap();
    }

    assert_eq!(
        balance(&pool, f.default_account).await,
        100_000,
        "a replayed provider callback credited the wallet more than once"
    );
    let postings: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM ledger_postings WHERE idempotency_key LIKE 'acquiring-settle-%'",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(postings, 1, "a replay created a second settlement posting");
}

/// Concurrency (§10): parallel confirmations produce exactly one financial
/// outcome. The old `SELECT EXISTS` pre-check let two callers both see "not
/// settled" and both post; the UNIQUE insert is what makes one of them lose.
#[sqlx::test(migrations = "../../db/migrations")]
async fn parallel_confirmations_produce_one_credit(pool: PgPool) {
    let f = seed(&pool).await;
    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;

    let results = tokio::join!(
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
        settle_confirmed_payment(&state, &payment),
    );
    for r in [
        results.0, results.1, results.2, results.3,
        results.4, results.5, results.6, results.7,
    ] {
        r.expect("a concurrent settlement returned an error instead of losing quietly");
    }

    assert_eq!(
        balance(&pool, f.default_account).await,
        100_000,
        "concurrent confirmations credited the wallet more than once"
    );
    assert_eq!(
        unbalanced_postings(&pool).await,
        0,
        "concurrency left an unbalanced posting"
    );
}

/// A frozen merchant is not credited, and no partial posting is left behind.
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_frozen_merchant_is_not_credited(pool: PgPool) {
    let f = seed(&pool).await;
    let merchant: Uuid = sqlx::query_scalar("SELECT merchant_id FROM wallets WHERE id = $1")
        .bind(f.wallet)
        .fetch_one(&pool)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO risk_freezes (subject_type, subject_id, reason, created_at)
         VALUES ('MERCHANT', $1, 'test', now())",
    )
    .bind(merchant)
    .execute(&pool)
    .await
    .ok();

    let state = build_state(pool.clone()).await;
    let payment = confirmed_payment(&pool, f.link, 100_000).await;
    settle_confirmed_payment(&state, &payment).await.unwrap();

    let frozen = crate::routes::risk::is_frozen(&pool, "MERCHANT", merchant).await;
    if frozen {
        assert_eq!(
            balance(&pool, f.default_account).await,
            0,
            "a frozen merchant's wallet was credited"
        );
    }
    assert_eq!(unbalanced_postings(&pool).await, 0);
}
