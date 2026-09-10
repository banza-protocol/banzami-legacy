package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/banzami/banzami/services/api-gateway/internal/apierror"
	"github.com/banzami/banzami/services/api-gateway/internal/middleware"
	"github.com/banzami/banzami/services/api-gateway/internal/service"
)

// BusinessMeHandler serves the authenticated Business account its own
// consolidated resolution — identity, KYB, category/pricing, wallet and
// settlement readiness (with machine-readable blockers) — so an integrating
// application (e.g. DOA) renders an accurate "Integration Health" surface
// instead of guessing from local env vars.
//
// Self-scoped: the merchant id comes from the token, never from path/query.
// Not a handle-enumeration oracle; returns only non-secret fields.
type BusinessMeHandler struct {
	svc *service.BusinessSelfService
}

func NewBusinessMeHandler(svc *service.BusinessSelfService) *BusinessMeHandler {
	return &BusinessMeHandler{svc: svc}
}

// resolveSelfAuthority answers "which Business account is asking about itself?".
//
// This route exists for an integrating application's Integration Health view —
// DOA is the named example in the comment above — and for most of its life a
// Developer Platform key could not reach it: it read only the merchant JWT, so
// the application it was built for got 401. The account is still never taken
// from the request; a project key's comes from its binding, a merchant JWT
// names itself.
func (h *BusinessMeHandler) resolveSelfAuthority(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if dp, isDev := middleware.GetDeveloperPrincipal(r.Context()); isDev {
		if !dp.HasScope("identity:read") {
			apierror.Respond(w, r, http.StatusForbidden, "INSUFFICIENT_SCOPE",
				"missing required scope: identity:read")
			return "", "", false
		}
		// Without a binding there is no Business account to describe. Answered as
		// a provisioning state rather than a 404, which would suggest the account
		// is missing rather than unlinked.
		if !dp.Bound || dp.MerchantID == "" {
			apierror.Respond(w, r, http.StatusForbidden, "PAYMENTS_UNAVAILABLE",
				"this project is not provisioned to hold funds")
			return "", "", false
		}
		return dp.MerchantID, dp.Environment, true
	}
	principal, ok := middleware.GetPrincipal(r.Context())
	if !ok || principal.MerchantID == "" {
		apierror.Respond(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "valid business credentials required")
		return "", "", false
	}
	return principal.MerchantID, principal.Environment, true
}

// GET /v1/integration
func (h *BusinessMeHandler) Me(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		apierror.Respond(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "business profile is not available")
		return
	}

	merchantID, environment, ok := h.resolveSelfAuthority(w, r)
	if !ok {
		return
	}

	env := "LIVE"
	if strings.EqualFold(strings.TrimSpace(environment), "SANDBOX") {
		env = "SANDBOX"
	}

	slog.InfoContext(r.Context(), "business_resolution_started",
		"merchant_id", merchantID, "environment", env)

	// A fee destination is configuration the integration supplies, not authority.
	// Asked about here so an operator learns whether it can receive the fee —
	// and which condition fails if it cannot.
	res, err := h.svc.Self(r.Context(), merchantID, env, r.URL.Query().Get("fee_destination"))
	if err != nil {
		if errors.Is(err, service.ErrMerchantNotFound) {
			slog.WarnContext(r.Context(), "business_resolution_failed",
				"merchant_id", merchantID, "reason", "not_found")
			apierror.Respond(w, r, http.StatusNotFound, "NOT_FOUND", "business account not found")
			return
		}
		slog.ErrorContext(r.Context(), "business_resolution_failed",
			"merchant_id", merchantID, "error", err.Error())
		apierror.Respond(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load business profile")
		return
	}

	if res.SettlementReady {
		slog.InfoContext(r.Context(), "business_resolution_success",
			"merchant_id", res.MerchantID, "handle", res.Handle, "kyb", res.KybStatus)
	} else {
		slog.InfoContext(r.Context(), "business_resolution_blocked",
			"merchant_id", res.MerchantID, "handle", res.Handle, "blockers", strings.Join(res.Blockers, ","))
	}

	// Empty slices must serialise as [] not null.
	blockers := res.Blockers
	if blockers == nil {
		blockers = []string{}
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}

	var category any
	if res.CategoryLabel != "" {
		category = res.CategoryLabel
	}
	var subcategory any
	if res.Subcategory != "" {
		subcategory = res.Subcategory
	}

	body := map[string]any{
		"environment":           env,
		"id":                    res.MerchantID,
		"handle":                res.Handle,
		"business_name":         res.BusinessName,
		"business_account_type": res.BusinessAccountType,
		"status":                res.Status,
		"kyb_status":            res.KybStatus,
		"verified":              res.Verified,

		// Flat fields (back-compat with earlier clients).
		"category":         category,
		"wallet_ready":     res.WalletReady,
		"settlement_ready": res.SettlementReady,
		"subcategory":      subcategory,

		// What this account is actually charged, per fee-bearing operation,
		// under the policy an operator assigned it.
		//
		// `pricing_category` and a single `fee_bps` used to be reported here.
		// Both described a model where the category a merchant typed chose the
		// rate, and a single number could stand for "the fee". Neither is true:
		// nothing is priced by category, and settlement and payout are priced
		// separately.
		"pricing": map[string]any{
			"profile":    res.PricingProfile,
			"operations": pricingOperations(res.PricingRules),
			"found":      res.PricingFound,
		},
		"wallet": map[string]any{
			"ready":                  res.WalletReady,
			"wallet_id":              res.WalletID,
			"currency":               res.WalletCurrency,
			"status":                 res.WalletStatus,
			"primary_account_id":     res.PrimaryAccountID,
			"application_account_id": res.ApplicationAccountID,
		},
		"settlement": map[string]any{
			"ready":    res.SettlementReady,
			"enabled":  true,
			"blockers": blockers,
		},
		"blockers":   blockers,
		"warnings":   warnings,
		"checked_at": time.Now().UTC().Format(time.RFC3339),
	}

	// Reported beside the account's own readiness, never merged into it. A
	// settlement that charges no fee needs no destination at all, so an unready
	// one is configuration to fix, not a blocker on the Business.
	if res.FeeDestination != nil {
		body["fee_destination"] = res.FeeDestination
	}

	// Which Project this readiness describes.
	//
	// The resource answers "can my integration settle, and what is missing" and
	// said nothing about what "my" referred to. A caller holding one key knows
	// implicitly; a caller holding several, or storing the answer beside its own
	// records, had nothing stable to file it under. Present only for a project
	// credential: a merchant JWT is not a Project and inventing an id for it
	// would make one up.
	if dp, isDev := middleware.GetDeveloperPrincipal(r.Context()); isDev {
		body["project"] = map[string]any{
			"id":   PublicProjectID(dp.ProjectID),
			"slug": dp.ProjectSlug,
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// pricingOperations renders the assigned rates as a stable list. Always a list,
// never null: an account with no priced policy has zero operations, which is a
// different and visible fact from the field being absent.
func pricingOperations(rules []service.OperationRate) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]any{
			"operation": r.Operation,
			"rate_bps":  r.RateBps,
			"rule_key":  r.RuleKey,
		})
	}
	return out
}
