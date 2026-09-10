package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Transaction Proof service (BANZA ADR-023). Materializes an immutable, publicly
// verifiable proof for a transaction. The receipt is not the proof — this is. The
// proof_reference is random/non-enumerable; generation is idempotent (one per
// transaction). Reversals move status to REVERSED, never delete.
type ProofService struct {
	pool       *pgxpool.Pool
	signingKey []byte
	keyID      string
	operatorID string
	network    string
	publicBase string // e.g. https://banzami.com/r/
	// newReference mints a candidate public reference. A field so a test can force
	// the collision path deterministically instead of hoping for a 1-in-2^40 event.
	newReference func() (string, error)
}

// A public reference collides with an already-issued one only by accident, but
// "rare" is not "handled": the insert must then mint a DIFFERENT one, never reuse
// or overwrite the reference somebody else's receipt already prints. Bounded, so
// a systematically broken generator fails closed instead of spinning.
const maxReferenceAttempts = 5

// proofReferenceCollision reports a unique violation on the PUBLIC REFERENCE
// specifically. The (transaction_id, environment) index is handled by ON CONFLICT
// and must never be retried — that one means another caller already minted this
// transaction's proof, and the answer is to read theirs, not to mint a second.
func proofReferenceCollision(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23505" {
		return false
	}
	return strings.Contains(pg.ConstraintName, "proof_reference")
}

var ErrProofNotFound = errors.New("transaction proof not found")

func NewProofService(pool *pgxpool.Pool, signingKey, keyID, operatorID, network, publicBase string) *ProofService {
	if keyID == "" {
		keyID = "op-hmac-v1"
	}
	if operatorID == "" {
		operatorID = "banzami"
	}
	if network == "" {
		network = "banza"
	}
	if publicBase == "" {
		publicBase = "https://banzami.com/r/"
	}
	return &ProofService{
		pool: pool, signingKey: []byte(signingKey), keyID: keyID,
		operatorID: operatorID, network: network, publicBase: publicBase,
		newReference: secureReference,
	}
}

// ProofInput is the operator-side data used to materialize a proof. It comes from
// a real, confirmed transaction (wallet payment / transfer).
type ProofInput struct {
	TransactionID    string
	TransferID       string
	PaymentIntentID  string
	Environment      string
	PayerSubjectType string
	PayerSubjectID   string
	PayerDisplayName string
	PayerHandle      string
	PayeeSubjectType string
	PayeeSubjectID   string
	PayeeDisplayName string
	PayeeHandle      string
	AmountMinor      int64
	Currency         string
	Status           string
	Description      string
	Method           string
	LedgerReference  string
	ConfirmedAt      *time.Time
}

type Proof struct {
	ID                string
	ProofReference    string
	TransactionID     string
	Environment       string
	PayerDisplayName  string
	PayerHandle       string
	PayerSubjectType  string
	PayeeDisplayName  string
	PayeeHandle       string
	PayeeSubjectType  string
	AmountMinor       int64
	Currency          string
	Status            string
	Description       string
	Method            string
	ProofHash         string
	SignatureKeyID    string
	SignatureAlg      string
	VerificationCount int
	IssuedAt          time.Time
	ConfirmedAt       *time.Time
	ReversedAt        *time.Time
}

// The public reference alphabet: Crockford-ish base32 with I, L, O and U removed,
// so a reference read aloud or copied off paper cannot become a different one.
const refAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// SECURE_V1 is 24 symbols in six groups: 24 x log2(32) = 120 bits.
//
// The previous format was 8 symbols — 40 bits. That is ample against a casual
// guess and inadequate against a patient one. A public proof reference is a
// BEARER capability: whoever holds it learns the amount, both @handles and the
// description. At LIVE scale (~1e5 live proofs) a distributed prober working
// within the anonymous rate ceiling lands roughly 8 valid references a day
// against 40 bits; against 120 bits the same effort returns nothing in any
// human timeframe. This repository already treats bearer material this way —
// webhook signing secrets are 256 bits — so 40 bits was the outlier.
//
// Six groups of four keeps it copy-, dictate- and QR-friendly, and makes the
// generation structurally obvious: 8 symbols is legacy, 24 is current.
const (
	secureRefGroups      = 6
	secureRefGroupSize   = 4
	secureRefSymbols     = secureRefGroups * secureRefGroupSize // 24
	SecureRefEntropyBits = secureRefSymbols * 5                 // 120
)

