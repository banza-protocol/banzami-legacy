package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	documents "github.com/banzami/banzami/services/common/documents"
	"github.com/banzami/banzami/services/public-api/internal/apierror"
	"github.com/banzami/banzami/services/public-api/internal/middleware"
	"github.com/banzami/banzami/services/public-api/internal/service"
)

// ReceiptCore is the subset of the core client the receipt handler needs.
type ReceiptCore interface {
	GetTransfer(ctx context.Context, id string) (*service.Transfer, error)
	GetConsumer(ctx context.Context, id string) (*service.ConsumerRecord, error)
}

// pdfGenerator renders ReceiptData to a PDF; swappable in tests.
type pdfGenerator func(context.Context, documents.ReceiptData) ([]byte, error)

// ReceiptHandler serves the official Consumer transfer receipt (PDF) generated
// server-side by the shared Document Engine from real `transfers` data.
type ReceiptHandler struct {
	core ReceiptCore
	gen  pdfGenerator
	// ProofMinter, not the concrete client, so the two outcomes that matter —
	// a proof was established, or it was not — are both reachable in a test.
	// A receipt is only issued in the first case, and that is the whole contract.
	proofs ProofMinter
	env    string // "LIVE" | "SANDBOX" (gateway proof vocabulary)
}

// ProofMinter establishes the durable public proof a receipt is allowed to
// advertise. Narrow on purpose: the receipt path needs exactly one thing from it.
type ProofMinter interface {
	EnsureReference(ctx context.Context, in service.ProofEnsureInput) (string, error)
}

func NewReceiptHandler(core ReceiptCore, proofs ProofMinter, environment string) *ReceiptHandler {
	// A nil *ProofClient stored in an interface is NOT nil. NewProofClient returns
	// nil when the internal proof authority is unconfigured — precisely the
	// deployed defect this change exists to stop — and without this normalisation
	// the required-dependency check would silently pass on a nil client and issue
	// unverifiable receipts again, through a bug that reads as correct Go.
	if pc, ok := proofs.(*service.ProofClient); ok && pc == nil {
		proofs = nil
	}
	env := "LIVE"
	if strings.EqualFold(strings.TrimSpace(environment), "SANDBOX") {
		env = "SANDBOX"
	}
	return &ReceiptHandler{core: core, gen: documents.GeneratePDF, proofs: proofs, env: env}
}

func nameOf(c *service.ConsumerRecord) string {
	if c == nil {
		return ""
	}
	if c.DisplayName != nil && strings.TrimSpace(*c.DisplayName) != "" {
		return *c.DisplayName
	}
	return "@" + c.Handle
}

func handleOf(c *service.ConsumerRecord) string {
	if c == nil {
		return ""
	}
	return c.Handle
}

