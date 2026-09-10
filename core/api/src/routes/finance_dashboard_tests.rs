//! Real-DB tests for the finance dashboard aggregations (ADR-021).

#![allow(clippy::inconsistent_digit_grouping)]

use axum::{
    extract::{Query, State},
    Json,
};
use sqlx::PgPool;
use uuid::Uuid;

use banzami_types::AccountId;

use crate::routes::finance_dashboard::{self, DashQuery};
use crate::state::{AppState, CoreEnvironment};

async fn ledger_account(pool: &PgPool, ty: &str) -> Uuid {
    sqlx::query_scalar::<_, Uuid>(
        "INSERT INTO ledger_accounts (id, account_type, name, currency)
         VALUES ($1, $2, 'x', 'AOA') RETURNING id",
    )
    .bind(Uuid::new_v4())
    .bind(ty)
    .fetch_one(pool)
    .await
    .unwrap()
}

async fn build_state(pool: PgPool) -> AppState {
    let transit = ledger_account(&pool, "ASSET").await;
    let bank = ledger_account(&pool, "ASSET").await;
    let opfee = ledger_account(&pool, "REVENUE").await;
    AppState::new(
        pool,
        AccountId::from_uuid(transit),
        AccountId::from_uuid(bank),
        AccountId::from_uuid(opfee),
        CoreEnvironment::Sandbox,
    )
}

/// Seed an operator_fee row (with its wallet + transaction chain) for a currency
/// / category / fee amount.
async fn seed_fee(pool: &PgPool, currency: &str, category: &str, gross: i64, fee: i64, env: &str) {
    let avail = ledger_account(pool, "LIABILITY").await;
    let reserved = ledger_account(pool, "LIABILITY").await;
    let wallet = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO wallets (id, merchant_id, currency, available_account_id, reserved_account_id)
         VALUES ($1, $2, $3, $4, $5)",
    )
    .bind(wallet)
    .bind(Uuid::new_v4())
    .bind(currency)
    .bind(avail)
    .bind(reserved)
    .execute(pool)
    .await
    .unwrap();

    let tx = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO transactions (id, idempotency_key, transaction_type, status, amount_minor,
            fee_minor, currency, merchant_id, wallet_id, environment)
         VALUES ($1, $2, 'PAYMENT', 'CAPTURED', $3, $4, $5, $6, $7, $8)",
    )
    .bind(tx)
    .bind(Uuid::new_v4().to_string())
    .bind(gross)
    .bind(fee)
    .bind(currency)
    .bind(Uuid::new_v4())
    .bind(wallet)
    // The transaction now says which universe it is in, like the operator_fee
    // below always did. The two used to disagree by omission: the fee row named
    // the environment and the transaction it points at inherited 'LIVE'.
    .bind(env)
    .execute(pool)
    .await
    .unwrap();

    sqlx::query(
        "INSERT INTO operator_fees (id, transaction_id, posting_id, amount_minor, currency,
            business_category, engine_version, snapshot_json, environment, idempotency_key)
         VALUES ($1, $2, $3, $4, $5, $6, 1, '{}'::jsonb, $7, $8)",
    )
    .bind(Uuid::new_v4())
    .bind(tx)
    .bind(Uuid::new_v4())
    .bind(fee)
    .bind(currency)
    .bind(category)
    .bind(env)
    .bind(format!("of-{}", Uuid::new_v4()))
    .execute(pool)
    .await
    .unwrap();
}

async fn seed_settlement(pool: &PgPool, status: &str, currency: &str, net: i64, env: &str) {
    sqlx::query(
        "INSERT INTO app_settlements (id, owner_ref, source_account_id, beneficiary_account_id,
            gross_amount_minor, application_fee_minor, net_amount_minor, currency, engine_version,
            pricing_snapshot_json, status, environment, idempotency_key)
         VALUES ($1, 'c', $2, $3, $4, 0, $4, $5, 1, '{}'::jsonb, $6, $7, $8)",
    )
    .bind(Uuid::new_v4())
    .bind(Uuid::new_v4())
    .bind(Uuid::new_v4())
    .bind(net)
    .bind(currency)
    .bind(status)
    .bind(env)
    .bind(format!("as-{}", Uuid::new_v4()))
    .execute(pool)
    .await
    .unwrap();
}

