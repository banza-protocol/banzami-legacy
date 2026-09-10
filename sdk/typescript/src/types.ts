// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

/** Selects the data universe for all API operations.
 *  'live' uses real money; 'sandbox' uses virtual, simulated funds.
 *  LIVE and SANDBOX data are completely isolated — they never mix. */
export type BanzamiEnvironment = 'live' | 'sandbox';

/*
 * BusinessCategory and PricingProfile used to live here.
 *
 * They were the argument types for createTransaction's pricing selectors, and
 * their own documentation said the quiet part out loud: "an unknown value
 * resolves to a zero fee". A public type whose contract explains how to be
 * charged nothing is not a reference — it is a price list with one entry.
 *
 * Pricing is resolved by the operator from the merchant's assigned profile.
 * No SDK method accepts a category or profile any more, so the types have no
 * argument left to describe and are removed rather than left exported as a
 * shape nothing consumes.
 */

// ---------------------------------------------------------------------------
// Shared
// ---------------------------------------------------------------------------

export interface Page<T> {
  data:         T[];
  next_cursor?: string;
}

// ---------------------------------------------------------------------------
// Consumers
// ---------------------------------------------------------------------------

export type ConsumerStatus = 'ACTIVE' | 'SUSPENDED' | 'CLOSED';

export interface Consumer {
  id:            string;
  handle:        string;
  display_name?: string;
  status:        ConsumerStatus;
  created_at:    string;
  updated_at:    string;
}

// ---------------------------------------------------------------------------
// Consumer wallets
// ---------------------------------------------------------------------------

export type WalletStatus = 'ACTIVE' | 'SUSPENDED' | 'CLOSED';

export interface ConsumerWallet {
  id:          string;
  consumer_id: string;
  currency:    string;
  status:      WalletStatus;
  created_at:  string;
}

export interface WalletBalance {
  available_minor: number;
  reserved_minor:  number;
  currency:        string;
}

// ---------------------------------------------------------------------------
// Transfers (P2P)
// ---------------------------------------------------------------------------

export type TransferStatus = 'PENDING' | 'COMPLETED' | 'FAILED';
export type TransferDirection = 'SENT' | 'RECEIVED';

export interface TransferMoney {
  amount_minor: number;
  currency:     string;
}

export interface Transfer {
  id:           string;
  sender_id:    string;
  recipient_id: string;
  amount:       TransferMoney;
  description?: string;
  status:       TransferStatus;
  created_at:   string;
  updated_at:   string;
}

// ---------------------------------------------------------------------------
// Transactions (merchant-facing)
// ---------------------------------------------------------------------------

export type TransactionStatus = 'PENDING' | 'COMPLETED' | 'FAILED' | 'REFUNDED';

export interface Transaction {
  id:           string;
  merchant_id:  string;
  consumer_id?: string;
  amount_minor: number;
  currency:     string;
  status:       TransactionStatus;
  environment:  'LIVE' | 'SANDBOX';
  reference?:   string;
  description?: string;
  created_at:   string;
  updated_at:   string;
}

// ---------------------------------------------------------------------------
// Wallets (merchant-facing)
// ---------------------------------------------------------------------------

export interface Wallet {
  id:           string;
  merchant_id?: string;
  currency:     string;
  status:       WalletStatus;
  created_at:   string;
}

// ---------------------------------------------------------------------------
// Payouts
// ---------------------------------------------------------------------------

export type PayoutStatus = 'PENDING' | 'PROCESSING' | 'COMPLETED' | 'FAILED';

export interface Payout {
  id:           string;
  wallet_id:    string;
  amount_minor: number;
  currency:     string;
  status:       PayoutStatus;
  reference?:   string;
  created_at:   string;
  updated_at:   string;
}

// ---------------------------------------------------------------------------
// QR codes
// ---------------------------------------------------------------------------

