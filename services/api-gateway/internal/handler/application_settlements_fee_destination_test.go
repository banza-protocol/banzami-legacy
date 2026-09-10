package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/banzami/banzami/services/api-gateway/internal/service"
)

// The fee DESTINATION must not depend on a field the pricing model ignores.
//
// The rate is chosen by the merchant's assigned pricing profile; a caller-supplied
// application_fee_bps is deliberately ignored for pricing, and the handler says so
// in a comment. But the destination was resolved only `if body.ApplicationFeeBps > 0`,
// so an integrator who followed the documented model — name the destination, let the
// operator's profile set the rate — had the fee account silently dropped. Core then
// refused with "application_fee_account_id is required for this category", naming a
// field the public contract never asks for.
//
// This is the deployed DOA failure: @doa named, no bps sent, settlement impossible.
func TestApplicationSettlement_FeeDestinationResolvedWithoutCallerBps(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"},
		&fakeWalletAccounts{balance: 100000}, &fakeParties{}, pricedFake())

	// No application_fee_bps at all — exactly what the documented model produces.
	body := `{"idempotency_key":"idem-1","source_account_id":"acct-campaign",
	          "beneficiary_banza_name":"maria","fee_destination_banza_name":"doa",
	          "reference_id":"ref-1"}`
	rec := postBusiness(h, "doa-merchant", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.lastInput.ApplicationFeeAccountID != "acct-doa" {
		t.Fatalf("fee destination was named but not resolved: fee_account=%q",
			fs.lastInput.ApplicationFeeAccountID)
	}
}

// Naming no destination stays legal: a profile that charges nothing needs none.
func TestApplicationSettlement_NoDestinationNamedStillSettles(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"},
		&fakeWalletAccounts{balance: 100000}, &fakeParties{}, pricedFake())
	body := `{"idempotency_key":"idem-2","source_account_id":"acct-campaign",
	          "beneficiary_banza_name":"maria","reference_id":"ref-2"}`
	rec := postBusiness(h, "doa-merchant", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.lastInput.ApplicationFeeAccountID != "" {
		t.Fatalf("no destination was named; none should be resolved, got %q",
			fs.lastInput.ApplicationFeeAccountID)
	}
}

// A destination that is not the caller's own account is still refused. Resolving
// the destination more often must not resolve it more loosely.
func TestApplicationSettlement_ForeignFeeDestinationStillRefused(t *testing.T) {
	// Owned by somebody else, and named WITHOUT bps — the path this change widened.
	parties := &fakeParties{ownerType: "MERCHANT", ownerID: "someone-else"}
	h := NewApplicationSettlementHandler(&fakeSettlements{}, &fakeWallets{merchantID: "doa-merchant"},
		&fakeWalletAccounts{balance: 100000}, parties, pricedFake())
	body := `{"idempotency_key":"idem-3","source_account_id":"acct-campaign",
	          "beneficiary_banza_name":"maria","fee_destination_banza_name":"stranger",
	          "reference_id":"ref-3"}`
	rec := postBusiness(h, "doa-merchant", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a stranger's account must not take this caller's fee: got %d (%s)",
			rec.Code, rec.Body.String())
	}
}

// rejectingSettlements stands in for core refusing the REQUEST rather than failing.
type rejectingSettlements struct{ err error }

func (r *rejectingSettlements) Create(ctx context.Context, in service.CreateApplicationSettlementInput) (*service.ApplicationSettlement, error) {
	return nil, r.err
}
func (r *rejectingSettlements) Complete(ctx context.Context, id string) (*service.ApplicationSettlement, error) {
	return nil, r.err
}
func (r *rejectingSettlements) Get(ctx context.Context, id string) (*service.ApplicationSettlement, error) {
	return nil, r.err
}

