// Materialise the proofs that should already exist.
//
// Between the receipt feature shipping and the proof service being wired, four
// receipt generators printed a public reference derived from an object id and
// never minted anything behind it. The documents are in people's hands; their QR
// codes resolve to "does not exist or may have been forged". BZM-F993-38E2 is
// one of them.
//
// This reconstructs those proofs from the ledger-backed records, using the exact
// LEGACY_HEX_V0 reference the retired generators printed, so an already-issued
// PDF becomes verifiable rather than being replaced by one nobody has.
//
// It is a command and not a migration on purpose. A proof carries a SHA-256 hash
// over a canonical, ordered payload and an HMAC signature under the operator
// key. That key is a docker secret and is not, and must not be, reachable from a
// SQL session; reproducing Go's canonical encoding in SQL would be a second
// implementation of proof materialisation, which is the exact class of defect
// this release is closing. So the backfill runs through ProofService itself and
// every field — hash, signature, status normalisation, idempotency — comes from
// the one implementation.
//
// Safe to re-run: Ensure is idempotent on (transaction_id, environment).
//
//	docker run --rm --network <net> \
//	  -e DATABASE_URL=... -e BZM_PROOF_SIGNING_KEY=... \
//	  <gateway-image> /usr/local/bin/backfill-proofs [-apply]
//
// Without -apply it reports what it would do and writes nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/banzami/banzami/services/api-gateway/internal/service"
	"github.com/banzami/banzami/services/common/env"
)

// The two families. Both once printed a derived reference:
//
//	transfers       → consumer/P2P receipts (public-api)
//	wallet_payments → merchant QR receipts (api-gateway)
//
// The SELECTs mirror proofInputFromTransfer and proofInputFromPayment exactly,
// including the Method strings and the ledger reference, so a backfilled proof
// is indistinguishable from one the live path would have minted — apart from the
// reference, which is the whole point.
//
// They deliberately do NOT filter on the source row's environment, and the proof
// does not inherit it. Those rows are exactly the ones 0113 repairs: most of them
// still say LIVE because their writer omitted the column. Reading the universe
// off a row we already know is mislabelled would be circular, and it would force
// the backfill to run after the migration — leaving a window in which a receipt
// request mints a SECURE_V1 proof for a record whose legacy reference is already
// printed on somebody's PDF, killing that reference permanently. The environment
// comes from platform_mode, which is the authority, and this can therefore run
// first.
const transfersQuery = `
	SELECT t.id::text,
	       t.sender_id::text, t.recipient_id::text,
	       COALESCE(cs.handle,''), COALESCE(NULLIF(TRIM(cs.display_name),''), '@'||cs.handle, ''),
	       COALESCE(cr.handle,''), COALESCE(NULLIF(TRIM(cr.display_name),''), '@'||cr.handle, ''),
	       t.amount_minor, t.currency, t.status, COALESCE(t.description,''),
	       COALESCE(t.updated_at, t.created_at)
	  FROM transfers t
	  LEFT JOIN consumers cs ON cs.id = t.sender_id
	  LEFT JOIN consumers cr ON cr.id = t.recipient_id
	 WHERE NOT EXISTS (SELECT 1 FROM transaction_proofs p
	                    WHERE p.transaction_id = t.id::text AND p.environment = $1)
	 ORDER BY t.created_at`

const walletPaymentsQuery = `
	SELECT wp.id::text,
	       wp.consumer_id::text, wp.merchant_id::text,
	       COALESCE(c.handle,''), COALESCE(NULLIF(TRIM(c.display_name),''), '@'||c.handle, ''),
	       COALESCE(m.name,''),
	       wp.amount_minor, wp.currency, wp.status,
	       wp.created_at
	  FROM wallet_payments wp
	  LEFT JOIN consumers c ON c.id = wp.consumer_id
	  LEFT JOIN merchants m ON m.id = wp.merchant_id
	 WHERE NOT EXISTS (SELECT 1 FROM transaction_proofs p
	                    WHERE p.transaction_id = wp.id::text AND p.environment = $1)
	 ORDER BY wp.created_at`

type outcome struct{ materialised, skipped, failed int }