export type QrCodeType   = 'STATIC' | 'DYNAMIC';
export type QrCodeStatus = 'ACTIVE' | 'USED' | 'EXPIRED';

export interface QrCode {
  id:               string;
  owner_id:         string;
  type:             QrCodeType;
  status:           QrCodeStatus;
  amount_minor?:    number;
  currency:         string;
  reference?:       string;
  expires_at?:      string;
  created_at:       string;
}

export interface QrResponse {
  qr_code: QrCode;
  payload: string;
}

export interface ParsedQr {
  owner_id?:    string;
  qr_code_id?:  string;
  type:         QrCodeType;
  currency?:    string;
  is_dynamic:   boolean;
}

// ---------------------------------------------------------------------------
// Merchants
// ---------------------------------------------------------------------------

export type MerchantStatus = 'ACTIVE' | 'SUSPENDED';

export interface Merchant {
  id:         string;
  name:       string;
  status:     MerchantStatus;
  created_at: string;
}

/**
 * The authenticated Business account's own consolidated profile, returned by
 * `getBusinessMe()` (GET /v1/integration). Non-secret fields only — safe to
 * render in an "Integration Health" surface. `settlement_ready` is derived by
 * the operator (ACTIVE + KYB APPROVED + a wallet exists).
 */
export interface BusinessProfile {
  environment:           'LIVE' | 'SANDBOX';
  id:                    string;
  handle:                string;
  business_name:         string;
  business_account_type: string;
  status:                string;
  kyb_status:            string;
  verified:              boolean;
  category:              string | null;
  wallet_ready:          boolean;
  settlement_ready:      boolean;
  /** Derived pricing category (e.g. DONATION); null when unmapped. */
  pricing_category:      string | null;
  /** Not modelled in the operator yet — always null for now. */
  subcategory:           string | null;
  pricing:               BusinessPricing;
  wallet:                BusinessWallet;
  settlement:            BusinessSettlement;
  /** Machine-readable settlement blockers (empty when settlement-ready). */
  blockers:              BusinessBlocker[];
  /**
   * Advisory warnings that DO NOT block settlement, but flag a degraded
   * integration the app should fix (e.g. no active webhook endpoint ⇒ the app
   * relies on polling and may miss confirmations). Empty when nothing to advise.
   */
  warnings:              BusinessWarning[];
}

/** Settlement-readiness blocker reason codes returned by the operator. */
export type BusinessBlocker =
  | 'BUSINESS_NOT_ACTIVE'
  | 'KYB_NOT_APPROVED'
  | 'WALLET_MISSING'
  | 'WALLET_ACCOUNT_MISSING'
  | 'PRICING_MISSING'
  | string;

/**
 * Advisory warning reason codes (never block settlement). WEBHOOK_ENDPOINT_MISSING:
 * no active webhook endpoint is registered, so payment confirmations depend on
 * client-side polling and may be missed — register a webhook (reconciliation is
 * the backstop, not a substitute).
 */
export type BusinessWarning =
  | 'WEBHOOK_ENDPOINT_MISSING'
  | string;

export interface BusinessPricing {
  category: string | null;
  profile:  string;
  rule_key: string;
  /** Operator's own fee for this category, in basis points (informational). */
  fee_bps:  number;
  found:    boolean;
}

export interface BusinessWallet {
  ready:                  boolean;
  wallet_id:              string;
  currency:               string;
  status:                 string;
  primary_account_id:     string;
  application_account_id: string;
}

export interface BusinessSettlement {
  ready:    boolean;
  enabled:  boolean;
  blockers: BusinessBlocker[];
}

export interface ApiKey {
  id:            string;
  prefix:        string;
  label?:        string;
  environment:   'LIVE' | 'SANDBOX';
  created_at:    string;
  last_used_at?: string;
}

export interface NewApiKey extends ApiKey {
  key: string;
}

// ---------------------------------------------------------------------------
// Payment links
// ---------------------------------------------------------------------------

