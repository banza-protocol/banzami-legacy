# Developer Platform — E2E Sandbox Results

Version: 1.0

Internal technical Sandbox only. Synthetic developers, workspaces, projects, platforms,
merchants, consumers, balances and API keys only. No LIVE, Production, real money,
external payment providers, real EMIS callback, customer data or public access.
Live-API calls were made over the internal-only network via `docker exec <service> curl`;
secrets read from mounted files into memory only and never printed. No secrets, tokens,
keys, IDs, hostnames, IPs or paths are recorded.

## What this pass proves

A synthetic platform/integrator can complete the full Developer Platform lifecycle in
the internal Sandbox: authenticate → use a workspace + project → mint/use an API key →
enforce scope → create a payment link and a payment intent from the platform context →
complete an online checkout → verify a receipt → reconcile → **revoke a key and be
rejected** → be rejected on invalid/unauthorised access — with **no ledger/balance
mutation** on any rejection and an **audit trail** recorded.

## Enablement

The developer/platform layer was already enabled and the `developer.*` schema migrated
(via the gated migration adapter) in prior work. This pass adds one Sandbox-only
internal fixture endpoint — `POST /internal/v1/fixture-keys/{keyID}/revoke` (hard
`ENVIRONMENT=sandbox` + internal-key gated, real-store unit-tested) — the revoke
counterpart needed to prove the revoked-key rejection path end-to-end. No product
feature beyond the fixture; nothing exposed publicly.

## Results (F0-DP-001..016 + UI)

| ID | Flow | Result |
|----|------|:------:|
| F0-DP-001 | Developer/platform auth (`/v1/me`) | PASS |
| F0-DP-002 | Workspace available | PASS |
| F0-DP-003 | Project available | PASS |
| F0-DP-004 | API key active accepted | PASS |
| F0-DP-005 | API key scope enforced (403 INSUFFICIENT_SCOPE) | PASS |
| F0-DP-006 | Payment link from platform context | PASS |
| F0-DP-007 | Payment intent from platform context | PASS |
| F0-DP-008 | Online checkout completes (+ ledger + idempotency) | PASS |
| F0-DP-009 | Receipt verification (state + privacy + non-fabricable) | PASS |
| F0-DP-010 | Platform reconciliation (zero discrepancy) | PASS |
| F0-DP-011 | Real signed webhook delivery to an independent public sink | PASS |
| F0-DP-012 | Revoked API key rejected (genuine revoke → 401) | PASS |
| F0-DP-013 | Invalid API key rejected (→401) | PASS |
| F0-DP-014 | Unauthorised platform rejected (→401) | PASS |
| F0-DP-015 | No ledger/balance mutation on rejected operations | PASS |
| F0-DP-016 | Audit/evidence record generated | PASS |
| F0-DP-UI  | Developer Console UI E2E | BLOCKED |

**Summary: PASS 16 · FAIL 0 · SIMULATED 0 · DEFERRED 0 · BLOCKED 1 · NOT_IN_SCOPE 0 · total 17.**
(Harness assertions incl. sub-checks: 24 PASS / 0 FAIL / 0 SIMULATED / 0 BLOCKED.)

## Proof detail

- **Successful payment operations** (F0-DP-008): status COMPLETED; ledger double-entry
  (merchant +50.000 / payer −50.000); balance movement confirmed; receipt reference
  generated; platform- and reconciliation-visible; idempotent retry did not double-charge.
- **Rejected operations** (F0-DP-012/013/014): deterministic 401/403; no completed
  transaction; **no balance mutation and no ledger mutation** (merchant + payer balances
  identical before/after all rejections, F0-DP-015); no receipt fabricated (forged
  reference → `exists=false`); audit/error evidence recorded.
- **API keys**: active key works (F0-DP-004); revoked key fails (F0-DP-012, genuine
  revoke); invalid key fails (F0-DP-013); wrong-scope key fails (F0-DP-005);
  unauthorised platform fails (F0-DP-014).
- **Webhooks** (F0-DP-011): PASS, and no longer simulated. The stated ground for the
  simulation was that a live outbound 2xx delivery needs a public HTTPS sink, which the
  no-external policy excluded. That sink now exists: `sandbox-webhook.banzami.com` is a
  genuinely public HTTPS receiver — the SSRF policy (RA-023) rightly refuses private
  targets and no exception was added for it — so the ground no longer holds and the
  assertion is made for real. A payer confirms a payment link, the deployed runtime
  delivers on its own schedule to a receiver it does not control, and the signature is
  checked by an implementation written from the published contract that imports nothing
  from `services/api-gateway/internal/webhook`. The same delivery is then re-checked
  under a wrong secret and rejected: a verifier that accepted everything would report a
  green delivery for a platform that signed nothing.
- **Audit trail** (F0-DP-016): `developer.audit_events` recorded project.created +
  fixture key creations + fixture key revocation for the synthetic project.

## Developer Console UI E2E — BLOCKED (reported honestly)

**BLOCKER — DEVELOPER CONSOLE UI E2E PATH MISSING.** No developer/console frontend
application exists in the repository (`apps/` contains admin, checkout, dashboard,
mobile, pay, validation-studio and website — none is the Developer Console). A browser/UI
E2E cannot be run and is **not** faked using API-only tests. The Developer Platform is
exercised at the API/SDK-contract level above.

## Non-claims

No LIVE, Production, real money, external payment provider, real EMIS callback, customer
data, public access, DNS/certificate/SMTP change, VM reset or infrastructure reset was
used. No production SDK is claimed — the harness is an SDK contract simulation. This is
not a claim of full Phase 0 / production completion or any BNA approval/admission.