// secureReference returns a random, non-enumerable SECURE_V1 public reference,
// BZM-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX.
//
// One random byte per symbol, reduced modulo 32. 256 is an exact multiple of 32,
// so the reduction is bias-free — every symbol is uniform over the alphabet, and
// the 120-bit figure above is real rather than nominal.
func secureReference() (string, error) {
	b := make([]byte, secureRefSymbols)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, secureRefSymbols)
	for i, c := range b {
		out[i] = refAlphabet[int(c)%len(refAlphabet)]
	}
	groups := make([]string, 0, secureRefGroups)
	for g := 0; g < secureRefGroups; g++ {
		groups = append(groups, string(out[g*secureRefGroupSize:(g+1)*secureRefGroupSize]))
	}
	return "BZM-" + strings.Join(groups, "-"), nil
}

// ReferenceVersion classifies a public proof reference. The two generations are
// structurally distinguishable by length alone, so no stored column is needed.
type ReferenceVersion int

const (
	ReferenceInvalid ReferenceVersion = iota
	// ReferenceLegacyV0 is BZM-XXXX-XXXX: 8 symbols derived from the first 32 bits
	// of an object UUID by a receipt generator that no longer exists. Compatibility
	// only, for artifacts already in people's hands, and Sandbox only.
	ReferenceLegacyV0
	// ReferenceSecureV1 is BZM + six groups of four: 120 random bits.
	ReferenceSecureV1
)

var (
	legacyRefPattern = regexp.MustCompile(`^BZM(?:-[0-9A-F]{4}){2}$`)
	secureRefPattern = regexp.MustCompile(`^BZM(?:-[0-9A-HJKMNP-TV-Z]{4}){6}$`)
)

// ClassifyReference is the ONE parser. Every service and surface that needs to
// know what kind of reference it is holding calls this, so a second, subtly
// different regex cannot drift into existence somewhere else.
//
// Legacy is hex-only because that is what a UUID prefix can produce; the secure
// alphabet is wider. Nothing is lower-cased or stripped here: the canonical form
// is upper-case and hyphenated, and quietly accepting other spellings would mean
// one proof had several spellings, which is exactly how a rate limit keyed on the
// reference gets bypassed.
func ClassifyReference(ref string) ReferenceVersion {
	switch {
	case secureRefPattern.MatchString(ref):
		return ReferenceSecureV1
	case legacyRefPattern.MatchString(ref):
		return ReferenceLegacyV0
	default:
		return ReferenceInvalid
	}
}

// canonicalPayload is the deterministic, ordered byte string that is hashed and
// signed. Field order is fixed by ADR-040.
func canonicalPayload(ref string, in ProofInput) []byte {
	confirmed := ""
	if in.ConfirmedAt != nil {
		confirmed = in.ConfirmedAt.UTC().Format(time.RFC3339)
	}
	// Ordered map via a slice of pairs serialized as a JSON array of [k,v] — stable.
	pairs := [][2]string{
		{"proof_reference", ref},
		{"transaction_reference", in.TransactionID},
		{"amount_minor", fmt.Sprintf("%d", in.AmountMinor)},
		{"currency", in.Currency},
		{"payer_handle", in.PayerHandle},
		{"payee_handle", in.PayeeHandle},
		{"status", in.Status},
		{"confirmed_at", confirmed},
		{"ledger_reference", in.LedgerReference},
	}
	raw, _ := json.Marshal(pairs)
	return raw
}

