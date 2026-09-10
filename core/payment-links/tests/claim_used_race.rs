//! One payment, one claim — however many callers arrive at once.
//!
//! `mark_used` read the link's status, checked it in Rust, and then updated
//! unconditionally. Six concurrent confirmations of a single payment therefore
//! all read ACTIVE, all passed the check, and all wrote. The gateway emits
//! `payment_link.paid` whenever mark_used returns Ok, so one payment produced
//! several events with DIFFERENT ids — an integrator deduplicating by event id
//! could not collapse them, and a donation platform would have credited the
//! donor once per event.
//!
//! Observed on the deployed Public Sandbox before this fix: six parallel payer
//! confirmations of one payment produced three distinct payment_link.paid
//! events. The ledger credit was already safe (the settlement posting is
//! idempotent on a UNIQUE key), so no money was duplicated — but "exactly one
//! financial effect" has to include the event that tells the world it happened.

use chrono::Utc;
use sqlx::PgPool;
use uuid::Uuid;

use banzami_payment_links::{
    PaymentLinkEngine, PaymentLinkError, PaymentLinkStatus, PostgresPaymentLinkEngine,
    PostgresPaymentLinkRepository,
};
use banzami_types::PaymentLinkId;

async fn seed_link(pool: &PgPool) -> Uuid {
    let merchant = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO merchants (id, name, email, status) VALUES ($1,'M','m@t.test','ACTIVE')",
    )
    .bind(merchant)
    .execute(pool)
    .await
    .unwrap();

    let mk = |ty: &'static str, name: &'static str| {
        let pool = pool.clone();
        async move {
            sqlx::query_scalar::<_, Uuid>(
                "INSERT INTO ledger_accounts (id, account_type, name, currency)
                 VALUES ($1,$2,$3,'AOA') RETURNING id",
            )
            .bind(Uuid::new_v4())
            .bind(ty)
            .bind(name)
            .fetch_one(&pool)
            .await
            .unwrap()
        }
    };
    let avail = mk("LIABILITY", "available").await;
    let reserved = mk("LIABILITY", "reserved").await;

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
    .bind(format!("s{}", &link.to_string()[..11]))
    .execute(pool)
    .await
    .unwrap();
    link
}

fn engine(pool: PgPool) -> PostgresPaymentLinkEngine<PostgresPaymentLinkRepository> {
    PostgresPaymentLinkEngine::new(PostgresPaymentLinkRepository::new(pool))
}

/// The defect, stated as the deployed Sandbox showed it.
#[sqlx::test(migrations = "../../db/migrations")]
async fn six_concurrent_confirmations_produce_one_claim(pool: PgPool) {
    let raw = seed_link(&pool).await;
    let id: PaymentLinkId = raw.to_string().parse().unwrap();
    let e = engine(pool.clone());

    let r = tokio::join!(
        e.mark_used(id),
        e.mark_used(id),
        e.mark_used(id),
        e.mark_used(id),
        e.mark_used(id),
        e.mark_used(id),
    );
    let results = [r.0, r.1, r.2, r.3, r.4, r.5];
    let winners = results.iter().filter(|x| x.is_ok()).count();

    assert_eq!(
        winners, 1,
        "{winners} of 6 concurrent confirmations claimed the same payment — each \
         Ok emits payment_link.paid, so one payment becomes {winners} events with \
         different ids and no integrator can deduplicate them"
    );

    // Everyone who lost must be told they lost, and told it precisely.
    for r in results.iter().filter(|x| x.is_err()) {
        match r.as_ref().unwrap_err() {
            PaymentLinkError::NotActive(_) => {}
            other => panic!("a losing caller got {other:?}, not NotActive — the reason a claim \
                             failed is what tells the caller whether to emit an event"),
        }
    }
}

/// Sequential replay: the second confirmation of a payment must also not claim.
#[sqlx::test(migrations = "../../db/migrations")]
async fn a_second_confirmation_does_not_claim_again(pool: PgPool) {
    let raw = seed_link(&pool).await;
    let id: PaymentLinkId = raw.to_string().parse().unwrap();
    let e = engine(pool.clone());

    assert!(e.mark_used(id).await.is_ok(), "the first claim must succeed");
    for _ in 0..3 {
        assert!(
            matches!(e.mark_used(id).await, Err(PaymentLinkError::NotActive(_))),
            "a replayed confirmation claimed the payment a second time"
        );
    }

    let (status, paid): (String, Option<chrono::DateTime<Utc>>) =
        sqlx::query_as("SELECT status, paid_at FROM payment_links WHERE id = $1")
            .bind(raw)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(status, "USED");
    assert!(paid.is_some(), "a claimed link must carry paid_at");
}

/// The winner gets the real row back — the caller builds the event payload from it.
#[sqlx::test(migrations = "../../db/migrations")]
async fn the_winner_receives_the_updated_link(pool: PgPool) {
    let raw = seed_link(&pool).await;
    let id: PaymentLinkId = raw.to_string().parse().unwrap();
    let link = engine(pool.clone()).mark_used(id).await.unwrap();
    assert!(matches!(link.status, PaymentLinkStatus::Used));
    assert!(link.paid_at.is_some(), "the returned link must carry paid_at");
}

/// An expired link is not claimable, and says so as Expired rather than NotActive.
#[sqlx::test(migrations = "../../db/migrations")]
async fn an_expired_link_cannot_be_claimed(pool: PgPool) {
    let raw = seed_link(&pool).await;
    sqlx::query("UPDATE payment_links SET expires_at = NOW() - interval '1 hour' WHERE id = $1")
        .bind(raw)
        .execute(&pool)
        .await
        .unwrap();
    let id: PaymentLinkId = raw.to_string().parse().unwrap();
    assert!(
        matches!(engine(pool.clone()).mark_used(id).await, Err(PaymentLinkError::Expired(_))),
        "an expired link was claimable, or reported the wrong reason"
    );
    let status: String = sqlx::query_scalar("SELECT status FROM payment_links WHERE id = $1")
        .bind(raw)
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(status, "ACTIVE", "a refused claim still wrote to the link");
}
