// Package coreclient is the developer-api → Core internal boundary (ADR-047 /
// RT04B §3). Two things cross it, each with its own credential and neither
// borrowing the other's.
//
// Payee validation is the original one: before recording a Project→Merchant
// binding, Core is asked whether merchant→wallet→wallet_account is real, because
// Core is independently authoritative for that relationship and the operator
// provisioning path must never trust submitted Core identifiers.
//
// Refunds are the second, added when the Console gained its own refund. That one
// does move money, which is why it is a separate client with a separate key
// rather than a method on the first: the package no longer holds only read
// authority, and the boundary should say so instead of quietly widening.
package coreclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnavailable means Core could not be reached or did not usably answer — the
// caller must fail closed (never record a binding on an unverified payee).
var ErrUnavailable = errors.New("core payee validation unavailable")

// Client calls Core's internal validate-payee endpoint.
type Client struct {
	baseURL     string
	internalKey string
	http        *http.Client
}

// New returns nil when unconfigured (base URL or internal key missing) so the
// caller can fail closed on binding.
func New(baseURL, internalKey string) *Client {
	if baseURL == "" || internalKey == "" {
		return nil
	}
	return &Client{
		baseURL:     baseURL,
		internalKey: internalKey,
		http:        &http.Client{Timeout: 5 * time.Second},
	}
}

