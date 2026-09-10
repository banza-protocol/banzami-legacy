# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.11.0] — 2026-09-10

### Removed — `applicationFeeBps` on `createBusinessApplicationSettlement`

**Breaking.** The field is gone from `CreateBusinessApplicationSettlementParams`
and the SDK no longer sends `application_fee_bps`, even if a JavaScript caller
passes it.

It used to work, which was the problem. A non-zero value made the operator skip
its own pricing entirely and charge whatever the caller asked for, up to 50%. A
caller cannot set the price of the service it is buying, so the rate now comes
from the pricing profile Banzami assigned to your business, and the operator
ignores the field on the wire.

To upgrade: delete the `applicationFeeBps` line. Nothing else changes. Keep
naming `feeDestinationBanzaName` whenever your business can receive a fee —
you no longer know from your own request whether one will be charged, so do not
gate the destination on a rate you used to send.

### Added — `project_id` on `me()`

`me()` returns `project_id` (`proj_…`) beside `project`. The slug is what you
typed and changes when you rename the project; `project_id` does not. File your
own records under it. It is derived, not the internal UUID.

## [0.10.0] — 2026-09-08

### Fixed — the ESM half of the dual build shipped undeclared

The package emits ESM into `dist/` and CommonJS into `dist/cjs/`, and `exports`
points at both. Nothing said which was which: the root `package.json` has no
`type`, so every `.js` in the package is CommonJS by default — including
`dist/index.js`, which is pure ESM syntax.

Node ≥22 covers that up by sniffing the file, so it worked on a laptop and in
CI. It did not work in the first place an external developer put it: a
serverless webhook receiver, where the import resolved as CommonJS and

```js
import { constructEvent } from '@banzami/sdk';
```

failed with `Named export 'constructEvent' not found` — the exact line the
webhook documentation tells them to write.

`npm run build` now emits `dist/package.json` (`module`) and
`dist/cjs/package.json` (`commonjs`). `tools/check-sdk-dual-package.mjs` packs
the tarball, installs it, and loads it both ways with syntax detection off, so
the guess is no longer what holds it up.

### Removed — `createApplicationSettlement` no longer accepts pricing selectors (breaking)

`feePolicyRef`, `businessCategory` and `pricingProfile` are gone from
`CreateApplicationSettlementParams` and are no longer sent on the wire.

0.9.0 removed the same three from `createTransaction` and left them here,
describing this method as remaining "for backwards compatibility". There is no
backwards compatibility to preserve: nothing has officially launched, and the
three fields were rule-matching dimensions in the operator's pricing engine.
A caller that supplied them was choosing between tariffs without ever sending an
amount.

The operator resolves the fee from the pricing profile it has assigned the
authenticated owner. There is no request field that influences it, and a test
asserts that passing the old names anyway — as an untyped JavaScript caller
would — leaves them off the wire.

## [0.9.0] — 2026-09-07

### Removed — `createTransaction` no longer accepts pricing selectors (breaking)

`businessCategory`, `pricingProfile` and `feePolicyRef` are gone, along with the
exported `BusinessCategory` and `PricingProfile` types.

They were documented as "reference only: the SDK never sends or receives a fee
or percentage; the operator resolves any fee internally". That was accurate
about the wire and wrong about the consequence. Each of the three is a
rule-matching dimension in the operator's pricing engine, and rule selection
ranks by specificity — the more dimensions a rule pins, the higher it wins. So
choosing the reference chose the rate, one level of indirection away from
naming a price.

The same documentation then promised that "omitting these keeps the legacy
zero-fee behaviour", which made sending nothing the cheapest thing a caller
could do. That is now a refusal on the operator side rather than a discount: a
transaction with no applicable pricing rule does not capture at all, and an
explicit 0 bps rule is how zero gets said.

Pricing is resolved by the operator from the merchant's assigned profile. There
is nothing for a client to pass.

**Upgrade:** delete the three arguments from any `createTransaction` call. The
gateway already ignores them, so behaviour does not change when you do —
removal is a compile-time correction, not a runtime one. If you imported
`BusinessCategory` or `PricingProfile`, remove those imports; no SDK method
takes either type any more.

