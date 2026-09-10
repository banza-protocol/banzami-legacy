package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/banzami/banzami/services/common/env"
	"github.com/banzami/banzami/services/common/obs"

	"github.com/banzami/banzami/services/api-gateway/internal/config"
	"github.com/banzami/banzami/services/api-gateway/internal/crypto"
	"github.com/banzami/banzami/services/api-gateway/internal/kybstorage"
	"github.com/banzami/banzami/services/api-gateway/internal/notify"
	"github.com/banzami/banzami/services/api-gateway/internal/observability"
	"github.com/banzami/banzami/services/api-gateway/internal/server"
	"github.com/banzami/banzami/services/api-gateway/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	initLogger(cfg)

	if cfg.PayBaseURL == "" {
		// Not fatal, because local development has no payer surface to point at.
		// It is loud because the consequence is silent: PAYMENT_LINK keeps
		// resolving to this gateway's own /public/pay route, which answers with
		// JSON — so every link an integration hands a customer opens a JSON
		// document instead of a payment page (ADR-052).
		slog.Warn("PAY_BASE_URL not set — payment links will point at this gateway's JSON route, not the hosted payer surface")
	}

	// Initialise OpenTelemetry. Metrics are always active (Prometheus);
	// tracing is active only when OTLP_ENDPOINT is set.
	ctx := context.Background()
	shutdownOTel, err := observability.Setup(ctx, "api-gateway", "0.1.0", cfg.Environment, cfg.OTLPEndpoint)
	if err != nil {
		slog.Error("otel setup error", "error", err)
		os.Exit(1)
	}

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		slog.Error("invalid REDIS_URL", "error", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)

	fcmSvc, err := notify.NewFCMService(ctx, cfg.FirebaseCredentialsJSON, cfg.Environment)
	if err != nil {
		slog.Error("[FCM] init error", "error", err)
		os.Exit(1)
	}
	if fcmSvc == nil {
		slog.Warn("[FCM] FIREBASE_CREDENTIALS_JSON not set — push notifications disabled")
	}

	// Real core-api client — delegates all financial operations to the Rust core.
	coreClient := service.NewCoreApiClient(cfg.CoreAPIURL, cfg.CoreInternalKey)

	// Webhook service: use PostgreSQL-backed implementation when DATABASE_URL is
	// set; fall back to the in-memory stub for local dev without a full stack.
	var webhookSvc service.WebhookService
	var teamSvc service.TeamService
	var merchantCredSvc service.MerchantCredentialService
	var merchantAppSvc service.MerchantApplicationService
	var merchantAppAdminSvc service.MerchantApplicationAdminService
	var merchantDocumentSvc service.MerchantDocumentService
	var merchantKybSvc *service.PostgresMerchantKybService
	var notificationsSvc *service.NotificationsService
	var platformSvc *service.PlatformReadService
	var proofSvc *service.ProofService
	var businessSelfSvc *service.BusinessSelfService
	var activationSvc service.ActivationService
	var walletPaymentSvc service.WalletPaymentReader
	var walletPaymentLister service.WalletPaymentLister
	// Hoisted so readiness can probe the same pool the service actually uses.
	// Probing a second, private connection would prove that PostgreSQL accepts
	// connections, not that THIS gateway can reach it — a distinction that
	// matters exactly when a pool is exhausted or misconfigured.
	var readinessDBPool *pgxpool.Pool
	var reqLogRecorder *service.PostgresRequestLogRecorder
	var bindingSeal *service.BindingSealService
	if cfg.DatabaseURL != "" {
		dbPool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("webhook db connect error", "error", err)
			os.Exit(1)
		}
		// Fail fast at boot on a mis-provisioned database rather than 500-ing on the
		// first request (startup half of migration governance — audit Part 4).
		validateGatewaySchema(ctx, dbPool, cfg.Environment)
		readinessDBPool = dbPool
		secretCipher, err := crypto.NewSecretCipher(cfg.WebhookEncryptionKey)
		if err != nil {
			slog.Error("webhook encryption key error", "error", err)
			os.Exit(1)
		}
		if secretCipher == nil {
			// A webhook signing secret is what proves an event came from Banzami.
			// Stored in plaintext, anyone with read access to the database can
			// forge signed events for every merchant endpoint. That is a dev-only
			// trade-off: in LIVE the gateway refuses to start, mirroring the
			// BZM_PROOF_SIGNING_KEY gate below.
			if service.NormaliseStackEnv(cfg.Environment) == "LIVE" {
				slog.Error("[SEC-002] WEBHOOK_ENCRYPTION_KEY not set in LIVE — refusing to start: webhook signing secrets would be stored in plaintext")
				os.Exit(1)
			}
			slog.Warn("[SEC-002] WEBHOOK_ENCRYPTION_KEY not set — webhook secrets stored in plaintext (dev/sandbox only)")
		}
		// env.Parse, not NormaliseStackEnv. The latter accepts "production" and
		// "staging" and maps them onto LIVE/SANDBOX, which is fine for a startup
		// gate but wrong for a value written into a financial row: a persisted
		// environment should record what the deployment declared, not what a
		// synonym table inferred from it.
		pgWebhook := service.NewPostgresWebhookService(dbPool, secretCipher, env.Parse(cfg.Environment))
		pgWebhook.StartWorker(ctx) // background delivery worker; stops on ctx cancel
		webhookSvc = pgWebhook
		teamSvc = service.NewPostgresTeamService(dbPool)
		credSvc := service.NewPostgresMerchantCredentialService(dbPool)
		// Optional cross-environment handle detection for login UX (ADR-025): a
		// read-only pool to the OTHER environment's DB lets a failed lookup report
		// "esta conta pertence ao ambiente X" instead of a misleading not-found.
		if cfg.CrossEnvDatabaseURL != "" {
			if crossPool, cerr := pgxpool.New(ctx, cfg.CrossEnvDatabaseURL); cerr != nil {
				slog.Warn("cross-env db connect failed — login env hint disabled", "error", cerr)
			} else {
				defer crossPool.Close()
				other := "LIVE"
				if service.NormaliseStackEnv(cfg.Environment) == "LIVE" {
					other = "SANDBOX"
				}
				credSvc.WithCrossEnvLookup(crossPool, other)
				slog.Info("cross-env handle detection enabled", "other_environment", other)
			}
		}
		merchantCredSvc = credSvc
		merchantAppSvc = service.NewPostgresMerchantApplicationService(dbPool)
		merchantAppAdminSvc = service.NewPostgresMerchantApplicationAdminService(dbPool, coreClient)
		activationSvc = service.NewPostgresActivationService(dbPool)
		walletPaymentService := service.NewPostgresWalletPaymentService(dbPool)
		walletPaymentSvc = walletPaymentService
		walletPaymentLister = walletPaymentService

		// KYB document storage (Track 3). Absent KYB_STORAGE_* → storage stays
		// nil and the document endpoints return 503 STORAGE_NOT_CONFIGURED.
		kybStore, kerr := kybstorage.NewFromConfig(kybstorage.Config{
			Provider:        cfg.KYBStorageProvider,
			Bucket:          cfg.KYBStorageBucket,
			Endpoint:        cfg.KYBStorageEndpoint,
			Region:          cfg.KYBStorageRegion,
			AccessKeyID:     cfg.KYBStorageAccessKeyID,
			SecretAccessKey: cfg.KYBStorageSecretKey,
			SignedURLTTL:    time.Duration(cfg.KYBSignedURLTTLSeconds) * time.Second,
		})
		if errors.Is(kerr, kybstorage.ErrNotConfigured) {
			slog.Warn("[Track 3] KYB_STORAGE_* not set — document storage disabled (endpoints return 503)")
			kybStore = nil
		} else if kerr != nil {
			slog.Error("[Track 3] KYB storage init error", "error", kerr)
			os.Exit(1)
		} else {
			slog.Info("[Track 3] KYB document storage configured", "bucket", cfg.KYBStorageBucket)
		}
		merchantDocumentSvc = service.NewPostgresMerchantDocumentService(dbPool, kybStore, cfg.KYBMaxFileSizeBytes)
		merchantKybSvc = service.NewPostgresMerchantKybService(dbPool, kybStore, cfg.KYBMaxFileSizeBytes)
		notificationsSvc = service.NewNotificationsService(dbPool)
		platformSvc = service.NewPlatformReadService(dbPool)
		businessSelfSvc = service.NewBusinessSelfService(dbPool)
		// Transaction-proof signatures are HMAC-keyed by BZM_PROOF_SIGNING_KEY.
		// An empty key makes signatures (and ip/ua hashes) forgeable/predictable,
		// so a LIVE stack must refuse to start without it; dev/sandbox may run
		// unkeyed with a loud warning.
		proofSigningKey := os.Getenv("BZM_PROOF_SIGNING_KEY")
		switch proofSigningKeyState(cfg.Environment, proofSigningKey) {
		case proofKeyFatal:
			slog.Error("[SEC-003] BZM_PROOF_SIGNING_KEY not set in LIVE — refusing to start: transaction-proof signatures would be forgeable")
			os.Exit(1)
		case proofKeyWarn:
			slog.Warn("[SEC-003] BZM_PROOF_SIGNING_KEY not set — transaction-proof signatures are unkeyed (dev/sandbox only)")
		}
		// Developer API request log (migration 0104). Asynchronous, bounded, and
		// pruned on its own retention — see postgres_request_logs.go.
		bindingSeal = service.NewBindingSealService(dbPool)
		reqLogRecorder = service.NewPostgresRequestLogRecorder(dbPool)
		reqLogRecorder.StartWorker(ctx)
		proofSvc = service.NewProofService(dbPool,
			proofSigningKey, os.Getenv("BZM_PROOF_KEY_ID"),
			"banzami", "banza", "https://banzami.com/r/")
		slog.Info("webhook + team services: postgres backend")
	} else {
		webhookSvc = service.NewStubWebhookService()
		teamSvc = service.NewStubTeamService()
		slog.Warn("webhook + team services: in-memory stub (DATABASE_URL not set)")
	}

	deps := server.Dependencies{
		Redis:                    rdb,
		DBPool:                   readinessDBPool,
		TransactionSvc:           service.NewCoreApiTransactionService(coreClient),
		WebhookSvc:               webhookSvc,
		MerchantSvc:              service.NewCoreApiMerchantService(coreClient),
		WalletSvc:                service.NewCoreApiWalletService(coreClient),
		ApplicationSettlementSvc: service.NewCoreApiApplicationSettlementService(coreClient),
		PayBaseURL:               cfg.PayBaseURL,
		WalletAccountSvc:         service.NewCoreApiWalletAccountService(coreClient),
		WalletAccountTransferSvc: service.NewCoreApiWalletAccountTransferService(coreClient),
		PartyResolverSvc:         service.NewCoreApiPartyResolver(coreClient),
		PaymentSessionSvc:        service.NewCoreApiPaymentSessionService(coreClient),
		PayoutSvc:                service.NewCoreApiPayoutService(coreClient),
		ConsumerSvc:              service.NewCoreApiConsumerService(coreClient),
		ConsumerWalletSvc:        service.NewCoreApiConsumerWalletService(coreClient),
		QrSvc:                    service.NewCoreApiQrService(coreClient),
		PaymentLinkSvc:           service.NewCoreApiPaymentLinkService(coreClient),
		CollectionSvc:            service.NewCoreApiCollectionService(coreClient),
		AcquiringSvc:             service.NewCoreApiAcquiringService(coreClient),
		RefundSvc:                service.NewCoreApiRefundService(coreClient),
		DisputeSvc:               service.NewCoreApiDisputeService(coreClient),
		PaymentRequestSvc:        service.NewCoreApiPaymentRequestService(coreClient),
		MerchantProfileSvc:       service.NewCoreApiMerchantProfileService(coreClient),
		ConsumerPayLinkSvc:       service.NewCoreApiConsumerPayLinkService(coreClient),
		FCMSvc:                   fcmSvc,
		TeamSvc:                  teamSvc,
		MerchantCredSvc:          merchantCredSvc,
		MerchantAppSvc:           merchantAppSvc,
		MerchantAppAdminSvc:      merchantAppAdminSvc,
		MerchantDocumentSvc:      merchantDocumentSvc,
		MerchantKybSvc:           merchantKybSvc,
		NotificationsSvc:         notificationsSvc,
		PlatformSvc:              platformSvc,
		ProofSvc:                 proofSvc,
		BusinessSelfSvc:          businessSelfSvc,
		RequestLogSink:           reqLogSink(reqLogRecorder),
		BindingSeal:              bindingSeal,
		ProofHashSalt:            proofHashSalt(),
		ActivationSvc:            activationSvc,
		ComplianceSvc:            service.NewCoreApiComplianceService(coreClient),
		WalletPaymentSvc:         walletPaymentSvc,
		WalletPaymentLister:      walletPaymentLister,
	}

	srv := server.New(cfg, deps)

	// Capture SIGINT / SIGTERM for graceful shutdown.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("api-gateway starting",
			"port", cfg.Port,
			"environment", cfg.Environment,
			"log_level", cfg.LogLevel,
			"log_format", cfg.LogFormat,
			"otlp_enabled", cfg.OTLPEndpoint != "",
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-sigCtx.Done()
	stop()
	slog.Info("shutdown signal received — draining requests")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}

	// Flush and shut down OTel providers (ensures all spans/metrics are exported).
	if err := shutdownOTel(shutdownCtx); err != nil {
		slog.Error("otel shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}

func initLogger(cfg *config.Config) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "pretty" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	// Wrap so every slog.*Context call carries correlation_id + request_id.
	slog.SetDefault(slog.New(obs.NewContextHandler(handler)))
}

// proofHashSalt salts the verification ip/ua hashes. Falls back to the proof
// signing key, then a non-secret default — raw IPs are never stored either way.
// requiredGatewayTables are the operator tables the gateway must have to function.
// A LIVE stack missing any of them is a mis-provisioned database, so it fails fast
// at boot instead of 500-ing on first request; dev/sandbox warn and continue. This
// is the startup half of migration governance (audit Part 4).
var requiredGatewayTables = []string{
	"merchants", "merchant_profiles", "merchant_applications", "handle_registry",
	"merchant_app_credentials", "merchant_activation_tokens", "merchant_kyb_documents",
	"kyc_cases", "transaction_proofs", "transaction_proof_verifications",
	"platform_settings", "app_settlements",
}

// reqLogSink converts the concrete recorder to the sink interface WITHOUT the
// typed-nil trap: assigning a nil *PostgresRequestLogRecorder straight into an
// interface yields a non-nil interface holding a nil pointer, and the middleware
// would then install itself and panic on the first request.
func reqLogSink(r *service.PostgresRequestLogRecorder) service.APIRequestLogSink {
	if r == nil {
		return nil
	}
	return r
}

func validateGatewaySchema(ctx context.Context, pool *pgxpool.Pool, env string) {
	sel := make([]string, len(requiredGatewayTables))
	for i, t := range requiredGatewayTables {
		sel[i] = fmt.Sprintf("to_regclass('public.%s') IS NOT NULL", t)
	}
	exists := make([]bool, len(requiredGatewayTables))
	dst := make([]any, len(requiredGatewayTables))
	for i := range exists {
		dst[i] = &exists[i]
	}
	if err := pool.QueryRow(ctx, "SELECT "+strings.Join(sel, ", ")).Scan(dst...); err != nil {
		slog.Error("[SCHEMA] startup schema validation query failed", "error", err)
		return
	}
	var missing []string
	for i, ok := range exists {
		if !ok {
			missing = append(missing, requiredGatewayTables[i])
		}
	}
	if len(missing) == 0 {
		slog.Info("[SCHEMA] startup validation passed", "required_tables", len(requiredGatewayTables))
		return
	}
	if strings.EqualFold(env, "LIVE") {
		slog.Error("[SCHEMA] required tables missing in LIVE — refusing to start (mis-provisioned database)", "missing", missing)
		os.Exit(1)
	}
	slog.Warn("[SCHEMA] required tables missing — continuing (dev/sandbox)", "missing", missing)
}

// proofKeyState classifies how main should react to a missing proof signing key.
type proofKeyState int

const (
	proofKeyOK    proofKeyState = iota // key present — nothing to do
	proofKeyWarn                       // key absent in dev/sandbox — warn, continue
	proofKeyFatal                      // key absent in LIVE — refuse to start
)

// proofSigningKeyState decides whether an empty BZM_PROOF_SIGNING_KEY is fatal.
// A live stack with no key is fatal because unkeyed HMAC proof signatures would
// be forgeable; any non-live environment may run unkeyed with a warning.
//
// The live test goes through service.NormaliseStackEnv, the single place that
// decides what counts as live. Matching the literal string "LIVE" instead made
// the guard fail OPEN for ENVIRONMENT=production (and "PROD"), which is the very
// spelling config.IsProduction() looks for — a production gateway would have
// started with forgeable payment proofs and only logged a warning.
func proofSigningKeyState(env, key string) proofKeyState {
	if key != "" {
		return proofKeyOK
	}
	if service.NormaliseStackEnv(env) == "LIVE" {
		return proofKeyFatal
	}
	return proofKeyWarn
}

func proofHashSalt() string {
	if s := os.Getenv("BZM_PROOF_SALT"); s != "" {
		return s
	}
	if s := os.Getenv("BZM_PROOF_SIGNING_KEY"); s != "" {
		return s
	}
	return "banzami-proof-salt"
}
