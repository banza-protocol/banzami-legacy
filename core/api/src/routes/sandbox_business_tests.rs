//! Sandbox Business readiness: complete enough to settle, and impossible in LIVE.
//!
//! Public Financial Setup created a merchant, a wallet and its PRIMARY account
//! and stopped. Application settlement names its parties by @banza, so a
//! Business with no `handle_registry` entry cannot be named — not as a
//! beneficiary and not as its own fee destination. Zero of the five Sandbox
//! owners the platform had provisioned had one, so no ordinary external
//! Developer Project could complete a settlement. ADR-028 additionally requires
//! a KYB-approved fee destination, which nothing on the public lifecycle could
//! produce.

use axum::{extract::State, Json};
use sqlx::PgPool;
use uuid::Uuid;

use banzami_types::AccountId;

use crate::routes::sandbox_business::{business_readiness, ReadinessBody};
use crate::state::{AppState, CoreEnvironment};

async fn state_for(pool: PgPool, env: CoreEnvironment) -> AppState {
    let acct = |name: &'static str| {
        let pool = pool.clone();
        async move {
            sqlx::query_scalar::<_, Uuid>(
                "INSERT INTO ledger_accounts (id, account_type, name, currency)
                 VALUES ($1,'ASSET',$2,'AOA') RETURNING id",
            )
            .bind(Uuid::new_v4())
            .bind(name)
            .fetch_one(&pool)
            .await
            .unwrap()
        }
    };
    let (t, b, f) = (acct("t").await, acct("b").await, acct("f").await);
    AppState::new(
        pool,
        AccountId::from_uuid(t),
        AccountId::from_uuid(b),
        AccountId::from_uuid(f),
        env,
    )
}

async fn merchant(pool: &PgPool, status: &str) -> Uuid {
    let id = Uuid::new_v4();
    sqlx::query("INSERT INTO merchants (id, name, email, status) VALUES ($1,'Sandbox · P',$2,$3)")
        .bind(id)
        .bind(format!("p-{}@projects.banzami.test", &id.to_string()[..8]))
        .bind(status)
        .execute(pool)
        .await
        .unwrap();
    id
}

/// A handle derived from a project id the way the caller derives one.
fn derived(seed: Uuid) -> String {
    format!("p{}", seed.to_string().replace('-', "")[..12].to_string())
}

#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_gives_a_business_a_handle_and_sandbox_kyb(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let h = derived(m);

    let (_, Json(res)) = business_readiness(
        State(state),
        Json(ReadinessBody { merchant_id: m.to_string(), handle: h.clone() }),
    )
    .await
    .expect("readiness should succeed in sandbox");

    assert_eq!(res.handle, h, "the derived handle was not registered");
    assert_eq!(res.kyb_status, "APPROVED");
    assert!(res.provisioned, "nothing was reported as provisioned");

    // The handle must be resolvable as a MERCHANT party — that is the whole
    // point, since settlement names its parties this way.
    let owner: (String, Option<Uuid>) =
        sqlx::query_as("SELECT owner_type, owner_id FROM handle_registry WHERE handle = $1")
            .bind(&h)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(owner.0, "MERCHANT");
    assert_eq!(owner.1, Some(m), "the handle points at a different owner");
}

/// Approving KYB is a compliance decision. A guard that lives only in the caller
/// is one deployment mistake from being absent.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_is_refused_in_live(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    let state = state_for(pool.clone(), CoreEnvironment::Live).await;

    let err = business_readiness(
        State(state),
        Json(ReadinessBody { merchant_id: m.to_string(), handle: derived(m) }),
    )
    .await
    .err()
    .expect("LIVE must refuse");
    let _ = err;

    let handles: i64 = sqlx::query_scalar("SELECT count(*) FROM handle_registry WHERE owner_id = $1")
        .bind(m)
        .fetch_one(&pool)
        .await
        .unwrap();
    let kyb: i64 = sqlx::query_scalar("SELECT count(*) FROM merchant_compliance WHERE merchant_id = $1")
        .bind(m)
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(handles, 0, "LIVE registered a handle");
    assert_eq!(kyb, 0, "LIVE wrote a compliance record — KYB was approved outside Sandbox");
}

/// Resumable: Financial Setup retries, and a retry must not move identity.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_is_idempotent(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let h = derived(m);

    let first = business_readiness(
        State(state.clone()),
        Json(ReadinessBody { merchant_id: m.to_string(), handle: h.clone() }),
    )
    .await
    .unwrap()
    .1;
    for _ in 0..3 {
        let again = business_readiness(
            State(state.clone()),
            Json(ReadinessBody { merchant_id: m.to_string(), handle: h.clone() }),
        )
        .await
        .unwrap()
        .1;
        assert_eq!(again.handle, first.handle, "a retry changed the handle");
        assert!(!again.provisioned, "a retry claimed to have provisioned again");
    }
    let n: i64 = sqlx::query_scalar("SELECT count(*) FROM handle_registry WHERE owner_id = $1")
        .bind(m)
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(n, 1, "a retry registered a second handle");
}