// GET /v1/consumer/transactions/{id}/receipt.pdf
func (h *ReceiptHandler) ConsumerReceipt(w http.ResponseWriter, r *http.Request) {
	consumer, ok := middleware.GetConsumer(r.Context())
	if !ok {
		apierror.Respond(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id := chi.URLParam(r, "id")
	t, err := h.core.GetTransfer(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrTransferNotFound) {
			apierror.Respond(w, r, http.StatusNotFound, "NOT_FOUND", "transaction not found")
			return
		}
		apierror.Respond(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load transaction")
		return
	}

	// Ownership: the consumer must be a party to the transfer. 404 (not 403) so we
	// never reveal the existence of someone else's transaction.
	if t.SenderID != consumer.ID && t.RecipientID != consumer.ID {
		apierror.Respond(w, r, http.StatusNotFound, "NOT_FOUND", "transaction not found")
		return
	}

	sender, _ := h.core.GetConsumer(r.Context(), t.SenderID)
	recipient, _ := h.core.GetConsumer(r.Context(), t.RecipientID)

	// The proof comes FIRST, and a receipt is only issued if it exists.
	//
	// This used to fall back to a reference derived from the transfer id whenever
	// the gateway was unreachable, on the reasoning that the QR "still renders, it
	// just won't resolve until a proof exists". It never resolved: no proof was
	// ever minted for it, so every such receipt advertised a verification URL that
	// answered "does not exist or may have been forged" forever. A receipt that
	// promises verification it cannot deliver is worse than no receipt — the
	// transfer is complete and the user can ask again in a moment.
	//
	// The error is not swallowed either. A nil client is deterministic
	// misconfiguration; a failing call is a transient outage; both mean no PDF.
	if h.proofs == nil {
		slog.ErrorContext(r.Context(), "receipt refused: proof client not configured")
		apierror.Respond(w, r, http.StatusServiceUnavailable, "RECEIPT_UNAVAILABLE",
			"receipt generation is temporarily unavailable")
		return
	}
	ref, perr := h.proofs.EnsureReference(r.Context(), proofInputFromTransfer(t, sender, recipient, h.env))
	if perr != nil || ref == "" {
		slog.ErrorContext(r.Context(), "receipt refused: could not establish public proof",
			"transfer_id", t.ID, "error", perr)
		apierror.Respond(w, r, http.StatusServiceUnavailable, "RECEIPT_UNAVAILABLE",
			"receipt generation is temporarily unavailable")
		return
	}

	data := buildConsumerReceipt(t, sender, recipient, ref, h.env)

	pdf, err := h.gen(r.Context(), data)
	if err != nil {
		apierror.Respond(w, r, http.StatusServiceUnavailable, "RECEIPT_UNAVAILABLE", "receipt generation is temporarily unavailable")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", documents.Filename(data.Reference)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}

// proofInputFromTransfer builds the gateway ensure-proof payload from a canonical
// transfer. Both parties are consumers (P2P).
func proofInputFromTransfer(t *service.Transfer, sender, recipient *service.ConsumerRecord, env string) service.ProofEnsureInput {
	desc := ""
	if t.Description != nil {
		desc = *t.Description
	}
	confirmed := t.UpdatedAt
	return service.ProofEnsureInput{
		TransactionID:    t.ID,
		TransferID:       t.ID,
		Environment:      env,
		PayerSubjectType: "consumer",
		PayerSubjectID:   t.SenderID,
		PayerDisplayName: nameOf(sender),
		PayerHandle:      handleOf(sender),
		PayeeSubjectType: "consumer",
		PayeeSubjectID:   t.RecipientID,
		PayeeDisplayName: nameOf(recipient),
		PayeeHandle:      handleOf(recipient),
		AmountMinor:      t.Amount.AmountMinor,
		Currency:         t.Currency,
		Status:           t.Status,
		Description:      desc,
		Method:           "Transferência Banzami · @banza",
		LedgerReference:  t.ID,
		ConfirmedAt:      &confirmed,
	}
}

// buildConsumerReceipt maps a real transfer + parties into ReceiptData. Pure.
func buildConsumerReceipt(t *service.Transfer, sender, recipient *service.ConsumerRecord, ref, env string) documents.ReceiptData {
	desc := ""
	if t.Description != nil {
		desc = *t.Description
	}
	return documents.ReceiptData{
		ReceiptID:             t.ID,
		TransactionID:         t.ID,
		Reference:             ref,
		Environment:           env,
		Perspective:           documents.PerspectiveConsumer,
		AmountMinor:           t.Amount.AmountMinor,
		Currency:              t.Currency,
		Status:                t.Status,
		CreatedAt:             t.CreatedAt,
		CompletedAt:           t.UpdatedAt,
		IssuedAt:              t.UpdatedAt,
		PayerName:             nameOf(sender),
		PayerHandle:           handleOf(sender),
		RecipientName:         nameOf(recipient),
		RecipientHandle:       handleOf(recipient),
		PaymentMethod:         "Transferência Banzami · @banza",
		Description:           desc,
		VerificationReference: "banzami.com/r/" + ref,
	}
}