fn q() -> DashQuery {
    DashQuery {
        environment: None,
        currency: None,
        from: None,
        to: None,
    }
}

#[sqlx::test(migrations = "../../db/migrations")]
async fn empty_aggregations(pool: PgPool) -> sqlx::Result<()> {
    let state = build_state(pool).await;
    let Json(v) = finance_dashboard::get(State(state), Query(q()))
        .await
        .unwrap();
    assert_eq!(v["operator_fees"]["today"].as_array().unwrap().len(), 0);
    assert_eq!(
        v["operator_fees"]["by_currency"].as_array().unwrap().len(),
        0
    );
    assert_eq!(v["application_settlements"]["pending_count"], 0);
    assert_eq!(v["application_settlements"]["failed_count"], 0);
    Ok(())
}

#[sqlx::test(migrations = "../../db/migrations")]
async fn aggregates_fees_and_settlements(pool: PgPool) -> sqlx::Result<()> {
    seed_fee(&pool, "AOA", "DONATION", 5_000_00, 100_00, "SANDBOX").await;
    seed_fee(&pool, "AOA", "MARKETPLACE", 10_000_00, 200_00, "SANDBOX").await;
    seed_settlement(&pool, "COMPLETED", "AOA", 93_100, "SANDBOX").await;
    seed_settlement(&pool, "FAILED", "AOA", 1_000, "SANDBOX").await;
    seed_settlement(&pool, "CREATED", "AOA", 50_000, "SANDBOX").await;

    let state = build_state(pool).await;
    let Json(v) = finance_dashboard::get(State(state), Query(q()))
        .await
        .unwrap();

    // operator fees: AOA total 300_00 across 2 fees
    let by_cur = v["operator_fees"]["by_currency"].as_array().unwrap();
    assert_eq!(by_cur.len(), 1);
    assert_eq!(by_cur[0]["key"], "AOA");
    assert_eq!(by_cur[0]["count"], 2);
    assert_eq!(by_cur[0]["total_minor"], 300_00);

    let by_cat = v["operator_fees"]["by_business_category"]
        .as_array()
        .unwrap();
    assert_eq!(by_cat.len(), 2);

    // today KPI present (rows seeded now)
    assert_eq!(
        v["operator_fees"]["today"].as_array().unwrap()[0]["total_minor"],
        300_00
    );

    // settlements
    assert_eq!(v["application_settlements"]["failed_count"], 1);
    assert_eq!(v["application_settlements"]["pending_count"], 1); // CREATED counts as pending
    let by_status = v["application_settlements"]["by_status"]
        .as_array()
        .unwrap();
    assert_eq!(by_status.len(), 3);
    Ok(())
}

#[sqlx::test(migrations = "../../db/migrations")]
async fn filters_by_currency_and_environment(pool: PgPool) -> sqlx::Result<()> {
    seed_fee(&pool, "AOA", "DONATION", 5_000_00, 100_00, "SANDBOX").await;
    seed_fee(&pool, "USD", "DONATION", 5_000_00, 50_00, "SANDBOX").await;
    seed_fee(&pool, "AOA", "DONATION", 5_000_00, 70_00, "LIVE").await;

    let state = build_state(pool.clone()).await;

    // currency filter -> only AOA rows
    let aoa = DashQuery {
        currency: Some("AOA".into()),
        ..q()
    };
    let Json(v) = finance_dashboard::get(State(state.clone()), Query(aoa))
        .await
        .unwrap();
    let by_cur = v["operator_fees"]["by_currency"].as_array().unwrap();
    assert_eq!(by_cur.len(), 1);
    assert_eq!(by_cur[0]["key"], "AOA");
    assert_eq!(by_cur[0]["count"], 2); // SANDBOX + LIVE AOA

    // environment filter -> only LIVE
    let live = DashQuery {
        environment: Some("LIVE".into()),
        ..q()
    };
    let Json(v2) = finance_dashboard::get(State(state), Query(live))
        .await
        .unwrap();
    let cur2 = v2["operator_fees"]["by_currency"].as_array().unwrap();
    assert_eq!(cur2.len(), 1);
    assert_eq!(cur2[0]["total_minor"], 70_00);
    Ok(())
}