export type PaymentLinkStatus = 'ACTIVE' | 'USED' | 'CANCELLED' | 'EXPIRED';

export interface PaymentLink {
  id:           string;
  slug:         string;
  merchant_id:  string;
  wallet_id:    string;
  amount_minor?: number;
  currency:     string;
  description?: string;
  status:       PaymentLinkStatus;
  expires_at?:  string;
  paid_at?:     string;
  created_at:   string;
  updated_at:   string;
}

/**
 * The official, renderable QR payload for a payment link — derived by the
 * SDK so the payload format stays owned by Banzami. `qrValue` is the
 * canonical, scannable value: encode it into a QR image as-is. Consumers
 * must NOT construct this value themselves; always obtain it from the SDK.
 */
export interface PaymentQr {
  /** Official QR payload — encode this exact string into the QR image. */
  qrValue:          string;
  /** Canonical Banzami pay URL the QR resolves to (same as `qrValue`). */
  paymentUrl:       string;
  /** Payment link slug. */
  slug:             string;
  /** Payment link id. */
  paymentLinkId:    string;
  /** Amount in minor units, or null for open-amount links. */
  amountMinor:      number | null;
  /** ISO currency code (e.g. 'AOA'). */
  currency:         string;
  /** Link description, if any. */
  description:      string | null;
  /** Recipient display name, when known to the caller. */
  recipientName:    string | null;
  /** Recipient @banza handle, when known to the caller. */
  recipientHandle:  string | null;
  /** Environment the link belongs to. */
  environment:      BanzamiEnvironment;
  /** Convenience flag — true in sandbox. */
  isSandbox:        boolean;
  /** Current payment link status. */
  status:           PaymentLinkStatus;
}

// ---------------------------------------------------------------------------
// Refunds
// ---------------------------------------------------------------------------

export type RefundStatus = 'PENDING' | 'SUCCEEDED' | 'FAILED';

/**
 * Typed refund source (BANZA ADR-017). A refund is always tied to an explicit
 * captured payment source — never a generic transfer, never an inferred type.
 *  - `ACQUIRING_PAYMENT`: an external-rail payment; the refund credit lands in transit.
 *  - `WALLET_PAYMENT`: a wallet-native merchant payment; the credit returns to the payer's wallet.
 */
export type RefundSourceType = 'ACQUIRING_PAYMENT' | 'WALLET_PAYMENT';

export interface Refund {
  id:              string;
  source_type:     RefundSourceType;
  source_id:       string;
  merchant_id:     string;
  /** Present only for WALLET_PAYMENT refunds (the payer). */
  consumer_id?:    string | null;
  amount_minor:    number;
  currency:        string;
  status:          RefundStatus;
  reason?:         string | null;
  created_at:      string;
  updated_at:      string;
}

export interface CreateRefundParams {
  /** The typed source class (BANZA ADR-017). Required — never inferred. */
  source_type:      RefundSourceType;
  /** Id of the typed source object (a transaction id, or a wallet-payment id). */
  source_id:        string;
  /** Partial amount in minor units; must stay within the source's refund ceiling. */
  amount_minor:     number;
  /** ISO-4217 code; the authoritative refund currency is the source's currency. */
  currency:         string;
  reason?:          string;
  /**
   * Idempotency key — REQUIRED. A refund is a financial write; the caller MUST
   * supply a stable key scoped to the refund intent so that retries converge on
   * the original result. The SDK never generates one: a browser- or SDK-minted
   * random key would make a retried refund create a second movement. Generate it
   * server-side and persist it before the first attempt.
   */
  idempotency_key:  string;
}

// ---------------------------------------------------------------------------
// Disputes
// ---------------------------------------------------------------------------

export type DisputeStatus =
  | 'OPEN'
  | 'UNDER_REVIEW'
  | 'WON_BY_CONSUMER'
  | 'WON_BY_MERCHANT'
  | 'CLOSED';

