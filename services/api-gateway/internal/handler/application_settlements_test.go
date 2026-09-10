package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/banzami/banzami/services/api-gateway/internal/middleware"
	"github.com/banzami/banzami/services/api-gateway/internal/service"
)

type fakeWallets struct {
	merchantID string
	available  int64
}

func (f *fakeWallets) Create(ctx context.Context, m, c string) (*service.WalletRecord, error) {
	return nil, nil
}
func (f *fakeWallets) Get(ctx context.Context, id string) (*service.WalletRecord, error) {
	return &service.WalletRecord{ID: id, MerchantID: f.merchantID, Currency: "AOA"}, nil
}
func (f *fakeWallets) Balance(ctx context.Context, id string) (*service.WalletBalance, error) {
	return &service.WalletBalance{WalletID: id, Currency: "AOA", AvailableMinor: f.available}, nil
}
func (f *fakeWallets) GetForMerchant(ctx context.Context, m, c string) (*service.WalletRecord, error) {
	return nil, nil
}
func (f *fakeWallets) SandboxFund(ctx context.Context, id string, a int64, c string) (*service.WalletBalance, error) {
	return nil, nil
}
func (f *fakeWallets) Analytics(ctx context.Context, id, from, to string) (json.RawMessage, error) {
	return nil, nil
}

type fakeSettlements struct {
	created, completed int
	lastInput          service.CreateApplicationSettlementInput
}

func (f *fakeSettlements) Create(ctx context.Context, in service.CreateApplicationSettlementInput) (*service.ApplicationSettlement, error) {
	f.created++
	f.lastInput = in
	// Echo the gross the gateway read so the test can assert it was used.
	return &service.ApplicationSettlement{ID: "set-1", OwnerRef: in.OwnerRef, Status: "CREATED", GrossAmountMinor: in.GrossAmountMinor, Currency: in.Currency}, nil
}
func (f *fakeSettlements) Complete(ctx context.Context, id string) (*service.ApplicationSettlement, error) {
	f.completed++
	return &service.ApplicationSettlement{ID: id, Status: "COMPLETED", GrossAmountMinor: 100000, ApplicationFeeMinor: 5000, NetAmountMinor: 95000, Currency: "AOA"}, nil
}
func (f *fakeSettlements) Get(ctx context.Context, id string) (*service.ApplicationSettlement, error) {
	return &service.ApplicationSettlement{ID: id, Status: "COMPLETED"}, nil
}

// fakeParties resolves @handle → an account; keyed by handle so beneficiary and
// fee destination get distinct accounts. ownerType/ownerID configurable.
type fakeParties struct {
	ownerType string // default MERCHANT
	ownerID   string // default doa-merchant
	err       error
}

func (f *fakeParties) Resolve(ctx context.Context, handle, currency string) (*service.ResolvedParty, error) {
	if f.err != nil {
		return nil, f.err
	}
	ot := f.ownerType
	if ot == "" {
		ot = "MERCHANT"
	}
	oid := f.ownerID
	if oid == "" {
		oid = "doa-merchant"
	}
	h := strings.TrimPrefix(handle, "@") // mirror the real resolver
	return &service.ResolvedParty{Handle: h, OwnerType: ot, OwnerID: oid, AvailableAccountID: "acct-" + h, Currency: currency}, nil
}

func postBusiness(h *ApplicationSettlementHandler, principalMerchant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/application-settlements", strings.NewReader(body))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{MerchantID: principalMerchant, Environment: "SANDBOX"}))
	rec := httptest.NewRecorder()
	h.CreateBusiness(rec, req)
	return rec
}

func postSettlement(t *testing.T, h *ApplicationSettlementHandler, principalMerchant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/application-settlements", strings.NewReader(body))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{MerchantID: principalMerchant, Environment: "SANDBOX"}))
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	return rec
}

