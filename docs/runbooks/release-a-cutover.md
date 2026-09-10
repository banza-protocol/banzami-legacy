# Release A cutover — order of operations

**Version: 1.0**

Release A carries two data operations alongside the code deploy, and the order
they run in is not a preference. Two of the steps close windows that, once
crossed, cannot be reopened.

## What is being cut over

| | |
|---|---|
| Code | nine writers now name `environment`; the fee rate comes from the operator's pricing profile; Sandbox readiness declares the ADR-028 taxonomy; the gateway gains an operator proof signing key |
| Data | migration 0113 relabels 272 rows that claimed to be LIVE |
| Data | the historical proof backfill materialises ~80 proofs for receipts already in people's hands |

## The two windows

**Window 1 — a burnt public reference.** The deployed gateway already mints a
proof on every receipt request. A record whose PDF prints `BZM-XXXX-XXXX` gets a
*fresh* SECURE_V1 reference if anyone asks for its receipt before the backfill
runs, because `Ensure` is keyed on `(transaction_id, environment)` and the first
caller wins. The printed reference is then dead permanently: the proof exists,
under a different name, and nothing can rename it — a public reference is not
something to rewrite. This window has been open since the last deploy. It closes
the moment the backfill claims the reference.

**Window 2 — a row relabelled and then re-mislabelled.** If 0113 runs before the
new binaries, any row written between the migration and the deploy inherits the
`DEFAULT 'LIVE'` again and the repair is silently incomplete. The migration must
run after the writers that stopped depending on the default.

The two windows point in opposite directions — one wants the backfill early, the
other wants the migration late — which is why the order below is what it is.

## Order

```
1. build           ./deploy.sh --all --build-only
2. mint the key    ensure the proof signing key exists in the secret dir
3. backfill        backfill-proofs (dry run, then -apply)   ← closes window 1
4. deploy          ./deploy.sh --all
5. migrate         0113 via sqlx, then the drift detector   ← closes window 2
6. readiness       re-run sandbox business readiness for bound projects
7. prove           historical proof E2E, fresh SECURE_V1 E2E, environment audit
```

### 2 — mint the key before anything signs with it

The backfill and the gateway must sign under the **same** key, so the key has to
exist before either runs. `sandbox-deploy.sh` mints it on first deploy and keeps
it forever after; minting it in step 2 simply moves that forward so the backfill
in step 3 can use it. Do not rotate it afterwards: a proof is an immutable
public record, and a new key invalidates the operator's signature on every
receipt already issued.

### 3 — the backfill runs BEFORE the migration, deliberately

It does not read the source row's `environment` and does not filter on it. Those
rows are exactly the ones 0113 repairs, so reading the universe off a row known
to be mislabelled would be circular — and it would force the backfill to run
after the migration, which is what leaves window 1 open. The environment comes
from `platform_mode`, which is the authority.

Run without `-apply` first. It writes nothing and reports what it would do; the
count should be the number of transfers plus wallet payments with no proof.

### 5 — the migration is the ONE sanctioned path

`sqlx migrate run`, never `psql`. Applying migration SQL directly leaves
`_sqlx_migrations` untouched, which is what produced the 0090–0095 drift. Follow
with the manifest drift detector and treat a non-zero exit as a hard stop.

0113 refuses on its own if `platform_mode` is not SANDBOX, or if any LIVE api
key or LIVE merchant application exists. Those refusals are the migration
working; they are not something to force past.

### 6 — readiness, not a sweep

All nine self-service Sandbox Businesses carry the create-time `MERCHANT` type
and therefore fail their own ADR-028 fee-destination check. Readiness now
declares `APPLICATION`, and it is idempotent, so re-running it for each bound
project corrects them through the lifecycle rather than through a one-off script
nobody maintains. It promotes only from the default: a type an operator set
deliberately survives.

## What must be true afterwards

- `BZM-F993-38E2` resolves to its own transfer, verified, with the amount and
  both handles the PDF prints.
- A newly issued receipt carries a 24-symbol SECURE_V1 reference that resolves.
- No row in `payment_links`, `qr_codes`, `transfers`, `payouts`,
  `webhook_endpoints` or `transactions` says `LIVE`.
- A freshly created record still says `SANDBOX` after the deploy — proving the
  writers, not just the repair.
- Every bound project's Business reports `type_allowed: true` for its own
  fee destination.

## Rollback

Steps 1, 4 and 6 are reversible: `sandbox-deploy.sh deploy-one <svc> <tag>
--rollback` restores the previous image, and readiness is idempotent and
additive.

Steps 3 and 5 are not, and neither should be undone:

- The backfill only ever adds proofs that should already have existed. Deleting
  one would return a receipt to unverifiable, which is the defect.
- 0113 relabels Sandbox rows as Sandbox. Reverting it would restore an assertion
  that Sandbox money is real.

A failure in either leaves the system in the state it was in before — 0113 is a
single transaction, and the backfill is idempotent per record and reports what
it could not do rather than continuing past it.