// ValidatePayee asks Core whether merchant→wallet→wallet_account is a valid,
// active, sandbox payee. Returns (valid, reason). A transport/decode/non-200
// fault returns ErrUnavailable so the caller fails closed. No Core detail leaks.
func (c *Client) ValidatePayee(ctx context.Context, merchantID, walletID, walletAccountID string) (bool, string, error) {
	body, _ := json.Marshal(map[string]string{
		"merchant_id":       merchantID,
		"wallet_id":         walletID,
		"wallet_account_id": walletAccountID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/v1/wallet-accounts/validate-payee", bytes.NewReader(body))
	if err != nil {
		return false, "", ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", c.internalKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", ErrUnavailable // 401/403/5xx/malformed target → fail closed
	}
	var out struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", ErrUnavailable
	}
	return out.Valid, out.Reason, nil
}

// ── Refunds ──────────────────────────────────────────────────────────────────
//
// The Console's own refund. Until this, the package validated a payee and
// nothing else: it held no financial state and started no movement of money.
// A refund does move money, so the boundary is drawn explicitly rather than by
// reusing the payee credential.
//
// Core's refund group is the one `/internal` family that authenticates its
// caller, against CORE_INTERNAL_KEY. That is the same key the Gateway holds —
// there is one refund service credential, not one per caller — so this client
// takes it as its own field and is nil when it is not configured. A Console
// with no refund key reports the capability as unavailable instead of failing
// at the moment someone presses the button.
//
// Authority is settled before we get here: the caller is a workspace member with
// a role that may refund, the project's ACTIVE binding names the merchant, and
// the merchant is the one this client sends. Core re-checks independently that
// the source belongs to that merchant and is eligible, so a mistake on this side
// still meets a refusal on the other.

// RefundClient is the subset used for refunding. Nil when unconfigured.
type RefundClient struct {
	baseURL   string
	refundKey string
	http      *http.Client
}

// NewRefund returns nil when unconfigured, so the caller can present the
// capability as unavailable rather than discovering it at the point of use.
func NewRefund(baseURL, refundKey string) *RefundClient {
	if baseURL == "" || refundKey == "" {
		return nil
	}
	return &RefundClient{baseURL: baseURL, refundKey: refundKey, http: &http.Client{Timeout: 15 * time.Second}}
}

// PaymentSource is what Core says a payment can be refunded against: the typed
// source of BANZA ADR-017, plus the merchant that owns the payment so the caller
// can check it against the one its own authority chain produced.
type PaymentSource struct {
	MerchantID  string
	SourceType  string
	SourceID    string
	AmountMinor *int64
	Currency    string
	Status      string
}

// ErrNoRefundSource means the payment exists but nothing has been paid against
// it yet, so there is nothing to give back. Distinct from "not found" because
// they are different answers and lead to different words on screen.
var ErrNoRefundSource = errors.New("payment has no refundable source")

// PaymentSession reads a payment session and its refundable source. The read is
// unauthenticated at Core (only the refund group is gated), so ownership is NOT
// established by being able to read it — the caller must compare MerchantID
// against the merchant its own authority chain produced.
func (c *RefundClient) PaymentSession(ctx context.Context, id string) (*PaymentSource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/internal/v1/payment-sessions/"+id, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	var out struct {
		MerchantID   string `json:"merchant_id"`
		AmountMinor  *int64 `json:"amount_minor"`
		Currency     string `json:"currency"`
		Status       string `json:"status"`
		RefundSource *struct {
			SourceType string `json:"source_type"`
			SourceID   string `json:"source_id"`
		} `json:"refund_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrUnavailable
	}
	p := &PaymentSource{
		MerchantID:  out.MerchantID,
		AmountMinor: out.AmountMinor,
		Currency:    out.Currency,
		Status:      out.Status,
	}
	if out.RefundSource == nil || out.RefundSource.SourceID == "" {
		return p, ErrNoRefundSource
	}
	p.SourceType = out.RefundSource.SourceType
	p.SourceID = out.RefundSource.SourceID
	return p, nil
}

// ErrNotFound means Core has no such object.
var ErrNotFound = errors.New("not found")

// RefundRejected carries Core's own refusal — its code and message — so the
// reason a refund did not happen survives the trip instead of collapsing into
// "something went wrong". Core's vocabulary here is caller-facing by design
// (INSUFFICIENT_FUNDS, REFUND_CEILING_EXCEEDED, IDEMPOTENCY_KEY_CONFLICT).
type RefundRejected struct {
	Status  int
	Code    string
	Message string
}

func (e *RefundRejected) Error() string { return e.Code + ": " + e.Message }

// Refund is what Core created.
type Refund struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	SourceType  string `json:"source_type"`
	SourceID    string `json:"source_id"`
}

// CreateRefund posts the refund. The idempotency key is the caller's and is
// never minted here: a financial write that quietly invents its own retry key
// turns a double-submit into two refunds.
func (c *RefundClient) CreateRefund(ctx context.Context, merchantID, sourceType, sourceID string,
	amountMinor int64, currency, reason, idempotencyKey string) (*Refund, error) {
	body, _ := json.Marshal(map[string]any{
		"source_type":     sourceType,
		"source_id":       sourceID,
		"merchant_id":     merchantID,
		"amount_minor":    amountMinor,
		"currency":        currency,
		"reason":          reason,
		"idempotency_key": idempotencyKey,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/v1/refunds", bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", c.refundKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		code := e.Error.Code
		if code == "" {
			code = "REFUND_FAILED"
		}
		return nil, &RefundRejected{Status: resp.StatusCode, Code: code, Message: e.Error.Message}
	}
	var out Refund
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ErrUnavailable
	}
	return &out, nil
}

// ── Sandbox financial provisioning ──────────────────────────────────────────
//
// The third thing that crosses this boundary, and the one that creates rather
// than reads. A developer who has just made a Sandbox project has no financial
// owner, and until now the only way to get one was an operator calling an
// internal route the developer could not see. This is what makes that step the
// developer's own.
//
// It creates exactly what the canonical model needs and nothing more: a Sandbox
// merchant and one wallet. The wallet's PRIMARY account is created by Core's own
// trigger, not by this client — a segregated CAMPAIGN account is the developer's
// to open, through the public API, when their application needs one.
//
// These Core routes are not credential-gated; the network boundary is what
// guards them. That is Core's decision and not this client's to work around, but
// it is why every caller of this must have established its own authority first:
// nothing downstream will ask again.

// ProvisionClient creates a Sandbox financial owner. Nil when unconfigured.
type ProvisionClient struct {
	baseURL string
	http    *http.Client
}

// NewProvision returns nil without a base URL, so an unconfigured deployment
// reports the capability as unavailable rather than half-creating an owner.
func NewProvision(baseURL string) *ProvisionClient {
	if baseURL == "" {
		return nil
	}
	return &ProvisionClient{baseURL: baseURL, http: &http.Client{Timeout: 20 * time.Second}}
}

// SandboxOwner is what was provisioned. The developer never sees these ids; they
// exist so the binding can name a payee and so the audit trail can be followed.
type SandboxOwner struct {
	MerchantID      string
	WalletID        string
	WalletAccountID string
}

func (c *ProvisionClient) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("core %s: %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}

// ProvisionSandboxOwner creates the merchant and wallet, and returns the PRIMARY
// account Core's trigger made alongside the wallet.
//
// Resumable at every step, because the first version was not and it showed
// immediately: a run that created the merchant and then failed reading the
// PRIMARY account left the merchant behind, and the retry died on the unique
// index over merchants.email. One project, two failures, no owner, and an orphan
// merchant nobody was going to find.
//
// So each step looks before it creates. The derived email is what makes that
// safe: it is unique per project by construction, so "a merchant with this
// address already exists" means "this project's own, from an earlier attempt"
// and never somebody else's.
//
// `name` and `email` are derived from the project by the caller, never supplied
// by the developer: a caller-chosen merchant name is a caller-chosen identity.
func (c *ProvisionClient) ProvisionSandboxOwner(ctx context.Context, name, email string) (*SandboxOwner, error) {
	merchantID, err := c.findMerchantByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if merchantID == "" {
		var created struct {
			ID string `json:"id"`
		}
		// The ADR-028 taxonomy is declared, not defaulted. A Developer Project's
		// Business exists to route value on behalf of an application, which is
		// what APPLICATION means; the create-time default is MERCHANT, and only
		// APPLICATION/PLATFORM may be an application-fee destination. Leaving it
		// to the default made all nine self-service Businesses fail their own
		// fee-destination check with FEE_DESTINATION_TYPE_NOT_ALLOWED.
		//
		// Readiness asserts it again for owners created before this. Both, so a
		// merchant is never left with the wrong type if readiness does not run.
		if err := c.post(ctx, "/internal/v1/merchants", map[string]any{
			"name": name, "email": email, "business_account_type": "APPLICATION",
		}, &created); err != nil {
			return nil, err
		}
		merchantID = created.ID
	}
	if merchantID == "" {
		return nil, ErrUnavailable
	}
	owner := &SandboxOwner{MerchantID: merchantID}

	walletID, err := c.findWallet(ctx, merchantID)
	if err != nil {
		return owner, err
	}
	if walletID == "" {
		var created struct {
			ID string `json:"id"`
		}
		if err := c.post(ctx, "/internal/v1/wallets",
			map[string]any{"merchant_id": merchantID, "currency": "AOA"}, &created); err != nil {
			return owner, err
		}
		walletID = created.ID
	}
	if walletID == "" {
		return owner, ErrUnavailable
	}
	owner.WalletID = walletID

	acct, err := c.primaryAccount(ctx, walletID)
	if err != nil {
		return owner, err
	}
	owner.WalletAccountID = acct
	return owner, nil
}

// ProvisionSandboxReadiness completes a Business so it can actually SETTLE.
//
// Financial Setup created a merchant, a wallet and its PRIMARY account and
// stopped there. That is enough to receive money and not enough to move it:
// application settlement names its parties by @banza, and a Business with no
// entry in the handle registry cannot be named — not as a beneficiary, and not
// as its own application-fee destination. Zero of the Sandbox owners this
// platform had provisioned had a handle, so no ordinary external Developer
// Project could complete a settlement.
//
// The handle is DERIVED from the project, exactly as the merchant's name and
// address already are. A caller-chosen handle is a caller-chosen identity, and
// handles are a scarce public namespace: a developer naming their Sandbox
// Business "banco" would reserve it.
//
// Core refuses this in LIVE on its own reading of the environment; the gate here
// is the second of two, not the only one.
func (c *ProvisionClient) ProvisionSandboxReadiness(ctx context.Context, merchantID, projectID string) (handle, kybStatus string, err error) {
	var out struct {
		Handle              string `json:"handle"`
		KybStatus           string `json:"kyb_status"`
		BusinessAccountType string `json:"business_account_type"`
	}
	if err := c.post(ctx, "/internal/v1/sandbox/business-readiness", map[string]any{
		"merchant_id": merchantID,
		"handle":      DeriveSandboxHandle(projectID),
	}, &out); err != nil {
		return "", "", err
	}
	return out.Handle, out.KybStatus, nil
}

// DeriveSandboxHandle turns a project id into a valid @banza handle.
//
// Deterministic, so a retry asks for the same one; prefixed with a letter
// because a handle must start with one; hex only, so it can never spell a word
// someone would want or read as an endorsement. 13 characters, inside the 3..20
// the registry allows.
//
// It hashes rather than slicing the id. Taking a prefix and padding short input
// made distinct projects derive the same handle — the empty string and the
// all-zero UUID both produced p000000000000 — and two Businesses contending for
// one identity is not a thing to leave to whether project ids are always
// well-formed. A hash is total: every distinct input gets its own handle.
//
// A collision would still be refused by Core rather than resolved in anyone's
// favour, so the worst case is an error, never a stolen identity.
func DeriveSandboxHandle(projectID string) string {
	sum := sha256.Sum256([]byte(projectID))
	return "p" + hex.EncodeToString(sum[:])[:12]
}

// findMerchantByEmail returns this project's own merchant from an earlier
// attempt, or empty. Matched on the exact address rather than on the search
// result's first row: a substring search is not an identity check.
func (c *ProvisionClient) findMerchantByEmail(ctx context.Context, email string) (string, error) {
	var list []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := c.get(ctx, "/internal/v1/merchants?search="+url.QueryEscape(email), &list); err != nil {
		return "", err
	}
	for _, m := range list {
		if strings.EqualFold(m.Email, email) {
			return m.ID, nil
		}
	}
	return "", nil
}

// findWallet returns the merchant's AOA wallet if it already has one.
func (c *ProvisionClient) findWallet(ctx context.Context, merchantID string) (string, error) {
	var w struct {
		ID string `json:"id"`
	}
	err := c.get(ctx, "/internal/v1/wallets?merchant_id="+url.QueryEscape(merchantID)+"&currency=AOA", &w)
	if err != nil {
		// A merchant with no wallet answers not-found, which is an answer and not
		// a fault: the next step creates one.
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return w.ID, nil
}

// get is the read half of this client. A 404 is reported as ErrNotFound so a
// caller can tell "there is none" from "the call did not work".
func (c *ProvisionClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return ErrUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("core %s: %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}

// primaryAccount reads the account Core's own trigger created with the wallet.
// It is never created here: Core rejects an attempt to open a PRIMARY through
// the wallet-accounts route, and rightly — one wallet has exactly one.
func (c *ProvisionClient) primaryAccount(ctx context.Context, walletID string) (string, error) {
	var page struct {
		Data []struct {
			ID      string `json:"id"`
			Purpose string `json:"purpose"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/internal/v1/wallets/"+url.PathEscape(walletID)+"/accounts", &page); err != nil {
		return "", err
	}
	for _, a := range page.Data {
		if a.Purpose == "PRIMARY" {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("no PRIMARY account on wallet %s", walletID)
}

// CreateWalletAccount opens a segregated account under a merchant's wallet.
//
// This is the primitive DOA uses for a campaign, reached the same way: the
// caller supplies a purpose, a reference and a label, and never a merchant or a
// wallet. Those are derived from the project's binding by the service above.
//
// PRIMARY is not creatable here and Core refuses it — one wallet has exactly
// one, made with the wallet itself.
func (c *ProvisionClient) CreateWalletAccount(ctx context.Context, walletID, merchantID, purpose, refType, refID, label string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "/internal/v1/wallet-accounts", map[string]any{
		"wallet_id":      walletID,
		"merchant_id":    merchantID,
		"currency":       "AOA",
		"purpose":        purpose,
		"reference_type": refType,
		"reference_id":   refID,
		"label":          label,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", ErrUnavailable
	}
	return out.ID, nil
}

// WalletForMerchant resolves the merchant's AOA wallet. Exported for the service,
// which needs it to open an account under a project's own owner without ever
// letting the caller name one.
func (c *ProvisionClient) WalletForMerchant(ctx context.Context, merchantID string) (string, error) {
	return c.findWallet(ctx, merchantID)
}

// AssignPricingProfile records the operator-governed pricing policy for a
// financial owner.
//
// Self-service provisioning calls this with the explicit Sandbox default, so a
// developer's project is priced by a rule that says zero rather than by nothing
// matching. Those are different states and the whole point of this work is that
// they stay different.
func (c *ProvisionClient) AssignPricingProfile(ctx context.Context, merchantID, profileCode string) error {
	b, _ := json.Marshal(map[string]string{"profile_code": profileCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/internal/v1/merchants/"+url.PathEscape(merchantID)+"/pricing-profile",
		bytes.NewReader(b))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("core pricing-profile assign: %d", resp.StatusCode)
	}
	return nil
}