func (s *ProofService) hashAndSign(ref string, in ProofInput) (proofHash, sig, alg string) {
	payload := canonicalPayload(ref, in)
	sum := sha256.Sum256(payload)
	proofHash = hex.EncodeToString(sum[:])
	alg = "HMAC-SHA256"
	mac := hmac.New(sha256.New, s.signingKey)
	mac.Write(payload)
	sig = hex.EncodeToString(mac.Sum(nil))
	return
}

// normalizeProofStatus maps any caller transaction status onto the canonical
// proof status set enforced by the transaction_proofs CHECK constraint. Unknown
// or empty values resolve to CONFIRMED — a proof only exists for a real, posted
// transaction, so the safe default is "confirmed", never a failure state.
func normalizeProofStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "PENDING", "AUTHORIZED", "PROCESSING":
		return "PENDING"
	case "FAILED", "DECLINED", "ERROR":
		return "FAILED"
	case "REVERSED", "REFUNDED", "CHARGEBACK":
		return "REVERSED"
	case "CANCELLED", "CANCELED", "VOIDED":
		return "CANCELLED"
	case "EXPIRED":
		return "EXPIRED"
	default: // COMPLETED, CONFIRMED, CAPTURED, SUCCEEDED, SETTLED, "" …
		return "CONFIRMED"
	}
}

// statusIsTerminalNegative reports whether a normalized proof status means the
// transaction did not stand, so the public verification page must not show the
// green "verified" state. Ensure only ever moves a proof INTO this set, never out,
// so a reversal can never be silently downgraded back to CONFIRMED.
func statusIsTerminalNegative(s string) bool {
	switch s {
	case "REVERSED", "CANCELLED", "FAILED", "EXPIRED":
		return true
	}
	return false
}

// updateStatus changes a materialized proof's status, stamping reversed_at the
// first time it becomes REVERSED. The proof row is never deleted.
func (s *ProofService) updateStatus(ctx context.Context, id, status string) (*Proof, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE transaction_proofs
		   SET status = $2,
		       reversed_at = CASE WHEN $2 = 'REVERSED' AND reversed_at IS NULL THEN now() ELSE reversed_at END,
		       updated_at = now()
		 WHERE id = $1`, id, status); err != nil {
		return nil, err
	}
	return scanProof(s.pool.QueryRow(ctx, `SELECT `+proofCols+` FROM transaction_proofs WHERE id=$1`, id))
}

// MarkReversed moves a transaction's proof to REVERSED (idempotent; never deletes;
// preserves verification_count). Call it from the reversal / refund-completed /
// dispute-resolved flow. It is a no-op when no proof exists yet — Ensure will then
// materialize the proof as REVERSED the next time a receipt is requested with the
// reversed transaction status.
func (s *ProofService) MarkReversed(ctx context.Context, transactionID, environment string) error {
	if environment == "" {
		environment = "LIVE"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE transaction_proofs
		   SET status = 'REVERSED',
		       reversed_at = COALESCE(reversed_at, now()),
		       updated_at = now()
		 WHERE transaction_id = $1 AND environment = $2 AND status <> 'REVERSED'`,
		transactionID, environment)
	return err
}

// Ensure idempotently returns the proof for a transaction, creating it on first
// call. Concurrent callers converge on a single proof (unique on transaction_id).
// A subsequent call with a terminal-negative status (e.g. REVERSED) updates the
// stored proof forward — see the status re-sync in the body.
func (s *ProofService) Ensure(ctx context.Context, in ProofInput) (*Proof, error) {
	mint := s.newReference
	if mint == nil {
		mint = secureReference
	}
	return s.ensureWith(ctx, in, mint, maxReferenceAttempts)
}

// legacyReference reconstructs the reference the retired receipt generators
// printed: "BZM-" + the first eight hex symbols of the object id, in two groups.
//
// It is here, next to the current generator, because this file is the one place
// allowed to construct a BZM- value — a guard that exists precisely because four
// other places once did. Nothing new is ever minted this way; see
// EnsureHistorical for the only caller and the reason it must exist.
func legacyReference(objectID string) string {
	hex := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(objectID), "-", ""))
	if len(hex) < 8 {
		return ""
	}
	return "BZM-" + hex[0:4] + "-" + hex[4:8]
}