// Ownership: an app may only settle FROM a wallet it owns.
func TestApplicationSettlement_RejectsForeignSourceWallet(t *testing.T) {
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "other-merchant", available: 100000}, &fakeWalletAccounts{}, &fakeParties{}, pricedFake())
	rec := postSettlement(t, h, "doa-merchant",
		`{"idempotency_key":"k1","owner_ref":"camp-1","source_wallet_id":"w-src","beneficiary_wallet_id":"w-ben"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign source wallet must be 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Happy path: owned source + balance → create + complete, gross = read balance.
func TestApplicationSettlement_CreatesAndCompletes(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant", available: 100000}, &fakeWalletAccounts{}, &fakeParties{}, pricedFake())
	rec := postSettlement(t, h, "doa-merchant",
		`{"idempotency_key":"k1","owner_ref":"camp-1","source_wallet_id":"w-src","beneficiary_wallet_id":"w-ben","application_fee_wallet_id":"w-fee","fee_policy_ref":"doa-5pct"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out service.ApplicationSettlement
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "COMPLETED" {
		t.Fatalf("settlement must be COMPLETED, got %s", out.Status)
	}
	if fs.created != 1 || fs.completed != 1 {
		t.Fatalf("expected one create + one complete, got create=%d complete=%d", fs.created, fs.completed)
	}
}

// Nothing to settle: zero balance → 422, never creates a settlement.
func TestApplicationSettlement_ZeroBalance(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant", available: 0}, &fakeWalletAccounts{}, &fakeParties{}, pricedFake())
	rec := postSettlement(t, h, "doa-merchant",
		`{"idempotency_key":"k1","owner_ref":"camp-1","source_wallet_id":"w-src","beneficiary_wallet_id":"w-ben"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zero balance must be 422, got %d", rec.Code)
	}
	if fs.created != 0 {
		t.Fatal("must not create a settlement when there is nothing to settle")
	}
}

// ADR-042: settle FROM a specific segregated (campaign) account. Gross is the
// account balance; the app sends no amount and never sees the ledger account id.
func TestApplicationSettlement_FromCampaignAccount(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 95000}, &fakeParties{}, pricedFake())
	rec := postSettlement(t, h, "doa-merchant",
		`{"idempotency_key":"k1","owner_ref":"camp-1","source_wallet_account_id":"wa-1","beneficiary_wallet_id":"w-ben"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.created != 1 || fs.completed != 1 {
		t.Fatalf("expected create+complete, got created=%d completed=%d", fs.created, fs.completed)
	}
}

// Ownership: a campaign account whose parent wallet belongs to another merchant
// must be rejected.
func TestApplicationSettlement_FromForeignCampaignAccount(t *testing.T) {
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "other-merchant"}, &fakeWalletAccounts{}, &fakeParties{}, pricedFake())
	rec := postSettlement(t, h, "doa-merchant",
		`{"idempotency_key":"k1","owner_ref":"camp-1","source_wallet_account_id":"wa-1","beneficiary_wallet_id":"w-ben"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign campaign account must be 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ADR-029 — business settlement (/v1/application-settlements)
// ---------------------------------------------------------------------------

// Still carries application_fee_bps, because every deployed integration does and
// the handler must keep accepting it. It no longer decides anything.
const doaBody = `{"idempotency_key":"doa-c1","source_account_id":"wa-camp","beneficiary_banza_name":"@maria","fee_destination_banza_name":"@doa","application_fee_bps":500,"reason":"CAMPAIGN_CLOSE","reference_type":"DOA_CAMPAIGN","reference_id":"campaign_123"}`

// Happy path on an owned CAMPAIGN account → create + complete, with the resolved
// beneficiary/fee accounts, the real balance as the gross, and the merchant's
// own pricing profile.
//
// This test used to assert the opposite of its last check: that a caller's 500
// bps "must reach core". It did reach core, and core treats a non-zero
// application_fee_bps as the APP-DEFINED path — the Pricing Engine is not
// consulted at all. So the assertion was pinning a hole: the caller was setting
// the price of the service it was buying, up to the 50% domain maximum.
func TestBusinessSettlement_OperatorPricedFee(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 200000}, &fakeParties{}, pricedFake())
	rec := postBusiness(h, "doa-merchant", doaBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.created != 1 || fs.completed != 1 {
		t.Fatalf("want create+complete, got created=%d completed=%d", fs.created, fs.completed)
	}
	if fs.lastInput.ApplicationFeeBps != 0 {
		t.Fatalf("the caller's rate must not reach core, got %d", fs.lastInput.ApplicationFeeBps)
	}
	if fs.lastInput.PricingProfile == "" {
		t.Fatal("the operator's assigned pricing profile must be what prices this")
	}
	if fs.lastInput.BeneficiaryAccountID != "acct-maria" || fs.lastInput.ApplicationFeeAccountID != "acct-doa" {
		t.Fatalf("resolved @names must be passed: ben=%q fee=%q", fs.lastInput.BeneficiaryAccountID, fs.lastInput.ApplicationFeeAccountID)
	}
	if fs.lastInput.GrossAmountMinor != 200000 {
		t.Fatalf("gross must be the account balance, got %d", fs.lastInput.GrossAmountMinor)
	}
}

// Source must be a segregated account, never the PRIMARY/default.
func TestBusinessSettlement_RejectsPrimarySource(t *testing.T) {
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 200000, purpose: "PRIMARY"}, &fakeParties{}, pricedFake())
	rec := postBusiness(h, "doa-merchant", doaBody)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PRIMARY source must be 422, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A fee > 0 with no fee destination is a bad request.
func TestBusinessSettlement_FeeWithoutDestination(t *testing.T) {
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 200000}, &fakeParties{}, pricedFake())
	rec := postBusiness(h, "doa-merchant",
		`{"idempotency_key":"k","source_account_id":"wa","beneficiary_banza_name":"@maria","application_fee_bps":500}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("fee without destination must be 400, got %d", rec.Code)
	}
}

// The fee destination must be the caller's OWN business account.
func TestBusinessSettlement_RejectsForeignFeeDestination(t *testing.T) {
	parties := &fakeParties{ownerType: "MERCHANT", ownerID: "someone-else"}
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 200000}, parties, pricedFake())
	rec := postBusiness(h, "doa-merchant", doaBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign fee destination must be 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A beneficiary handle that resolves to no wallet → 422.
func TestBusinessSettlement_BeneficiaryNotFound(t *testing.T) {
	parties := &fakeParties{err: service.ErrNotFound}
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 200000}, parties, pricedFake())
	rec := postBusiness(h, "doa-merchant", doaBody)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unresolvable beneficiary must be 422, got %d", rec.Code)
	}
}

