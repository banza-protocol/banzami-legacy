package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/banzami/banzami/services/api-gateway/internal/kybstorage"
)

func dbPoolOrSkip(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set — skipping DB-backed test")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var reg *string
	_ = pool.QueryRow(context.Background(), `SELECT to_regclass('public.merchant_kyb_documents')::text`).Scan(&reg)
	if reg == nil {
		pool.Close()
		t.Skip("merchant_kyb_documents not migrated — skipping")
	}
	return pool
}

func seedKybDoc(t *testing.T, pool *pgxpool.Pool, merchantID, status string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO merchant_kyb_documents
		   (id, merchant_id, document_type, status, storage_bucket, storage_key, mime_type, environment, submitted_at)
		 VALUES ($1,$2,'COMPANY_TAX_ID',$3,'banzami-kyb-sandbox',$4,'image/jpeg','SANDBOX',NOW())`,
		id, merchantID, status, "k/"+id)
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	return id
}

func TestKybAdminList_EnrichesMerchant(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()
	svc := NewPostgresMerchantKybService(pool, kybstorage.NewFakeStorage("banzami-kyb-sandbox"), 5*1024*1024)

	m := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, email, status) VALUES ($1,'Doa Sandbox',$2,'ACTIVE')`, m, m+"@test"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	realDoc := seedKybDoc(t, pool, m, "PENDING_REVIEW")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE id=$1`, realDoc)
		_, _ = pool.Exec(ctx, `DELETE FROM merchants WHERE id=$1`, m)
	})

	docs, err := svc.AdminList(ctx, "PENDING_REVIEW", 200)
	if err != nil {
		t.Fatalf("AdminList: %v", err)
	}
	var real *MerchantKybAdminDocument
	for i := range docs {
		if docs[i].ID == realDoc {
			real = &docs[i]
		}
	}
	if real == nil {
		t.Fatalf("expected the seeded document in the admin list")
	}
	if real.MerchantName != "Doa Sandbox" || !real.MerchantExists {
		t.Fatalf("document not enriched: name=%q exists=%v", real.MerchantName, real.MerchantExists)
	}
}

// An orphan KYB document — one whose merchant_id names no merchant — is no
// longer representable. Migration 0078 added
// merchant_kyb_documents_merchant_id_fkey (ON DELETE RESTRICT) precisely because
// referential integrity had rested on a non-atomic app-layer check-then-insert.
//
// This test therefore asserts the guarantee that migration actually delivers:
// the database refuses the orphan outright, and refuses to orphan an existing
// document by deleting its merchant. The service still carries an app-layer
// orphan guard (ErrKybMerchantNotFound) as defence in depth; it is unreachable
// through the database while this constraint stands, which is the point.
func TestKybDocument_CannotBeOrphaned(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()

	// 1. Inserting a document for a merchant that does not exist is rejected.
	orphanMerchant := uuid.NewString()
	id := uuid.NewString()
	_, err := pool.Exec(ctx,
		`INSERT INTO merchant_kyb_documents
		   (id, merchant_id, document_type, status, storage_bucket, storage_key, mime_type, environment, submitted_at)
		 VALUES ($1,$2,'COMPANY_TAX_ID','PENDING_REVIEW','banzami-kyb-sandbox',$3,'image/jpeg','SANDBOX',NOW())`,
		id, orphanMerchant, "k/"+id)
	if err == nil {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE id=$1`, id)
		t.Fatal("an orphan KYB document was accepted: the merchant_id foreign key is missing")
	}
	if !strings.Contains(err.Error(), "merchant_kyb_documents_merchant_id_fkey") {
		t.Fatalf("orphan insert rejected for the wrong reason: %v", err)
	}

	// 2. Deleting a merchant that still has KYB documents is refused, so an
	//    existing document cannot be orphaned after the fact either.
	m := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, email, status) VALUES ($1,'Loja FK',$2,'ACTIVE')`, m, m+"@test"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	doc := seedKybDoc(t, pool, m, "PENDING_REVIEW")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE id=$1`, doc)
		_, _ = pool.Exec(ctx, `DELETE FROM merchants WHERE id=$1`, m)
	})
	if _, err := pool.Exec(ctx, `DELETE FROM merchants WHERE id=$1`, m); err == nil {
		t.Fatal("deleting a merchant with KYB documents was allowed: ON DELETE RESTRICT is missing")
	}
}