export interface Dispute {
  id:                string;
  transaction_id:    string;
  merchant_id:       string;
  consumer_id:       string;
  amount_minor:      number;
  currency:          string;
  reason:            string;
  status:            DisputeStatus;
  evidence_deadline?: string;
  resolution_notes?:  string;
  resolved_at?:       string;
  created_at:        string;
  updated_at:        string;
}

export interface OpenDisputeParams {
  transaction_id:   string;
  consumer_id:      string;
  amount_minor:     number;
  currency:         string;
  reason:           string;
  evidence_deadline?: string;
}

export interface ListDisputesParams {
  status?: DisputeStatus;
  limit?:  number;
}

// ---------------------------------------------------------------------------
// Payment requests
// ---------------------------------------------------------------------------

export type PaymentRequestStatus =
  | 'PENDING'
  | 'PAID'
  | 'DECLINED'
  | 'CANCELLED'
  | 'EXPIRED';

export interface PaymentRequest {
  id:             string;
  requester_id:   string;
  payer_id?:      string;
  amount_minor:   number;
  currency:       string;
  description?:   string;
  status:         PaymentRequestStatus;
  expires_at?:    string;
  created_at:     string;
  updated_at:     string;
}

export interface CreatePaymentRequestParams {
  requester_id:     string;
  payer_handle?:    string;
  amount_minor:     number;
  currency:         string;
  description?:     string;
  expires_at?:      string;
  idempotency_key?: string;
}