// The operator's fee rate is chosen by the merchant's own category, never by the
// caller's request.
//
// business_category used to be read from the request body and passed to the
// pricing engine, which selects the rate. The body comment was right that a
// caller cannot send a FEE — there is no rate field — but it could send the
// CATEGORY, and the category is what chooses among the operator's rates. With
// one priced category configured and an unknown one costing nothing, omitting it
// was worth the whole fee to the caller.
//
// A struct field cannot be sent, so this asserts the shape rather than the
// behaviour: the request type has no business_category, so there is nothing to
// send and nothing to honour.
func TestApplicationSettlement_CallerCannotChooseItsOwnPricing(t *testing.T) {
	src, err := os.ReadFile("application_settlements.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// The anonymous request struct in Create must not carry it.
	start := strings.Index(body, "var body struct {")
	end := strings.Index(body[start:], "}")
	if start < 0 || end < 0 {
		t.Fatal("could not find the request struct")
	}
	// All three selectors, not just the category.
	//
	// The first version of this test checked business_category alone, and
	// fee_policy_ref survived the cleanup precisely because nothing looked for
	// it — defended as "resolved by the Pricing Engine, never a number", which
	// is how the category was defended too. Each of the three matches a rule,
	// and the engine ranks the more specific rule higher: a rule keyed on a
	// policy reference beats one keyed only on the merchant's profile. Naming
	// any of them is naming the price through one level of indirection.
	//
	// Checked as struct tags, so the explanatory comment inside the struct —
	// which necessarily names the fields it no longer has — cannot satisfy or
	// trip the assertion.
	req := body[start : start+end]
	for _, field := range []string{"business_category", "pricing_profile", "fee_policy_ref"} {
		if strings.Contains(req, `json:"`+field+`"`) {
			t.Errorf("the request body accepts %q — the caller can steer which rate applies to it", field)
		}
	}

	// The value sent onward must be the merchant's assigned POLICY, and the
	// category must not appear in the fee path at all. Reading a category was the
	// second version of this bug, not the fix for the first.
	if !strings.Contains(body, "PricingProfile:         pricingProfile,") &&
		!strings.Contains(body, "PricingProfile: pricingProfile,") {
		t.Error("the settlement does not send the server-resolved pricing profile")
	}
	if strings.Contains(body, "BusinessCategory:") {
		t.Error("the settlement still sends a business category — it has no place in a fee path")
	}
	if strings.Contains(body, "PricingProfile:         body.PricingProfile") {
		t.Error("the settlement sends the caller's pricing profile")
	}
	if strings.Contains(body, "FeePolicyRef:") {
		t.Error("the settlement still forwards a fee policy reference — it selects a rule, so it selects the price")
	}
	if strings.Contains(body, "body.FeePolicyRef") || strings.Contains(body, "body.BusinessCategory") {
		t.Error("the caller's pricing input still reaches the service")
	}
}

// A resolver that prices every merchant under one named policy — enough for the
// handler tests, which are about routing and authorisation rather than rates.
type fakePricing struct {
	profile string
	err     error
}

func (f *fakePricing) PricingProfileForMerchant(context.Context, string) (string, error) {
	return f.profile, f.err
}

// The default for these tests: an owner priced by the explicit Sandbox zero.
func pricedFake() *fakePricing { return &fakePricing{profile: "sandbox-default"} }

// An owner with no assigned policy must be refused, not settled for free. This
// is the invariant the whole pricing change turns on, and it is asserted at the
// handler because that is where a missing policy would otherwise become a
// perfectly ordinary-looking zero.
func TestApplicationSettlement_UnpricedOwnerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pricing PricingProfileResolver
	}{
		{"no resolver at all", nil},
		{"resolver with no assignment", &fakePricing{profile: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSettlements{}
			h := NewApplicationSettlementHandler(fs,
				&fakeWallets{merchantID: "doa-merchant", available: 100000},
				&fakeWalletAccounts{}, &fakeParties{}, tc.pricing)

			rr := postSettlement(t, h, "doa-merchant",
				`{"idempotency_key":"k1","owner_ref":"c1","source_wallet_id":"w-src","beneficiary_wallet_id":"w-ben","gross_amount_minor":1000}`)
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 — body %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "PRICING_NOT_CONFIGURED") {
				t.Errorf("body = %s", rr.Body.String())
			}
			if fs.created != 0 {
				t.Error("an unpriced settlement reached the settlement service")
			}
		})
	}
}