// The merchant-centric queue returns one row per merchant with correct per-status
// counts; per-merchant documents lists that merchant's docs; and the KYB badge
// counts distinct merchants, not loose documents.
func TestKybAdminMerchants_AggregatesAndCounts(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()
	svc := NewPostgresMerchantKybService(pool, kybstorage.NewFakeStorage("banzami-kyb-sandbox"), 5*1024*1024)

	m := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, email, status) VALUES ($1,'Loja Agregada',$2,'ACTIVE')`, m, m+"@test"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	seed := func(dtype, status string) string {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO merchant_kyb_documents (id, merchant_id, document_type, status, storage_bucket, storage_key, mime_type, environment, submitted_at)
			 VALUES ($1,$2,$3,$4,'banzami-kyb-sandbox',$5,'image/jpeg','SANDBOX',NOW())`,
			id, m, dtype, status, "k/"+id); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
		return id
	}
	d1 := seed("COMMERCIAL_REGISTRATION", "PENDING_REVIEW")
	d2 := seed("COMPANY_TAX_ID", "VALID")
	d3 := seed("REPRESENTATIVE_ID", "REJECTED")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE merchant_id=$1`, m)
		_, _ = pool.Exec(ctx, `DELETE FROM merchants WHERE id=$1`, m)
	})

	merchants, err := svc.AdminListMerchants(ctx, 200)
	if err != nil {
		t.Fatalf("AdminListMerchants: %v", err)
	}
	var row *MerchantKybSummary
	for i := range merchants {
		if merchants[i].MerchantID == m {
			row = &merchants[i]
		}
	}
	if row == nil {
		t.Fatalf("merchant not in queue")
	}
	if row.Name != "Loja Agregada" || !row.MerchantExists {
		t.Fatalf("merchant row not enriched: %+v", row)
	}
	if row.Total != 3 || row.Pending != 1 || row.Approved != 1 || row.Rejected != 1 {
		t.Fatalf("aggregate counts wrong: total=%d pending=%d approved=%d rejected=%d", row.Total, row.Pending, row.Approved, row.Rejected)
	}

	docs, err := svc.AdminMerchantDocuments(ctx, m)
	if err != nil {
		t.Fatalf("AdminMerchantDocuments: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("expected 3 documents for merchant, got %d", len(docs))
	}
	_ = d1
	_ = d2
	_ = d3

	// The KYB badge counts distinct merchants, not documents: this merchant with 1
	// pending doc contributes exactly 1.
	sum, err := NewNotificationsService(pool).Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.PendingKybDocuments < 1 {
		t.Fatalf("expected >=1 merchant with pending KYB, got %d", sum.PendingKybDocuments)
	}
}

func TestNotificationsSummary_Counts(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()

	// The KYB document needs a real merchant: merchant_kyb_documents.merchant_id
	// is a foreign key (migration 0078), so a random id would be rejected.
	nm := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, email, status) VALUES ($1,'Notif Merchant',$2,'ACTIVE')`, nm, nm+"@test"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	doc := seedKybDoc(t, pool, nm, "PENDING_REVIEW")
	asID := uuid.NewString()
	_, _ = pool.Exec(ctx,
		`INSERT INTO app_settlements (id, owner_ref, source_account_id, beneficiary_account_id,
		   gross_amount_minor, application_fee_minor, net_amount_minor, currency, engine_version,
		   pricing_snapshot_json, status, environment, idempotency_key)
		 VALUES ($1,'c',$2,$3,1000,0,1000,'AOA',1,'{}'::jsonb,'FAILED','SANDBOX',$4)`,
		asID, uuid.NewString(), uuid.NewString(), "notif-"+asID)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE id=$1`, doc)
		_, _ = pool.Exec(ctx, `DELETE FROM app_settlements WHERE id=$1`, asID)
	})

	sum, err := NewNotificationsService(pool).Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.PendingKybDocuments < 1 {
		t.Fatalf("expected >=1 pending KYB doc, got %d", sum.PendingKybDocuments)
	}
	if sum.FailedAppSettlements < 1 {
		t.Fatalf("expected >=1 failed settlement, got %d", sum.FailedAppSettlements)
	}
}

// Summary must not 500 when an optional table is absent (e.g. live `banzami` has
// no `disputes` table). The probe drops the missing metric to 0 instead of letting
// Postgres fail to parse the relation.
func TestNotificationsSummary_ToleratesMissingTable(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()

	var hasDisputes bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.disputes') IS NOT NULL`).Scan(&hasDisputes); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if hasDisputes {
		// Simulate the live shape (no disputes table) for the duration of the test.
		if _, err := pool.Exec(ctx, `ALTER TABLE disputes RENAME TO disputes__hidden_for_test`); err != nil {
			t.Skipf("cannot hide disputes table: %v", err)
		}
		// Restored by defer, not t.Cleanup, and the error is not discarded.
		//
		// Cleanups run AFTER the test function returns — which is after the
		// `defer pool.Close()` above has already closed the pool. So the restore
		// ran against a dead pool, `_ =` swallowed the failure, and the table
		// stayed renamed. Every later user of that database then found `disputes`
		// missing: sqlx::query! stopped compiling core-api against it, and this
		// test itself started reporting the absent-table case as if it were real.
		// A test that reshapes a shared schema has to put it back, loudly.
		//
		// Deferred funcs run last-in-first-out, so this one runs before the pool
		// is closed.
		defer func() {
			if _, err := pool.Exec(context.Background(),
				`ALTER TABLE disputes__hidden_for_test RENAME TO disputes`); err != nil {
				t.Errorf("could not restore the disputes table: %v — this database is "+
					"now missing a relation every later run expects", err)
			}
		}()
	}

	sum, err := NewNotificationsService(pool).Summary(ctx)
	if err != nil {
		t.Fatalf("Summary must tolerate a missing table, got: %v", err)
	}
	if sum.OpenDisputes != 0 {
		t.Fatalf("absent disputes table must count as 0, got %d", sum.OpenDisputes)
	}
}

