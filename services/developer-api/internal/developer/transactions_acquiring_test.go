package developer

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Console showed a Payment Session's PROTOCOL status and nothing else, so a
// developer saw thirteen sessions reading ACTIVE beside a balance of 1 100 000 Kz
// and had no way to reconcile the two. The money had arrived; the protocol
// representation had not moved, because BANZA's payment_session.paid requires a
// transfer_id and a Transfer must originate from a consumer wallet — which an
// externally acquired payer is not (BANZA RFC-0007).
//
// These prove the operator's execution state is now visible ALONGSIDE the
// protocol status, and that neither is edited to agree with the other.

func seedAcquiringFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (merchant string, unpaid string, paid string, account string) {
	t.Helper()
	m := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO merchants (id, name, email, status) VALUES ($1,'ACQ',$2,'ACTIVE')`,
		m, "acq-"+m[:8]+"@t.test"); err != nil {
		// A fixed address made the SECOND caller in a run skip on a unique-key
		// violation, which reads as "not applicable" and is really "not run".
		t.Fatalf("cannot seed merchant: %v", err)
	}
	mk := func(kind, name string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO ledger_accounts (id, account_type, name, currency)
			 VALUES (gen_random_uuid(),$1,$2,'AOA') RETURNING id::text`, kind, name).Scan(&id); err != nil {
			t.Fatalf("ledger account: %v", err)
		}
		return id
	}
	avail, reserved := mk("LIABILITY", "avail"), mk("LIABILITY", "reserved")
	wallet := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO wallets (id, merchant_id, currency, status, available_account_id, reserved_account_id)
		 VALUES ($1,$2,'AOA','ACTIVE',$3,$4)`, wallet, m, avail, reserved); err != nil {
		t.Fatalf("wallet: %v", err)
	}
	var wa string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM wallet_accounts WHERE wallet_id = $1 AND purpose = 'PRIMARY'`, wallet).Scan(&wa); err != nil {
		t.Fatalf("primary wallet account: %v", err)
	}

	// One session whose link is still ACTIVE, and one that actually SETTLED.
	newSession := func(linkStatus string, paidAt *time.Time) string {
		link := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO payment_links (id, merchant_id, wallet_id, wallet_account_id, slug, amount_minor, currency, status, paid_at, environment)
			 VALUES ($1,$2,$3,$4,$5,100000,'AOA',$6,$7,'SANDBOX')`,
			link, m, wallet, wa, "s"+uuid.NewString()[:11], linkStatus, paidAt); err != nil {
			t.Fatalf("link: %v", err)
		}
		sess := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO payment_sessions (id, merchant_id, wallet_id, wallet_account_id, currency, amount_minor, status, payment_link_id)
			 VALUES ($1,$2,$3,$4,'AOA',100000,'ACTIVE',$5)`, sess, m, wallet, wa, link); err != nil {
			t.Fatalf("session: %v", err)
		}
		return sess
	}
	when := time.Now().UTC().Truncate(time.Second)
	unpaidSession := newSession("ACTIVE", nil)
	paidSession := newSession("USED", &when)
	settle(ctx, t, pool, paidSession, wa, 100000, when)
	return m, unpaidSession, paidSession, wa
}