func main() {
	apply := flag.Bool("apply", false, "write the proofs; without it, report only")
	flag.Parse()

	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fatal("DATABASE_URL is not set")
	}

	// The platform's own declaration, checked here as well as in 0113. This
	// command mints LEGACY_HEX_V0 references, which the public lookup refuses
	// outside the Sandbox — a LIVE run would write rows nothing can read.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fatal("database: %v", err)
	}
	defer pool.Close()

	var mode string
	if err := pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key='platform_mode' AND environment='GLOBAL'`,
	).Scan(&mode); err != nil {
		fatal("could not read platform_mode: %v", err)
	}
	if !env.Parse(mode).IsSandbox() {
		fatal("refusing: platform_mode is %s, not SANDBOX", mode)
	}

	signingKey := os.Getenv("BZM_PROOF_SIGNING_KEY")
	if signingKey == "" {
		// An unsigned proof is a weaker record than no proof is a missing one:
		// it would sit in the table looking materialised while carrying an HMAC
		// under the empty key.
		fatal("refusing: BZM_PROOF_SIGNING_KEY is not set — a proof must be signed")
	}

	svc := service.NewProofService(pool, signingKey,
		os.Getenv("BZM_PROOF_KEY_ID"), os.Getenv("BZM_OPERATOR_ID"),
		os.Getenv("BZM_NETWORK"), os.Getenv("BZM_PROOF_PUBLIC_BASE"))

	total := outcome{}
	// env.SandboxName, not the string on the row: platform_mode is the authority
	// and the rows are the thing being corrected.
	total.add(run(ctx, pool, svc, "transfers", transfersQuery, scanTransfer, env.SandboxName, *apply))
	total.add(run(ctx, pool, svc, "wallet_payments", walletPaymentsQuery, scanWalletPayment, env.SandboxName, *apply))

	verb := "would materialise"
	if *apply {
		verb = "materialised"
	}
	fmt.Printf("\n%s %d proofs, %d already present, %d failed\n",
		verb, total.materialised, total.skipped, total.failed)
	if total.failed > 0 {
		os.Exit(1)
	}
}

// scannable is one row. Named so the two scanners read as what they are.
type scannable interface{ Scan(dest ...any) error }

type rowScanner func(r scannable, environment string) (sourceID string, in service.ProofInput, err error)

func scanTransfer(r scannable, environment string) (string, service.ProofInput, error) {
	var id, senderID, recipientID, senderHandle, senderName, recipientHandle, recipientName string
	var amount int64
	var currency, status, description string
	var confirmed time.Time
	if err := r.Scan(&id, &senderID, &recipientID, &senderHandle, &senderName,
		&recipientHandle, &recipientName, &amount, &currency, &status,
		&description, &confirmed); err != nil {
		return "", service.ProofInput{}, err
	}
	return id, service.ProofInput{
		TransactionID: id, TransferID: id, Environment: environment,
		PayerSubjectType: "consumer", PayerSubjectID: senderID,
		PayerDisplayName: senderName, PayerHandle: senderHandle,
		PayeeSubjectType: "consumer", PayeeSubjectID: recipientID,
		PayeeDisplayName: recipientName, PayeeHandle: recipientHandle,
		AmountMinor: amount, Currency: currency, Status: status,
		Description: description,
		Method:      "Transferência Banzami · @banza",
		// The live path uses the transfer id as the ledger reference; keeping it
		// identical matters because it is one of the signed fields.
		LedgerReference: id,
		ConfirmedAt:     &confirmed,
	}, nil
}

func scanWalletPayment(r scannable, environment string) (string, service.ProofInput, error) {
	var id, consumerID, merchantID, payerHandle, payerName, merchantName string
	var amount int64
	var currency, status string
	var created time.Time
	if err := r.Scan(&id, &consumerID, &merchantID, &payerHandle, &payerName,
		&merchantName, &amount, &currency, &status, &created); err != nil {
		return "", service.ProofInput{}, err
	}
	return id, service.ProofInput{
		TransactionID: id, Environment: environment,
		PayerSubjectType: "consumer", PayerSubjectID: consumerID,
		PayerDisplayName: payerName, PayerHandle: payerHandle,
		PayeeSubjectType: "merchant", PayeeSubjectID: merchantID,
		PayeeDisplayName: merchantName,
		AmountMinor:      amount, Currency: currency, Status: status,
		Method:          "Pagamento por QR · @banza",
		LedgerReference: id,
		ConfirmedAt:     &created,
	}, nil
}

func run(ctx context.Context, pool *pgxpool.Pool, svc *service.ProofService,
	family, query string, scan rowScanner, environment string, apply bool) outcome {
	rows, err := pool.Query(ctx, query, environment)
	if err != nil {
		fatal("%s: %v", family, err)
	}
	type item struct {
		sourceID string
		in       service.ProofInput
	}
	var items []item
	for rows.Next() {
		sourceID, in, err := scan(rows, environment)
		if err != nil {
			fatal("%s: %v", family, err)
		}
		items = append(items, item{sourceID, in})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fatal("%s: %v", family, err)
	}

	out := outcome{}
	fmt.Printf("%s: %d record(s) without a proof\n", family, len(items))
	for _, it := range items {
		if !apply {
			out.materialised++
			continue
		}
		p, err := svc.EnsureHistorical(ctx, it.sourceID, it.in)
		switch {
		case err == nil:
			out.materialised++
			// The reference is printed because it is already public — it is on a
			// PDF somebody holds. Nothing else about the proof is logged.
			fmt.Printf("  %s → %s\n", it.sourceID, p.ProofReference)
		case errors.Is(err, service.ErrHistoricalReferenceTaken):
			// Two records deriving the same 32-bit reference. Neither can own it
			// unambiguously, so the second is left alone and reported rather than
			// pointed at the first record's proof.
			out.failed++
			slog.Error("legacy reference collision", "family", family, "source_id", it.sourceID, "error", err)
		default:
			out.failed++
			slog.Error("could not materialise proof", "family", family, "source_id", it.sourceID, "error", err)
		}
	}
	return out
}

func (o *outcome) add(other outcome) {
	o.materialised += other.materialised
	o.skipped += other.skipped
	o.failed += other.failed
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "backfill-proofs: "+format+"\n", args...)
	os.Exit(1)
}