## [0.8.3] — 2026-09-06

### Changed — refunds moved to the canonical public path `/v1/refunds`

`/business/` described where the refund handler happened to be mounted back
when a refund could only be asked for with a merchant credential. It was never
a word in the developer-facing vocabulary, and an external developer reading
the reference had no way to know why this one financial verb sat behind a noun
naming nothing they own.

0.8.1 moved the *client* to `/v1/refunds` so it would match the server.
The server has now moved instead, and this release follows it back:
`createRefund`, `getRefund` and `listRefunds` call `/v1/refunds`. The method
names, parameters and return types are unchanged — only the URL the SDK sends.

**Upgrade:** if you are on 0.8.1 or 0.8.2, refunds now 404 against the deployed
Sandbox, because the retired path is unmounted rather than redirected. Upgrade
to 0.8.3; no code of yours changes.

## [0.8.2] — 2026-09-05

### Fixed — the documented `@banzami/sdk/webhooks` import did not resolve

The webhooks module's own example teaches
`import { constructEvent } from '@banzami/sdk/webhooks'`, but `exports` declared
no `./webhooks` subpath, so under `NodeNext` resolution that import failed with
**TS2307 Cannot find module**. The names were reachable from the package root
all along, which is why it went unnoticed — the documented path was the broken
one. `./webhooks` is now exported (ESM, CJS and types); the root export is
unchanged.

## [0.8.1] — 2026-09-05

### Fixed — refunds were unreachable with the key this SDK documents

`createRefund`, `getRefund` and `listRefunds` pointed at `/v1/refunds`, which is
mounted under merchant-JWT authentication. The only credential this SDK's README
documents is a Developer Platform project key (`bz_test_sk_…`), so every refund
call answered **401 INVALID_TOKEN** — the refunds capability was advertised as
available and could not be used. DOA's production refund path was calling it.

They now target `/v1/refunds`, the project-scoped route, which resolves
the financial owner from the key's binding and answers 404 for another project's
payment. No signature changed; no caller needs to change anything but the version.

`getBusinessMe()` was unreachable for the same reason and is fixed operator-side:
`/v1/integration` now accepts a project key (the route was built for an
integrating application's Integration Health view, and that application holds a
project key).

### Added

- `route-drift` now checks **credential reachability**, not only that a path
  exists. A mounted route under the wrong auth group is the defect this release
  fixes, and path-existence alone could never have caught it.

## [0.8.0] — 2026-09-05

### Added

- `createTransfer` — move value between two wallet accounts of the project's own
  bound owner (BANZA ADR-052). `POST /v1/transfers`.
- Webhook endpoint management scoped to the project.

## [0.6.0] — 2026-09-04

### Added — segregated accounts for Developer Platform credentials
- **Wallet accounts are usable with a Developer Platform key.** An application
  can open and address its own segregated accounts — one per campaign, tenant
  or purpose — beneath the owner its project binding established.

  `CreateWalletAccountParams.walletId` and `listWalletAccounts(walletId?)` are
  now optional, on the same credential split as `walletAccountId`:

  - **Developer Platform key** — omit the wallet. It is derived from your
    project's binding, and the API rejects a client-supplied wallet outright,
    even one that is your own: naming the wallet is naming the owner.
  - **Merchant credential** — supply it; ownership is checked server-side.

  This is additive. Existing merchant integrations that pass `walletId` are
  unchanged, and 0.5.1's payment-session contract is untouched.

  Compile-time contract tests cover both credential models for wallet accounts
  and payment sessions (`src/payment-session-contract.test-d.ts`).

## [0.5.1] — 2026-09-04