// Record what the acquiring rail records when a payment settles: a CONFIRMED
// acquiring payment against the session's link, and the balanced posting whose
// CREDIT leg lands on the destination account. Money moving is the fact the
// view reads; anything less is a link with a flag set.
func settle(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, walletAccount string, amount int64, at time.Time) {
	t.Helper()
	var link, ledgerAccount, transit string
	if err := pool.QueryRow(ctx,
		`SELECT payment_link_id::text FROM payment_sessions WHERE id = $1`, sessionID).Scan(&link); err != nil {
		t.Fatalf("session link: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT account_id::text FROM wallet_accounts WHERE id = $1`, walletAccount).Scan(&ledgerAccount); err != nil {
		t.Fatalf("destination ledger account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ledger_accounts (id, account_type, name, currency)
		 VALUES (gen_random_uuid(),'ASSET','transit','AOA') RETURNING id::text`).Scan(&transit); err != nil {
		t.Fatalf("transit: %v", err)
	}
	acq := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO acquiring_payments
		   (id, payment_link_id, provider, external_ref, status, amount_minor, currency, instructions, confirmed_at, expires_at)
		 VALUES ($1,$2,'EMIS_MULTICAIXA_SIMULATED',$3,'CONFIRMED',$4,'AOA','{}'::jsonb,$5,$6)`,
		acq, link, acq[:9], amount, at, at.Add(time.Hour)); err != nil {
		t.Fatalf("acquiring payment: %v", err)
	}
	var posting string
	if err := pool.QueryRow(ctx,
		`INSERT INTO ledger_postings (id, description, idempotency_key, created_at)
		 VALUES (gen_random_uuid(), 'Acquiring settlement', $1, $2) RETURNING id::text`,
		"acquiring-settle-"+acq, at).Scan(&posting); err != nil {
		t.Fatalf("posting: %v", err)
	}
	for _, leg := range []struct {
		kind, account string
	}{{"DEBIT", transit}, {"CREDIT", ledgerAccount}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO ledger_entries (id, posting_id, account_id, entry_type, amount_minor, currency, created_at)
			 VALUES (gen_random_uuid(),$1,$2,$3,$4,'AOA',$5)`,
			posting, leg.account, leg.kind, amount, at); err != nil {
			t.Fatalf("%s leg: %v", leg.kind, err)
		}
	}
}

func acqRow(t *testing.T, rows []TransactionView, id string) TransactionView {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("session %s not present in the developer's own transactions view", id)
	return TransactionView{}
}

func TestTransactions_AcquiringStateIsVisibleBesideProtocolStatus(t *testing.T) {
	ctx := context.Background()
	pool := devPoolOrSkip(ctx, t)
	defer pool.Close()
	store := &pgStore{pool: pool}

	merchant, unpaid, paid, account := seedAcquiringFixture(ctx, t, pool)
	rows, err := store.TransactionsForMerchant(ctx, merchant, TransactionFilter{Limit: 50})
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}

	t.Run("an unpaid session is not described as having received anything", func(t *testing.T) {
		r := acqRow(t, rows, unpaid)
		if r.Status != "ACTIVE" {
			t.Fatalf("protocol status = %q, want ACTIVE", r.Status)
		}
		if r.Acquiring == nil {
			t.Fatal("no acquiring state at all — the developer is back to guessing")
		}
		if r.Acquiring.State != "UNPAID" {
			t.Fatalf("acquiring state = %q, want UNPAID", r.Acquiring.State)
		}
		if r.Acquiring.PaidAt != nil {
			t.Errorf("an unpaid operation carries paid_at %v — a timestamp where there is none", r.Acquiring.PaidAt)
		}
		if r.Acquiring.AmountMinor != nil {
			t.Errorf("an unpaid operation reports %d received", *r.Acquiring.AmountMinor)
		}
		if r.Acquiring.CreditedWalletAccountID != "" {
			t.Errorf("an unpaid operation attributes a credit to %s", r.Acquiring.CreditedWalletAccountID)
		}
		if r.Acquiring.ProtocolNote != "" {
			t.Error("the protocol note appears where the two states agree — that trains the reader to skip it")
		}
	})

	t.Run("an externally acquired paid session shows both truths without contradiction", func(t *testing.T) {
		r := acqRow(t, rows, paid)
		// The protocol status is NOT edited. That is the point.
		if r.Status != "ACTIVE" {
			t.Fatalf("protocol status = %q — the session must not be relabelled PAID until BANZA defines that transition", r.Status)
		}
		if r.Acquiring == nil {
			t.Fatal("the payment settled and the developer can see only ACTIVE — this is the defect")
		}
		if r.Acquiring.State != "PAID" {
			t.Fatalf("acquiring state = %q, want PAID", r.Acquiring.State)
		}
		if r.Acquiring.AmountMinor == nil || *r.Acquiring.AmountMinor != 100000 {
			t.Errorf("amount received = %v, want 100000", r.Acquiring.AmountMinor)
		}
		if r.Acquiring.PaidAt == nil {
			t.Error("no paid_at on a paid operation")
		}
		if r.Acquiring.CreditedWalletAccountID != account {
			t.Errorf("credited account = %q, want the session's own account %q", r.Acquiring.CreditedWalletAccountID, account)
		}
		if r.Acquiring.Interface != "PAYMENT_LINK" {
			t.Errorf("interface = %q, want PAYMENT_LINK — the state must be traceable to how it was paid", r.Acquiring.Interface)
		}
		if r.Acquiring.ProtocolNote == "" {
			t.Error("PAID sits beside ACTIVE with no explanation — the payload must carry the reason, not just the Console")
		}
	})
}

// THE PHANTOM-SUCCESS GUARD.
//
// A payment confirmed before the acquiring settlement defect was fixed marks its
// link USED and posts nothing to the ledger. Keying the view on the link
// reported "PAGO · Recebido 100 000 Kz · creditado <account>" for a payment
// where no money had moved — reproduced on the deployed Sandbox, where the DOA
// 11:25 payment's destination campaign account holds 0 to this day.
//
// A view that invents a receipt is worse than the blank one it replaced: it
// tells an integrator the money is there.
func TestTransactions_AUsedLinkWithoutSettlementIsNotPaid(t *testing.T) {
	ctx := context.Background()
	pool := devPoolOrSkip(ctx, t)
	defer pool.Close()
	store := &pgStore{pool: pool}

	merchant, _, paid, _ := seedAcquiringFixture(ctx, t, pool)

	// Exactly the pre-fix shape: the link is USED, and nothing was posted.
	var link string
	if err := pool.QueryRow(ctx,
		`SELECT payment_link_id::text FROM payment_sessions WHERE id = $1`, paid).Scan(&link); err != nil {
		t.Fatalf("link: %v", err)
	}
	stranded := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO payment_links (id, merchant_id, wallet_id, wallet_account_id, slug, amount_minor, currency, status, paid_at, environment)
		 SELECT $1, merchant_id, wallet_id, wallet_account_id, $2, amount_minor, currency, 'USED', now(), environment
		   FROM payment_links WHERE id = $3`, stranded, "s"+uuid.NewString()[:11], link); err != nil {
		t.Fatalf("stranded link: %v", err)
	}
	sess := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO payment_sessions (id, merchant_id, wallet_id, wallet_account_id, currency, amount_minor, status, payment_link_id)
		 SELECT $1, merchant_id, wallet_id, wallet_account_id, currency, amount_minor, 'ACTIVE', $2
		   FROM payment_sessions WHERE id = $3`, sess, stranded, paid); err != nil {
		t.Fatalf("stranded session: %v", err)
	}

	rows, err := store.TransactionsForMerchant(ctx, merchant, TransactionFilter{Limit: 50})
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	r := acqRow(t, rows, sess)
	if r.Acquiring == nil {
		t.Fatal("no acquiring state at all")
	}
	if r.Acquiring.State != "UNPAID" {
		t.Fatalf("state = %q for a used link with NO ledger posting — the view is reporting a "+
			"receipt for money that never moved", r.Acquiring.State)
	}
	if r.Acquiring.AmountMinor != nil {
		t.Errorf("reports %d received where nothing was posted", *r.Acquiring.AmountMinor)
	}
	if r.Acquiring.PaidAt != nil {
		t.Error("carries a paid_at for a settlement that never happened")
	}
	if r.Acquiring.CreditedWalletAccountID != "" {
		t.Errorf("attributes a credit to %s that the ledger does not show",
			r.Acquiring.CreditedWalletAccountID)
	}
}

// A QR-paid session must not be reported UNPAID. Reading only the link would be
// a worse answer than the one this view replaces.
func TestTransactions_AcquiringStateFollowsTheInterfaceActuallyUsed(t *testing.T) {
	ctx := context.Background()
	pool := devPoolOrSkip(ctx, t)
	defer pool.Close()
	store := &pgStore{pool: pool}

	m := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO merchants (id, name, email, status) VALUES ($1,'QR',$2,'ACTIVE')`,
		m, "qr-"+m[:8]+"@t.test"); err != nil {
		t.Fatalf("cannot seed merchant: %v", err)
	}
	var avail, reserved string
	for _, p := range []*string{&avail, &reserved} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO ledger_accounts (id, account_type, name, currency)
			 VALUES (gen_random_uuid(),'LIABILITY','a','AOA') RETURNING id::text`).Scan(p); err != nil {
			t.Fatalf("ledger account: %v", err)
		}
	}
	wallet := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO wallets (id, merchant_id, currency, status, available_account_id, reserved_account_id)
		 VALUES ($1,$2,'AOA','ACTIVE',$3,$4)`, wallet, m, avail, reserved); err != nil {
		t.Fatalf("wallet: %v", err)
	}
	var wa string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM wallet_accounts WHERE wallet_id = $1 AND purpose='PRIMARY'`, wallet).Scan(&wa); err != nil {
		t.Fatalf("wallet account: %v", err)
	}
	qr := uuid.NewString()
	used := time.Now().UTC().Truncate(time.Second)
	if _, err := pool.Exec(ctx,
		// A DYNAMIC QR must carry an expiry (qr_codes_dynamic_requires_expiry).
		`INSERT INTO qr_codes (id, owner_id, owner_type, qr_type, currency, amount_minor, status, used_at, expires_at, wallet_account_id, environment)
		 VALUES ($1,$2,'MERCHANT','DYNAMIC','AOA',100000,'USED',$3,$4,$5,'SANDBOX')`,
		qr, m, used, used.Add(time.Hour), wa); err != nil {
		t.Fatalf("qr_codes: %v", err)
	}
	sess := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO payment_sessions (id, merchant_id, wallet_id, wallet_account_id, currency, amount_minor, status, qr_code_id)
		 VALUES ($1,$2,$3,$4,'AOA',100000,'PAID',$5)`, sess, m, wallet, wa, qr); err != nil {
		t.Fatalf("session: %v", err)
	}

	rows, err := store.TransactionsForMerchant(ctx, m, TransactionFilter{Limit: 50})
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	r := acqRow(t, rows, sess)
	if r.Acquiring == nil || r.Acquiring.State != "PAID" {
		t.Fatalf("a QR-paid session reports %v — reading only the link would call it UNPAID", r.Acquiring)
	}
	if r.Acquiring.Interface != "DYNAMIC_QR" {
		t.Errorf("interface = %q, want DYNAMIC_QR", r.Acquiring.Interface)
	}
	if r.Acquiring.PaidAt == nil {
		t.Error("a QR-paid session carries no paid_at")
	}
}