// A deliberate core 4xx must reach the caller as a 4xx carrying its reason.
//
// It was answered with 502 UPSTREAM_ERROR "could not create settlement", which
// tells an integrator that Banzami broke — when in fact core had named the exact
// thing to fix. Worse, a 502 on this path reached the browser with no usable body
// at all, so the deployed symptom was an opaque network failure.
func TestApplicationSettlement_CoreClientErrorKeepsItsStatusAndReason(t *testing.T) {
	core := &service.CoreError{Status: http.StatusBadRequest, Code: "BAD_REQUEST",
		Message: "application_fee_account_id is required for this category"}
	h := NewApplicationSettlementHandler(&rejectingSettlements{err: core},
		&fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 100000},
		&fakeParties{}, pricedFake())
	body := `{"idempotency_key":"idem-4","source_account_id":"acct-campaign",
	          "beneficiary_banza_name":"maria","reference_id":"ref-4"}`
	rec := postBusiness(h, "doa-merchant", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a core 400 must stay a 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "application_fee_account_id") {
		t.Fatalf("the actionable reason must reach the caller, got %s", rec.Body.String())
	}
}

// A core 5xx or a transport failure is NOT the caller's fault and must stay 502.
func TestApplicationSettlement_CoreServerErrorStaysBadGateway(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"core 5xx", &service.CoreError{Status: http.StatusInternalServerError}},
		{"transport", &service.TransportError{Err: errors.New("connection refused")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewApplicationSettlementHandler(&rejectingSettlements{err: tc.err},
				&fakeWallets{merchantID: "doa-merchant"}, &fakeWalletAccounts{balance: 100000},
				&fakeParties{}, pricedFake())
			body := `{"idempotency_key":"idem-5","source_account_id":"acct-campaign",
			          "beneficiary_banza_name":"maria","reference_id":"ref-5"}`
			rec := postBusiness(h, "doa-merchant", body)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("want 502, got %d", rec.Code)
			}
			// The caller must not be told its own request was bad.
			if strings.Contains(rec.Body.String(), "connection refused") {
				t.Fatalf("upstream detail leaked: %s", rec.Body.String())
			}
		})
	}
}

// The rate belongs to the operator, not to the caller.
//
// core treats a non-zero application_fee_bps as the APP-DEFINED path and does
// not consult the Pricing Engine at all. The handler forwarded the caller's
// number while its own comment claimed the field was ignored, so every existing
// integration — all of which still send it, because the old contract asked them
// to — was choosing its own rate up to the 50% domain maximum, and the pricing
// profile the handler resolves was dead code for exactly those requests.
func TestApplicationSettlement_CallerSuppliedRateNeverReachesPricing(t *testing.T) {
	for _, bps := range []int{500, 4999, 1} {
		fs := &fakeSettlements{}
		h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"},
			&fakeWalletAccounts{balance: 100000}, &fakeParties{}, pricedFake())
		body := `{"idempotency_key":"idem-bps","source_account_id":"acct-campaign",
		          "beneficiary_banza_name":"maria","fee_destination_banza_name":"doa",
		          "application_fee_bps":` + strconv.Itoa(bps) + `,"reference_id":"ref-bps"}`
		rec := postBusiness(h, "doa-merchant", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("bps=%d: want 201, got %d (%s)", bps, rec.Code, rec.Body.String())
		}
		if fs.lastInput.ApplicationFeeBps != 0 {
			t.Fatalf("bps=%d reached core as %d — the caller set the operator's rate",
				bps, fs.lastInput.ApplicationFeeBps)
		}
		// The operator's own policy must still be the thing that prices it.
		if fs.lastInput.PricingProfile == "" {
			t.Fatalf("bps=%d: the merchant's pricing profile was not applied", bps)
		}
	}
}

// Sending the retired field must not become an error. An integration written
// against the old contract keeps working; it simply no longer decides the price.
func TestApplicationSettlement_RetiredRateFieldIsAcceptedNotRefused(t *testing.T) {
	fs := &fakeSettlements{}
	h := NewApplicationSettlementHandler(fs, &fakeWallets{merchantID: "doa-merchant"},
		&fakeWalletAccounts{balance: 100000}, &fakeParties{}, pricedFake())
	body := `{"idempotency_key":"idem-legacy","source_account_id":"acct-campaign",
	          "beneficiary_banza_name":"maria","fee_destination_banza_name":"doa",
	          "application_fee_bps":500,"reference_id":"ref-legacy"}`
	if rec := postBusiness(h, "doa-merchant", body); rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if fs.lastInput.ApplicationFeeAccountID != "acct-doa" {
		t.Fatal("the named fee destination must still be resolved")
	}
}