### Fixed — the documented Quickstart did not type-check
- **`CreatePaymentSessionParams.walletAccountId` is now optional.** It was
  required, which made the public developer Quickstart impossible to compile:
  a Developer Platform key must NOT send a payee — the server derives it from
  the project's Banzami binding and rejects a client-supplied one with 400 —
  yet the type demanded one. A TypeScript developer following the docs
  literally had to choose between a cast and a guaranteed 400.

  Which credential you hold decides whether the field is sent:

  - **Developer Platform key** — omit it; the payee comes from the binding.
  - **Merchant credential** — supply it; the server requires it on that route
    and re-validates ownership.

  Runtime behaviour is unchanged: an omitted field was already absent from the
  request body. Nothing about server-side validation is relaxed, and the SDK
  still never infers, derives or substitutes a payee.

  Compile-time regression tests cover both credential models
  (`src/payment-session-contract.test-d.ts`).

## [0.5.0] — 2026-09-04

### Fixed — the documented path did not work
- **A Developer Console key is now used directly as the bearer credential.**
  The client previously sent every key to the merchant exchange endpoint
  `POST /v1/auth/token`. Console-issued keys (`bz_test_sk_…`) are not
  exchangeable — the Gateway introspects them per request — so the very first
  call an external developer makes, following the documented Quickstart with a
  key they had just created, failed with `BanzamiAuthError: Invalid or
  unauthorized Banzami API key` on a key that was perfectly valid.

  The two credential shapes are told apart by the `sk_`/`pk_` segment, which
  only Console keys carry; a merchant key has hex immediately after the
  environment prefix. Merchant API keys continue to be exchanged for a JWT
  exactly as before, so existing integrations are unaffected.

### Added
- `isDeveloperPlatformKey(apiKey)` — classifies which credential model a key
  belongs to.

## [0.4.0] — 2026-09-04

First release published to the public npm registry. Versions 0.1.0–0.3.0 were
internal/vendored only and were never published; 0.4.0 is therefore the first
version installable as `npm install @banzami/sdk`.

### Added
- Published to npm as **`@banzami/sdk`** (public). External applications no
  longer vendor a copy of this package to integrate with Banzami.
- MIT licence for this package's client code (`sdk/typescript/LICENSE`). The
  grant covers this package only — not the Banzami service, API, platform, the
  BANZA protocol, or Banzami trademarks. See the README "Licence" section.

### Removed — BREAKING (security)
- `sendTransfer(...)`, `getTransfer(id)` and `listTransfers(...)`. These called an
  id-based **merchant** transfer surface that has been retired. A
  consumer-to-consumer P2P transfer has two consumer participants and no merchant
  party, so a merchant credential held no authority over it — and the routes took
  their subject straight from client input. A merchant key could therefore name
  any `sender_id` and move that consumer's money, read any transfer by id, or
  list any consumer's entire history (security audit SEC-015 / SEC-018; the
  authorization gap was first recorded in
  `docs/security/2026-07-03-transfer-surface-findings.md`, finding B, as a hard
  blocker before Live activation).

  **Migration:** there is deliberately no merchant-facing replacement, and no
  `merchant_id` was added to the transfer model to manufacture one. Consumer P2P
  transfers belong to the consumer surface (public-api), where the sender is
  derived from the authenticated consumer token and a read is allowed only to the
  transfer's own sender or recipient. Integrations that need a consumer to move
  their own money should use the consumer API with that consumer's credential.

- `suspendConsumer(id)` and `closeConsumer(id)`. Suspending or closing a consumer
  account is an **operator** action; publishing it in the merchant SDK exposed a
  capability the merchant surface could not authorise. The routes it called
  (`POST /v1/consumers/{id}/suspend` and `/close`) performed no ownership check
  at all, so any authenticated caller could suspend or close **any** consumer
  account by id — security audit finding SEC-003. Both routes were removed from
  the merchant surface and a regression test now asserts they stay off it.

  **Migration:** there is deliberately no merchant-facing replacement. The
  capability lives on the operator surface, where it can be authorised and
  audited: `POST /admin/v1/consumers/{id}/suspend` in admin-api, gated by the
  `consumer.suspend` capability and recorded as a `SUSPEND_CONSUMER` audit event.
  It is reached through the operator console, not through this SDK.

## [0.2.0] — 2026-06-30