// Observability must stay singular under the duplicate and parallel
// confirmations the payer rail actually sees: one settlement, one credit, one row.
func TestTransactions_AcquiringStateIsSingularUnderRepeatedConfirmation(t *testing.T) {
	ctx := context.Background()
	pool := devPoolOrSkip(ctx, t)
	defer pool.Close()
	store := &pgStore{pool: pool}

	merchant, _, paid, account := seedAcquiringFixture(ctx, t, pool)

	// The link transition is idempotent in core; re-running the read must not
	// multiply the row or change what it says.
	var seen []TransactionView
	for i := 0; i < 5; i++ {
		rows, err := store.TransactionsForMerchant(ctx, merchant, TransactionFilter{Limit: 50})
		if err != nil {
			t.Fatalf("transactions: %v", err)
		}
		n := 0
		for _, r := range rows {
			if r.ID == paid {
				n++
				seen = append(seen, r)
			}
		}
		if n != 1 {
			t.Fatalf("the paid session appears %d times in one page — a duplicated row is a duplicated payment to anyone reading it", n)
		}
	}
	for _, r := range seen {
		if r.Acquiring == nil || r.Acquiring.State != "PAID" ||
			r.Acquiring.AmountMinor == nil || *r.Acquiring.AmountMinor != 100000 ||
			r.Acquiring.CreditedWalletAccountID != account {
			t.Fatalf("the acquiring state is not deterministic across reads: %+v", r.Acquiring)
		}
	}
}