// ErrHistoricalNotSandbox refuses to materialise a legacy-referenced proof
// outside the Sandbox.
var ErrHistoricalNotSandbox = errors.New("historical proof materialisation is Sandbox-only")

// ErrHistoricalReferenceTaken means the legacy reference this record must carry
// already belongs to a different proof.
var ErrHistoricalReferenceTaken = errors.New("the legacy reference for this record is already held by another proof")

// EnsureHistorical materialises the proof that should have existed for a record
// predating proof minting.
//
// Between the receipt feature shipping and the proof service being wired, four
// receipt generators printed a reference derived from the object id and never
// minted anything behind it. Those PDFs are in people's hands and their QR codes
// resolve to "does not exist or may have been forged" — BZM-F993-38E2 is one.
// Reconstructing the proof from the ledger is the only way those documents ever
// become verifiable again; issuing them a fresh SECURE_V1 reference would leave
// the printed one dead forever.
//
// Two constraints make this narrow enough to be safe:
//
//   - The reference is DERIVED from the source object id, never supplied. A
//     caller that could choose a public reference could mint one that collides
//     with — or impersonates — a proof that already exists.
//   - Sandbox only. A LEGACY_HEX_V0 reference carries ~32 bits and is refused by
//     the public lookup outside the Sandbox anyway, so writing one as LIVE would
//     create a row nothing can read.
//
// There is no retry: a legacy reference has exactly one possible value, so a
// collision is a fact to report, not a dice roll to re-throw.
func (s *ProofService) EnsureHistorical(ctx context.Context, sourceID string, in ProofInput) (*Proof, error) {
	if !strings.EqualFold(strings.TrimSpace(in.Environment), "SANDBOX") {
		return nil, ErrHistoricalNotSandbox
	}
	ref := legacyReference(sourceID)
	if ClassifyReference(ref) != ReferenceLegacyV0 {
		return nil, fmt.Errorf("%q does not derive a legacy reference", sourceID)
	}
	p, err := s.ensureWith(ctx, in, func() (string, error) { return ref, nil }, 1)
	if err != nil && proofReferenceCollision(err) {
		return nil, fmt.Errorf("%w: %s", ErrHistoricalReferenceTaken, ref)
	}
	return p, err
}