export interface ListPaymentRequestsParams {
  status?: PaymentRequestStatus;
  limit?:  number;
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

export type WebhookEndpointStatus = 'ACTIVE' | 'DISABLED';

export interface WebhookEndpoint {
  id:         string;
  url:        string;
  events:     string[];
  status?:    WebhookEndpointStatus;
  created_at: string;
  /** Owner of the endpoint. With a Developer Platform key this is your
   *  project's bound owner — you never supply it. */
  merchant_id?: string;
  active?:      boolean;
  /**
   * The signing secret, present ONLY in the response to
   * `createWebhookEndpoint` and `rotateWebhookEndpointSecret`, and only in that
   * one response. It is stored encrypted and is never readable afterwards — a
   * get or list never carries it. Persist it when you receive it, or rotate to
   * obtain a new one.
   */
  secret?:      string;
}

/**
 * Canonical event types dispatched by the Banzami api-gateway.
 * Use `string` for forward-compatibility with types not yet in this list.
 */
export type WebhookEventType =
  | 'payment_link.paid'
  | 'transaction.completed'
  | 'transaction.failed'
  | 'payout.created'
  | 'payout.completed'
  | 'payout.failed';

export interface WebhookEvent {
  id:         string;
  type:       WebhookEventType | string;
  /** Event-specific payload. Cast to a typed interface after checking `type`. */
  data:       unknown;
  created_at: string;
}

// Application Settlement (Banzami ADR-021) — an app settles accumulated net value
// from one of its wallets to a beneficiary, splitting off an application fee.
export type ApplicationSettlementStatus = 'CREATED' | 'PENDING' | 'COMPLETED' | 'FAILED' | 'CANCELLED';

export interface ApplicationSettlement {
  id: string;
  owner_ref: string;
  status: ApplicationSettlementStatus;
  gross_amount_minor: number;
  application_fee_minor: number;
  net_amount_minor: number;
  currency: string;
  environment: string;
  created_at: string;
  completed_at: string | null;
  failure_reason: string | null;
}

export interface CreateApplicationSettlementParams {
  idempotencyKey: string;
  ownerRef: string;
  sourceWalletId: string;
  beneficiaryWalletId: string;
  applicationFeeWalletId?: string;
}

// ---------------------------------------------------------------------------
// Wallet Accounts (ADR-042) — segregated accounts within a wallet
// ---------------------------------------------------------------------------

export type WalletAccountPurpose =
  | 'PRIMARY' | 'CAMPAIGN' | 'PROJECT' | 'EVENT'
  | 'STORE' | 'ESCROW' | 'RESERVE' | 'SETTLEMENT' | 'CUSTOM';

/** A segregated account inside a wallet. The balance is read from the operator
 *  ledger — the app never holds or computes it. Ledger account ids are never
 *  exposed; reference an account by `id`. */
export interface WalletAccount {
  id: string;
  wallet_id: string;
  purpose: WalletAccountPurpose;
  reference_type: string | null;
  reference_id: string | null;
  label: string | null;
  status: string;
  available_balance_minor: number;
  currency: string;
  created_at: string;
}

export interface CreateWalletAccountParams {
  /**
   * The wallet the new account is opened under.
   *
   * Which credential you hold decides whether you send this at all:
   *
   * - **Developer Platform key** — OMIT it. The wallet is derived from your
   *   project's Banzami binding, and the API rejects a client-supplied wallet
   *   outright, even one that happens to be your own. Naming the wallet is
   *   naming the owner, and that is authority rather than configuration.
   * - **Merchant credential** — supply it; ownership is checked server-side.
   */
  walletId?: string;
  purpose: WalletAccountPurpose;
  referenceType?: string;
  referenceId?: string;
  label?: string;
}

// ---------------------------------------------------------------------------
// Application Settlement (ADR-029)
// ---------------------------------------------------------------------------

/** Close-out of a segregated account. The operator reads the gross from the
 *  account's real balance, applies the RATE ASSIGNED TO YOUR BUSINESS, splits
 *  fee→fee-destination / net→beneficiary, and audits it. You send no amount and
 *  never compute the fee. Beneficiary and fee destination are given as @banza
 *  names; the operator resolves them.
 *
 *  There is no rate field. `applicationFeeBps` used to be here and used to work:
 *  a non-zero value made the operator skip pricing entirely and charge whatever
 *  the caller asked for. A caller cannot set the price of the service it is
 *  buying, so the rate now comes from the pricing profile assigned to your
 *  business. The operator still accepts the old field on the wire so existing
 *  integrations do not break — it just no longer decides anything. */
export interface CreateBusinessApplicationSettlementParams {
  /** A segregated wallet account id the caller owns (e.g. a CAMPAIGN account). */
  sourceAccountId: string;
  beneficiaryBanzaName: string;
  /** Where the fee goes when your profile charges one. Must be your own
   *  business account; the operator refuses a destination you do not own. */
  feeDestinationBanzaName?: string;
  reason?: string;
  referenceType?: string;
  referenceId?: string;
  idempotencyKey: string;
}

// ---------------------------------------------------------------------------
// Payment Sessions (BANZA ADR-015) — one financial object, many interfaces
// ---------------------------------------------------------------------------

export type PaymentSessionInterfaceType = 'PAYMENT_LINK' | 'DYNAMIC_QR' | 'STATIC_QR' | 'DEEP_LINK';

/** One interface presenting a session (canonical ADR-043 shape). `value` is the
 *  presentable artifact — a URL (link/deep link) or a signed QR payload — and
 *  carries only an opaque session reference, never an account id. The app DISPLAYS
 *  it; it never builds a financial payload itself. */
export interface PaymentSessionInterface {
  type: PaymentSessionInterfaceType;
  value: string;
  format?: 'URL' | 'QR_PAYLOAD' | 'PNG' | 'SVG' | 'PDF' | null;
  /** Gateway path that renders a QR image (QR interfaces only). */
  qr_url?: string;
  expires_at?: string | null;
  status?: string | null;
}

/** A Payment Session bound to one `wallet_account_id`. The app references it by
 *  `session_id` and shows its `interfaces` (an array); it never sees a ledger
 *  account id. Status: CREATED | ACTIVE | PAID | PARTIALLY_PAID | EXPIRED |
 *  CANCELLED | FAILED. */
export interface PaymentSession {
  session_id: string;
  wallet_account_id: string;
  currency: string;
  amount_minor: number | null;
  purpose: string | null;
  reference_type: string | null;
  reference_id: string | null;
  status: string;
  expires_at: string | null;
  created_at: string;
  interfaces: PaymentSessionInterface[];
  /**
   * Merchant-safe refundable-source reference (Banzami operator extension).
   * Present only after the session's payment has settled; carries the PUBLIC
   * typed source you feed straight into `createRefund`. Absent (undefined)
   * before payment. `source_type` is `ACQUIRING_PAYMENT` | `WALLET_PAYMENT` —
   * never an internal token. NOT a BANZA-normative field (see ADR-018 draft).
   */
  refund_source?: RefundSource;
}

/** The typed source pair returned by refundable-source discovery and accepted
 *  by {@link CreateRefundParams}. */
export interface RefundSource {
  source_type: RefundSourceType;
  source_id: string;
}

export interface CreatePaymentSessionParams {
  /**
   * The segregated wallet account the session credits (e.g. a CAMPAIGN account).
   *
   * Which credential you hold decides whether you send this at all:
   *
   * - **Developer Platform key** (`bz_test_sk_…`, issued by the Developer
   *   Console) — OMIT it. The payee is derived server-side from the project's
   *   Banzami binding, and the API rejects a client-supplied payee outright.
   *   A client that could name its own payee could name someone else's.
   * - **Merchant credential** — supply it. The server requires it on that route
   *   and independently re-validates that the account is owned by the merchant.
   *
   * Optional in the type because the developer path — the documented one — must
   * not send it. That does not make it optional on the merchant route: the
   * server still requires it there, and this SDK never infers or substitutes a
   * value.
   */
  walletAccountId?: string;
  purpose?: string;
  referenceType?: string;
  referenceId?: string;
  /** Omit for an open-amount session; set (minor units) for a fixed-amount one. */
  amountMinor?: number;
  currency?: string;
  description?: string;
  expiresAt?: Date;
  metadata?: Record<string, unknown>;
}

export interface CreateWebhookEndpointParams {
  /** Must be a public https URL. */
  url: string;
  /** Event types to receive. An unlisted name is rejected rather than silently
   *  accepted, because an accepted typo produces an endpoint that never fires. */
  events: string[];
}

export interface WebhookDeliveryRecord {
  id: string;
  event_id: string;
  endpoint_id: string;
  status: string;
  attempt?: number;
  response_status?: number | null;
  created_at: string;
  [k: string]: unknown;
}

export interface WebhookEndpointHealth {
  endpoint_id?: string;
  [k: string]: unknown;
}

// ---------------------------------------------------------------------------
// Transferências — money between your own accounts
// ---------------------------------------------------------------------------

/**
 * An internal transfer between two wallet accounts of the SAME financial owner
 * (Banzami ADR-052).
 *
 * This is deliberately the smallest safe transfer product: money moves between
 * two child accounts your project's bound owner already holds. It is NOT a
 * payout, NOT an application settlement, and NOT a transfer to another party —
 * nothing here can move value outside your own owner.
 */
export interface WalletAccountTransfer {
  id: string;
  source_wallet_account_id: string;
  destination_wallet_account_id: string;
  amount_minor: number;
  currency: string;
  status: string;
  description?: string | null;
  created_at: string;
}

export interface CreateTransferParams {
  /** An account belonging to your project's bound owner. */
  sourceWalletAccountId: string;
  /** Another account belonging to the SAME owner. Must differ from the source. */
  destinationWalletAccountId: string;
  amountMinor: number;
  currency: string;
  /**
   * Required, not optional. A retry with the same key returns the original
   * transfer and moves nothing; without one, every retry would be a new
   * transfer, which is the opposite of what a retry means.
   */
  idempotencyKey: string;
  description?: string;
}