### Added
- Segregated wallet accounts (BANZA ADR-020): `createWalletAccount`, `listWalletAccounts`, `getWalletAccount` — bind funds to an app reference (e.g. a campaign) under one merchant wallet via `POST/GET /v1/wallet-accounts`
- App-defined application settlement (BANZA ADR-019): `createBusinessApplicationSettlement` — the app names the source segregated account, the beneficiary `@banza`, an optional fee destination `@banza`, and its OWN `applicationFeeBps`; the operator reads the real balance as the gross, resolves the `@names`, splits (fee → app, net → beneficiary) and audits it. The app sends no amount and never computes the final fee. Idempotent on `idempotencyKey`
- `resolveHandle(handle)` — resolve a `@banza` handle (backed by `GET /v1/consumers/handle/{handle}`) to pre-validate a beneficiary before settlement
- Types: `WalletAccount`, `CreateWalletAccountParams`, `CreateBusinessApplicationSettlementParams`, `WalletAccountPurpose`

- Payment Sessions (BANZA ADR-015): `createPaymentSession`, `getPaymentSession`, `listPaymentSessions` — one financial object bound to a `walletAccountId`, returning display interfaces (payment link, deep link, dynamic/static QR) that all credit that account. `interfaces` is the canonical ARRAY of `{ type, value, format, qr_url?, expires_at?, status? }`; use `client.paymentSessionInterface(session, 'DYNAMIC_QR')` to pick one. Omit `amountMinor` for an open-amount session. Types: `PaymentSession`, `PaymentSessionInterface`, `PaymentSessionInterfaceType`, `CreatePaymentSessionParams`

### Note
- The legacy `createApplicationSettlement` (operator-priced, `sourceWalletId`) remains alongside it. New app-defined-fee flows should use `createBusinessApplicationSettlement`. (Its pricing selectors were removed in 0.10.0.)

## [0.1.0] — 2026-05-15

### Added
- `BanzamiClient` class with `baseUrl` / `apiKey` constructor options, configurable `maxRetries`, and `retryDelay` with exponential backoff
- Consumer management: `createConsumer`, `getConsumer`, `getConsumerByHandle`, `suspendConsumer`, `closeConsumer`
- Consumer wallet management: `getOrCreateConsumerWallet`, `getConsumerWallet`, `getConsumerWalletBalance`, `getConsumerWalletForConsumer`
- P2P transfers: `sendTransfer`, `getTransfer`, `listTransfers` with cursor-based pagination
- QR code operations: `createStaticQr`, `createDynamicQr`, `getQrCode`, `decodeQrPayload`, `markQrUsed`
- Merchant-facing transactions: `createTransaction`, `getTransaction`, `listTransactions` with status filter
- Merchant wallet operations: `getWallet`, `getWalletBalance`
- Payout management: `createPayout`, `listPayouts`
- Merchant API key management: `getMerchant`, `listApiKeys`, `createApiKey`, `revokeApiKey`
- Payment links: `createPaymentLink`, `listPaymentLinks`, `getPaymentLink`, `cancelPaymentLink`, `getPublicPaymentLink`, `getPaymentLinkStatus`
- Webhook endpoint management: `listWebhookEndpoints`, `registerWebhookEndpoint`, `deleteWebhookEndpoint`, `listWebhookEvents`
- `BanzamiApiError` with typed getters: `isNotFound`, `isUnauthorized`, `isForbidden`, `isConflict`, `isInsufficientFunds`, `isHandleNotFound`, `isHandleTaken`, `isWalletNotFound`, `isWalletNotActive`
- Money utilities exported from `@banzami/sdk/money`: `formatMinor`, `addMinor`, `subtractMinor`
- Theme design tokens exported from `@banzami/sdk/theme`: `colors`, `tailwindTokens`, `cssVariables`
- Full TypeScript type exports for all domain models: `Consumer`, `Wallet`, `Transfer`, `Transaction`, `Payout`, `PaymentLink`, `QrCode`, `Merchant`, `WebhookEndpoint`, `WebhookEvent`, and supporting types
- Automatic idempotency key generation for all POST requests
- Retry logic for HTTP 429, 502, 503, and 504 responses
- ESM build targeting ES2020 with declaration files and source maps
- CommonJS build targeting Node.js (`dist/cjs/`) for `require()` compatibility