func (s *ProofService) ensureWith(
	ctx context.Context,
	in ProofInput,
	mint func() (string, error),
	maxAttempts int,
) (*Proof, error) {
	if in.Environment == "" {
		in.Environment = "LIVE"
	}
	// Normalize to the proof status vocabulary (the transaction_proofs CHECK):
	// callers pass their own transaction status (e.g. a transfer's "COMPLETED"),
	// which must map onto {PENDING,CONFIRMED,FAILED,REVERSED,CANCELLED,EXPIRED}.
	in.Status = normalizeProofStatus(in.Status)
	if existing, err := s.getByTxn(ctx, in.TransactionID, in.Environment); err == nil {
		// A proof is minted lazily from the then-current transaction status, but the
		// transaction may later be reversed/cancelled. Move a materialized proof
		// FORWARD into a terminal-negative status (never back to CONFIRMED) so the
		// public page stops showing "verified"; reversed_at is stamped on REVERSED.
		if in.Status != existing.Status && statusIsTerminalNegative(in.Status) {
			return s.updateStatus(ctx, existing.ID, in.Status)
		}
		return existing, nil
	} else if !errors.Is(err, ErrProofNotFound) {
		return nil, err
	}

	insert := `
		INSERT INTO transaction_proofs
		  (proof_reference, transaction_id, transfer_id, payment_intent_id, environment,
		   payer_subject_type, payer_subject_id, payer_display_name, payer_handle,
		   payee_subject_type, payee_subject_id, payee_display_name, payee_handle,
		   amount_minor, currency, status, description, method, ledger_reference,
		   proof_hash, signature_key_id, signature_algorithm, signature_value, confirmed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		ON CONFLICT (transaction_id, environment) DO NOTHING`

	for attempt := 1; ; attempt++ {
		ref, rerr := mint()
		if rerr != nil {
			return nil, rerr
		}
		proofHash, sig, alg := s.hashAndSign(ref, in)
		_, err := s.pool.Exec(ctx, insert,
			ref, in.TransactionID, nz(in.TransferID), nz(in.PaymentIntentID), in.Environment,
			nz(in.PayerSubjectType), nz(in.PayerSubjectID), nz(in.PayerDisplayName), nz(in.PayerHandle),
			nz(in.PayeeSubjectType), nz(in.PayeeSubjectID), nz(in.PayeeDisplayName), nz(in.PayeeHandle),
			in.AmountMinor, in.Currency, in.Status, nz(in.Description), nz(in.Method), nz(in.LedgerReference),
			proofHash, s.keyID, alg, sig, in.ConfirmedAt)
		if err == nil {
			break
		}
		if !proofReferenceCollision(err) {
			return nil, err
		}
		if attempt >= maxAttempts {
			if maxAttempts == 1 {
				// The caller's reference is fixed, so there is nothing to re-roll.
				return nil, err
			}
			return nil, fmt.Errorf("could not mint a unique proof reference after %d attempts", maxAttempts)
		}
	}
	// Whether we inserted or lost the race, the row now exists — read it back.
	return s.getByTxn(ctx, in.TransactionID, in.Environment)
}

const proofCols = `id, proof_reference, transaction_id, environment,
	COALESCE(payer_display_name,''), COALESCE(payer_handle,''), COALESCE(payer_subject_type,''),
	COALESCE(payee_display_name,''), COALESCE(payee_handle,''), COALESCE(payee_subject_type,''),
	amount_minor, currency, status, COALESCE(description,''), COALESCE(method,''),
	COALESCE(proof_hash,''), COALESCE(signature_key_id,''), COALESCE(signature_algorithm,''),
	verification_count, issued_at, confirmed_at, reversed_at`

func scanProof(row pgx.Row) (*Proof, error) {
	var p Proof
	err := row.Scan(&p.ID, &p.ProofReference, &p.TransactionID, &p.Environment,
		&p.PayerDisplayName, &p.PayerHandle, &p.PayerSubjectType,
		&p.PayeeDisplayName, &p.PayeeHandle, &p.PayeeSubjectType,
		&p.AmountMinor, &p.Currency, &p.Status, &p.Description, &p.Method,
		&p.ProofHash, &p.SignatureKeyID, &p.SignatureAlg,
		&p.VerificationCount, &p.IssuedAt, &p.ConfirmedAt, &p.ReversedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProofNotFound
		}
		return nil, err
	}
	return &p, nil
}

func (s *ProofService) getByTxn(ctx context.Context, txnID, env string) (*Proof, error) {
	return scanProof(s.pool.QueryRow(ctx,
		`SELECT `+proofCols+` FROM transaction_proofs WHERE transaction_id=$1 AND environment=$2`, txnID, env))
}

