// The proofs that should already have existed.
//
// Four retired receipt generators printed "BZM-" + the first eight hex symbols
// of an object id and minted nothing behind it. Those PDFs are in people's
// hands. Issuing those records a fresh SECURE_V1 reference would leave the
// printed one dead forever, so the historical path must reconstruct the exact
// legacy reference — and, because a chosen public reference is a capability, it
// must derive that reference itself rather than accept one.
//
// Skipped when DATABASE_URL is unset, matching the other DB-backed proof tests.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const histTransferID = "f99338e2-b4e5-4309-ba3e-d0a376ed94b5" // the reproducer
const histExpectedRef = "BZM-F993-38E2"

func histInput(txn, environment string) ProofInput {
	in := proofInput(txn)
	in.Environment = environment
	in.PayerHandle = "oxfannio"
	in.PayeeHandle = "fm65"
	in.LedgerReference = txn
	return in
}

// The whole point: the reference on the document resolves.
func TestEnsureHistorical_ReproducesTheReferenceAlreadyPrinted(t *testing.T) {
	pool, svc := proofFixture(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM transaction_proofs WHERE transaction_id=$1`, histTransferID)
	})

	p, err := svc.EnsureHistorical(ctx, histTransferID, histInput(histTransferID, "SANDBOX"))
	if err != nil {
		t.Fatalf("EnsureHistorical: %v", err)
	}
	if p.ProofReference != histExpectedRef {
		t.Fatalf("reference %q, want %q — the PDF in the user's hands prints the latter",
			p.ProofReference, histExpectedRef)
	}
	if ClassifyReference(p.ProofReference) != ReferenceLegacyV0 {
		t.Fatal("the reconstructed reference must classify as LEGACY_HEX_V0")
	}
	// And it must be reachable by the public lookup, which is what was broken.
	got, err := svc.GetByReference(ctx, histExpectedRef)
	if err != nil {
		t.Fatalf("the backfilled proof does not resolve: %v", err)
	}
	if got.TransactionID != histTransferID {
		t.Fatalf("resolved to %q, want %q", got.TransactionID, histTransferID)
	}
}

// A backfilled proof is signed like any other. An unsigned row would look
// materialised while carrying no operator attestation at all.
func TestEnsureHistorical_IsSignedLikeAnyOtherProof(t *testing.T) {
	pool, svc := proofFixture(t)
	ctx := context.Background()
	txn := "aaaa1111-2222-4333-8444-555566667777"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM transaction_proofs WHERE transaction_id=$1`, txn) })

	p, err := svc.EnsureHistorical(ctx, txn, histInput(txn, "SANDBOX"))
	if err != nil {
		t.Fatalf("EnsureHistorical: %v", err)
	}
	if p.ProofHash == "" || p.SignatureKeyID == "" || p.SignatureAlg == "" {
		t.Fatalf("proof is not attested: hash=%q key=%q alg=%q", p.ProofHash, p.SignatureKeyID, p.SignatureAlg)
	}
	var sig string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(signature_value,'') FROM transaction_proofs WHERE transaction_id=$1`, txn,
	).Scan(&sig); err != nil {
		t.Fatal(err)
	}
	if sig == "" {
		t.Fatal("signature_value is empty — the proof carries no signature")
	}
}

// Re-running the backfill must not mint a second proof or change the reference.
func TestEnsureHistorical_IsIdempotent(t *testing.T) {
	pool, svc := proofFixture(t)
	ctx := context.Background()
	txn := "bbbb1111-2222-4333-8444-555566667777"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM transaction_proofs WHERE transaction_id=$1`, txn) })

	first, err := svc.EnsureHistorical(ctx, txn, histInput(txn, "SANDBOX"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.EnsureHistorical(ctx, txn, histInput(txn, "SANDBOX"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ProofReference != second.ProofReference {
		t.Fatalf("a second run produced a different proof: %s/%s vs %s/%s",
			first.ID, first.ProofReference, second.ID, second.ProofReference)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transaction_proofs WHERE transaction_id=$1`, txn).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d proofs for one record", n)
	}
}

// LEGACY_HEX_V0 is ~32 bits and the public lookup refuses it outside the
// Sandbox, so a LIVE row of that shape would be unreadable by construction.
func TestEnsureHistorical_RefusesOutsideSandbox(t *testing.T) {
	_, svc := proofFixture(t)
	for _, environment := range []string{"LIVE", "", "PRODUCTION"} {
		_, err := svc.EnsureHistorical(context.Background(),
			"cccc1111-2222-4333-8444-555566667777", histInput("cccc-live", environment))
		if !errors.Is(err, ErrHistoricalNotSandbox) {
			t.Fatalf("environment %q: want ErrHistoricalNotSandbox, got %v", environment, err)
		}
	}
}

// Two records deriving the same 32-bit reference cannot both own it. The second
// must be reported, never silently pointed at the first record's proof — that
// would make one document verify as a different payment.
func TestEnsureHistorical_RefusesToStealAnAlreadyHeldReference(t *testing.T) {
	pool, svc := proofFixture(t)
	ctx := context.Background()
	// Same first 32 bits, different records.
	a := "dddd2222-1111-4333-8444-555566667777"
	b := "dddd2222-9999-4333-8444-000011112222"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM transaction_proofs WHERE transaction_id = ANY($1)`, []string{a, b})
	})

	if _, err := svc.EnsureHistorical(ctx, a, histInput(a, "SANDBOX")); err != nil {
		t.Fatal(err)
	}
	_, err := svc.EnsureHistorical(ctx, b, histInput(b, "SANDBOX"))
	if !errors.Is(err, ErrHistoricalReferenceTaken) {
		t.Fatalf("want ErrHistoricalReferenceTaken, got %v", err)
	}
	if !strings.Contains(err.Error(), "BZM-DDDD-2222") {
		t.Fatalf("the error should name the contested reference, got %q", err)
	}
}

// The reference is derived, never supplied. This is the property that keeps the
// historical path from being a way to mint a chosen public capability.
func TestLegacyReference_IsDerivedFromTheObjectId(t *testing.T) {
	if got := legacyReference(histTransferID); got != histExpectedRef {
		t.Fatalf("legacyReference = %q, want %q", got, histExpectedRef)
	}
	// Lower case in, canonical out: the reference has exactly one spelling.
	if got := legacyReference(strings.ToLower(histTransferID)); got != histExpectedRef {
		t.Fatalf("lower-case id derived %q", got)
	}
	if got := legacyReference("abc"); got != "" {
		t.Fatalf("too short an id must derive nothing, got %q", got)
	}
}