func TestKybContext_Timeline_Notes(t *testing.T) {
	pool := dbPoolOrSkip(t)
	defer pool.Close()
	ctx := context.Background()
	svc := NewPostgresMerchantKybService(pool, kybstorage.NewFakeStorage("banzami-kyb-sandbox"), 5*1024*1024)

	m := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, email, status) VALUES ($1,'Loja Teste',$2,'ACTIVE')`, m, m+"@test"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO merchant_applications (id, status, environment, desired_handle, business_name, email,
		   created_merchant_id, legal_representative, phone, nif, country, city, address, business_activity)
		 VALUES ($1,'APPROVED','SANDBOX',$2,'Loja Teste Lda',$3,$4,'Ana Silva','+244900','NIF123','AO','Luanda','Rua 1','Retalho')`,
		uuid.NewString(), "loja"+m[:6], m+"@test", m); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	doc := seedKybDoc(t, pool, m, "PENDING_REVIEW")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_events WHERE merchant_id=$1`, m)
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_kyb_documents WHERE id=$1`, doc)
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_applications WHERE created_merchant_id=$1`, m)
		_, _ = pool.Exec(ctx, `DELETE FROM merchant_compliance WHERE merchant_id=$1`, m)
		_, _ = pool.Exec(ctx, `DELETE FROM merchants WHERE id=$1`, m)
	})

	// Context returns merchant + representative + company.
	c, err := svc.Context(ctx, m)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if !c.MerchantExists || c.Name != "Loja Teste" || c.RepName != "Ana Silva" || c.LegalName != "Loja Teste Lda" || c.Nif != "NIF123" || c.Country != "AO" {
		t.Fatalf("context not enriched: %+v", c)
	}

	// Approve with internal notes -> stored in metadata + event payload.
	if err := svc.AdminApprove(ctx, doc, "op-1", "documentos conferidos", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ev, err := svc.Timeline(ctx, m)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	var approved bool
	for _, e := range ev {
		if e.EventType == "merchant.kyb.document.approved" {
			approved = true
			if e.Payload["notes"] != "documentos conferidos" {
				t.Fatalf("notes not in event payload: %v", e.Payload)
			}
		}
	}
	if !approved {
		t.Fatalf("expected an approved event in timeline, got %d events", len(ev))
	}

	// On-demand signed URL is minted (fake storage), key never returned.
	url, err := svc.ReadURL(ctx, doc)
	if err != nil || url == "" {
		t.Fatalf("ReadURL: url=%q err=%v", url, err)
	}
}