/// An operator's real decision outranks provisioning. A retry must not launder a
/// REJECTED business into APPROVED.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_never_overwrites_an_existing_compliance_decision(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    sqlx::query(
        "INSERT INTO merchant_compliance (merchant_id, kyb_status, aml_status, created_at, updated_at)
         VALUES ($1,'REJECTED','REJECTED',now(),now())",
    )
    .bind(m)
    .execute(&pool)
    .await
    .unwrap();

    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let res = business_readiness(
        State(state),
        Json(ReadinessBody { merchant_id: m.to_string(), handle: derived(m) }),
    )
    .await
    .unwrap()
    .1;

    assert_eq!(
        res.kyb_status, "REJECTED",
        "provisioning reported APPROVED over an operator's REJECTED decision"
    );
    let stored: String =
        sqlx::query_scalar("SELECT kyb_status FROM merchant_compliance WHERE merchant_id = $1")
            .bind(m)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(stored, "REJECTED", "an operator's decision was overwritten");
}

/// A handle another owner already holds is never taken from them.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_never_steals_a_registered_handle(pool: PgPool) {
    let incumbent = merchant(&pool, "ACTIVE").await;
    let newcomer = merchant(&pool, "ACTIVE").await;
    let h = derived(incumbent);
    sqlx::query("INSERT INTO handle_registry (handle, owner_type, owner_id) VALUES ($1,'MERCHANT',$2)")
        .bind(&h)
        .bind(incumbent)
        .execute(&pool)
        .await
        .unwrap();

    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let out = business_readiness(
        State(state),
        Json(ReadinessBody { merchant_id: newcomer.to_string(), handle: h.clone() }),
    )
    .await;
    assert!(out.is_err(), "a handle registered to another owner was reassigned");

    let owner: Option<Uuid> =
        sqlx::query_scalar("SELECT owner_id FROM handle_registry WHERE handle = $1")
            .bind(&h)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(owner, Some(incumbent), "the incumbent lost its handle");
}

/// It completes a Business; it never conjures one.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_refuses_an_unknown_or_inactive_merchant(pool: PgPool) {
    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let ghost = Uuid::new_v4();
    assert!(
        business_readiness(
            State(state.clone()),
            Json(ReadinessBody { merchant_id: ghost.to_string(), handle: derived(ghost) })
        )
        .await
        .is_err(),
        "an unknown merchant was given an identity"
    );

    let suspended = merchant(&pool, "SUSPENDED").await;
    assert!(
        business_readiness(
            State(state),
            Json(ReadinessBody { merchant_id: suspended.to_string(), handle: derived(suspended) })
        )
        .await
        .is_err(),
        "a suspended business was made settlement-ready"
    );
}

// ADR-028 has TWO conditions for an application-fee destination: KYB approved
// AND an APPLICATION/PLATFORM account type. Readiness closed the first and left
// the second at the create-time default, so all nine self-service Businesses
// carried `MERCHANT` and every one of them failed its own fee-destination check
// with FEE_DESTINATION_TYPE_NOT_ALLOWED. A Developer Project's Business exists
// to route value on behalf of an application; that is what APPLICATION means.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_declares_the_application_fee_taxonomy(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    let before: Option<String> =
        sqlx::query_scalar("SELECT business_account_type FROM merchants WHERE id = $1")
            .bind(m)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(
        before.as_deref(),
        Some("MERCHANT"),
        "precondition: the create-time default is the one that cannot take a fee"
    );

    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let (_, Json(res)) = business_readiness(
        State(state),
        Json(ReadinessBody {
            merchant_id: m.to_string(),
            handle: derived(m),
        }),
    )
    .await
    .unwrap();

    assert_eq!(res.business_account_type, "APPLICATION");
    let after: Option<String> =
        sqlx::query_scalar("SELECT business_account_type FROM merchants WHERE id = $1")
            .bind(m)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(after.as_deref(), Some("APPLICATION"));
}

// An operator's deliberate choice outranks a provisioning retry, exactly as an
// operator's KYB decision does. Promotion happens only FROM the default, which
// is the state that means nobody chose.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_does_not_overrule_a_deliberate_taxonomy(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    sqlx::query("UPDATE merchants SET business_account_type = 'PLATFORM' WHERE id = $1")
        .bind(m)
        .execute(&pool)
        .await
        .unwrap();

    let state = state_for(pool.clone(), CoreEnvironment::Sandbox).await;
    let (_, Json(res)) = business_readiness(
        State(state),
        Json(ReadinessBody {
            merchant_id: m.to_string(),
            handle: derived(m),
        }),
    )
    .await
    .unwrap();

    assert_eq!(
        res.business_account_type, "PLATFORM",
        "a type an operator set must survive a provisioning retry"
    );
}

// Idempotent, like the rest of readiness: a second call changes nothing.
#[sqlx::test(migrations = "../../db/migrations")]
async fn readiness_taxonomy_is_idempotent(pool: PgPool) {
    let m = merchant(&pool, "ACTIVE").await;
    let state = || state_for(pool.clone(), CoreEnvironment::Sandbox);
    let body = || ReadinessBody {
        merchant_id: m.to_string(),
        handle: derived(m),
    };

    let (_, Json(first)) = business_readiness(State(state().await), Json(body()))
        .await
        .unwrap();
    let (_, Json(second)) = business_readiness(State(state().await), Json(body()))
        .await
        .unwrap();

    assert_eq!(first.business_account_type, "APPLICATION");
    assert_eq!(second.business_account_type, "APPLICATION");
    assert!(first.provisioned, "the first call did the work");
    assert!(!second.provisioned, "the second found nothing left to do");
}