// GetByReference looks up a proof by its public reference.
//
// Two gates run BEFORE the query, because both are lookup-authority decisions
// and every caller must get them:
//
//  1. The reference must be canonically spelled. The SQL matches case-
//     insensitively, so without this a single proof would answer to many
//     spellings — and a rate limit keyed on the reference could be evaded by
//     rotating case. A non-canonical spelling is not a different identity for
//     the same proof; it is not a reference at all.
//
//  2. LEGACY_HEX_V0 carries roughly 32 bits of guessing resistance and exists
//     only as compatibility for Sandbox receipts already in people's hands. It
//     is never LIVE proof authority. This holds even if a LIVE row of that shape
//     were somehow inserted, which is the point of checking here rather than
//     trusting the data — "Financial LIVE does not exist yet" is a fact about
//     today, not a rule.
//
// Both refuse with ErrProofNotFound rather than a distinct reason: a caller
// probing LIVE must not learn that a reference is a real Sandbox receipt.
func (s *ProofService) GetByReference(ctx context.Context, ref string) (*Proof, error) {
	class := ClassifyReference(ref)
	if class == ReferenceInvalid {
		return nil, ErrProofNotFound
	}
	// Exact equality, not upper(...)=upper(...). The parser above already fixed the
	// canonical spelling, and a case-insensitive query would quietly restore the
	// second definition it exists to remove — leaving persistence and the public
	// contract disagreeing about what "the same reference" means, and giving
	// UNIQUE(proof_reference) a different notion of identity than lookup.
	p, err := scanProof(s.pool.QueryRow(ctx,
		`SELECT `+proofCols+` FROM transaction_proofs WHERE proof_reference=$1`, ref))
	if err != nil {
		return nil, err
	}
	if class == ReferenceLegacyV0 && !strings.EqualFold(strings.TrimSpace(p.Environment), "SANDBOX") {
		return nil, ErrProofNotFound
	}
	return p, nil
}

// RecordVerification logs a public verification (hashed ip/ua only) and bumps the
// counter. Best-effort: a logging failure must not break verification.
func (s *ProofService) RecordVerification(ctx context.Context, proofID, ipHash, uaHash, country string) {
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO transaction_proof_verifications (proof_id, ip_hash, user_agent_hash, country, result)
		 VALUES ($1,$2,$3,$4,'VERIFIED')`, proofID, nz(ipHash), nz(uaHash), nz(country))
	_, _ = s.pool.Exec(ctx,
		`UPDATE transaction_proofs SET verification_count=verification_count+1, updated_at=now() WHERE id=$1`, proofID)
}

// Public returns the safe, ADR-040-shaped public payload (no sensitive fields).
// Public builds the ADR-033 public ViewModel: an allow-list of non-secret,
// ledger-derived fields only. It deliberately omits the proof hash and the
// verification counter (§4/§5), every internal id/signature, and — by the
// privacy default (§7) — a party's display name unless that party is a public
// entity (a business); consumers are shown by @handle only.
func (s *ProofService) Public(p *Proof) map[string]any {
	out := map[string]any{
		"exists":           true,
		"status":           p.Status,
		"amount":           p.AmountMinor,
		"currency":         p.Currency,
		"payer_display":    publicDisplayName(p.PayerSubjectType, p.PayerDisplayName),
		"payer_handle":     p.PayerHandle,
		"payee_display":    publicDisplayName(p.PayeeSubjectType, p.PayeeDisplayName),
		"payee_handle":     p.PayeeHandle,
		"method":           p.Method,
		"description":      p.Description,
		"issued_at":        p.IssuedAt.UTC().Format(time.RFC3339),
		"verification_url": s.publicBase + p.ProofReference,
		"network":          s.network,
		"operator":         s.operatorID,
	}
	if p.ConfirmedAt != nil {
		out["confirmed_at"] = p.ConfirmedAt.UTC().Format(time.RFC3339)
	} else {
		out["confirmed_at"] = nil
	}
	return out
}

// publicDisplayName applies the ADR-033 §7 name-privacy default: a person's full
// name is private, so consumers (and unknown/empty subject types) resolve to nil
// (the page then shows only the @handle). Public entities — businesses — show
// their name. A future per-user "show my name publicly" opt-in would flip a
// consumer to public at proof time.
func publicDisplayName(subjectType, name string) any {
	switch strings.ToLower(strings.TrimSpace(subjectType)) {
	case "", "consumer", "customer", "user", "person":
		return nil
	default:
		if strings.TrimSpace(name) == "" {
			return nil
		}
		return name
	}
}

// Reference exposes the public reference for a transaction without leaking how it
// is generated — used by the receipt engine.
func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}
