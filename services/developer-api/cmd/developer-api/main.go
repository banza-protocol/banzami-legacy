// Command developer-api is the authenticated Developer Platform management API
// (developer-api.banzami.com, ADR-033): Account Identity + Developer contexts
// (workspaces, projects, sandbox API keys). It owns no financial state.
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/banzami/banzami/services/common/env"
	"github.com/banzami/banzami/services/common/webhookprov"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	ce "github.com/banzami/banzami/services/common/email"
	"github.com/banzami/banzami/services/common/obs"
	"github.com/banzami/banzami/services/developer-api/internal/accountidentity"
	"github.com/banzami/banzami/services/developer-api/internal/config"
	"github.com/banzami/banzami/services/developer-api/internal/coreclient"
	"github.com/banzami/banzami/services/developer-api/internal/developer"
	"github.com/banzami/banzami/services/developer-api/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}
	initLogger(cfg)

	ctx := context.Background()

	// DB is optional at boot so the service can answer /health before the full
	// stack is provisioned; management endpoints require it and fail closed.
	var pool *pgxpool.Pool
	if cfg.DatabaseURL != "" {
		pool, err = pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("db connect error", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
	} else {
		slog.Warn("DATABASE_URL not set — using in-memory Account Identity store (local dev only)")
	}

	// Redis backs OTP rate limiting / resend cooldown; falls back to in-memory
	// for local dev (sandbox uses its own isolated Redis).
	var rdb *redis.Client
	if cfg.RedisURL != "" {
		opt, perr := redis.ParseURL(cfg.RedisURL)
		if perr != nil {
			slog.Error("invalid REDIS_URL", "error", perr)
			os.Exit(1)
		}
		rdb = redis.NewClient(opt)
		defer rdb.Close()
	}

	// Account Identity wiring.
	var store accountidentity.Store
	if pool != nil {
		store = accountidentity.NewPGStore(pool)
	} else {
		store = accountidentity.NewMemStore()
	}
	var limiter accountidentity.RateLimiter
	if rdb != nil {
		limiter = accountidentity.NewRedisLimiter(rdb)
	} else {
		limiter = accountidentity.NewMemLimiter()
	}
	if cfg.OTPPepper == "" || cfg.SessionSecret == "" {
		slog.Warn("OTP_PEPPER / SESSION_SECRET not set — auth fails closed until configured")
	}
	mailer := accountidentity.NewMailer(ce.Config{
		Provider: cfg.EmailProvider, DryRun: cfg.EmailDryRun, ResendAPIKey: cfg.ResendAPIKey,
		SMTPHost: cfg.SMTPHost, SMTPPort: cfg.SMTPPort, SMTPUser: cfg.SMTPUser, SMTPPassword: cfg.SMTPPassword,
		FromName: cfg.EmailFromName, FromAddress: cfg.EmailFromAddress, ReplyTo: cfg.EmailReplyTo,
		NoreplyName: cfg.EmailNoreplyName, NoreplyAddress: cfg.EmailNoreplyAddress,
	})
	svc := accountidentity.NewService(store, limiter, mailer, accountidentity.ServiceConfig{
		OTPPepper:     cfg.OTPPepper,
		SessionSecret: cfg.SessionSecret,
		SessionTTL:    time.Duration(cfg.SessionTTLHours) * time.Hour,
	})
	auth := accountidentity.NewHandlers(svc, cfg.ConsoleOrigin, cfg.SecureCookies())

	// Developer domain (workspaces, members, projects, sandbox API keys).
	var devStore developer.Store
	if pool != nil {
		devStore = developer.NewPGStore(pool, env.Parse(cfg.Environment))
	} else {
		devStore = developer.NewMemStore()
	}
	devSvc := developer.NewService(devStore, cfg.SessionSecret, cfg.APIKeyPepper, 0)
	// Wire the Core payee-validation boundary (ADR-047 §3). When unset, operator
	// binding fails closed — no binding is recorded on an unverified payee.
	if pv := coreclient.New(cfg.CoreAPIURL, cfg.CorePayeeValidationKey); pv != nil {
		devSvc.SetPayeeValidator(pv)
	} else {
		slog.Warn("CORE_API_URL / CORE_INTERNAL_KEY not set — project binding fails closed until configured")
	}
	// The Console's own refund (the first financial write this service makes).
	// Unset key → nil Refunder → the Console reports the capability as
	// unavailable rather than rendering a control that would fail when pressed.
	if rf := developer.NewCoreRefunder(coreclient.NewRefund(cfg.CoreAPIURL, cfg.CoreRefundKey)); rf != nil {
		devSvc.SetRefunder(rf)

		// Webhook signing secrets at rest. The Console creates endpoints now, so this
		// service writes the same column the gateway reads — and must protect it the
		// same way. Without a key the secret is stored in the clear, which is only
		// tolerable in a sandbox; anywhere else the service refuses to start rather
		// than persist a signing secret a database dump would hand over.
		if cfg.WebhookEncryptionKey != "" {
			cipher, err := webhookprov.NewSecretCipher(cfg.WebhookEncryptionKey)
			if err != nil {
				slog.Error("[SEC-002] WEBHOOK_ENCRYPTION_KEY is not a valid key", "error", err)
				os.Exit(1)
			}
			devSvc.SetWebhookCipher(cipher)
		} else if strings.EqualFold(cfg.Environment, "sandbox") || strings.EqualFold(cfg.Environment, "development") {
			slog.Warn("[SEC-002] WEBHOOK_ENCRYPTION_KEY not set — webhook signing secrets stored in plaintext (sandbox only)")
		} else {
			slog.Error("[SEC-002] WEBHOOK_ENCRYPTION_KEY not set outside sandbox — refusing to start")
			os.Exit(1)
		}
	} else {
		slog.Warn("CORE_API_URL / CORE_REFUND_KEY not set — Console refunds report as unavailable")
	}
	// Self-service Sandbox financial setup. A project created in the Console had
	// no financial owner and no way to get one that the developer could perform
	// or even see; this is what makes that step theirs. Sandbox-only, gated on
	// the deployment's own environment in the same fail-closed shape as fixtures.
	if pc := coreclient.NewProvision(cfg.CoreAPIURL); pc != nil {
		devSvc.SetSandboxProvisioner(developer.NewSandboxProvisioner(pc))
		// The same client opens segregated destinations: a developer should not
		// need to write code merely to get a Sandbox project into a usable shape.
		devSvc.SetWalletAccountProvisioner(pc)
	} else {
		slog.Warn("CORE_API_URL not set — sandbox financial setup reports as unavailable")
	}
	// Deploy-vs-release control (RT04C §1): logged without secrets. Config already
	// forces this false outside a sandbox environment.
	devSvc.SetPaymentCapabilityReleased(cfg.PaymentCapabilityReleased)
	// Operator E2E fixture-key path is hard-enabled ONLY in a sandbox/development
	// environment (RT04D §2); hard-disabled everywhere else regardless of config.
	// RA-055: fixtures are a Sandbox privilege. "development" is a separate
	// deployment-topology word and stays its own explicit condition rather than
	// being folded into the meaning of "sandbox".
	fixturesEnabled := env.Parse(cfg.Environment).IsSandbox() || cfg.IsDevelopment()
	devSvc.SetFixturesEnabled(fixturesEnabled)
	// Same gate as fixtures, for the same reason: a self-service path into a
	// real-money financial owner must not be reachable outside Sandbox.
	devSvc.SetSandboxEnvironment(env.Parse(cfg.Environment).IsSandbox() || cfg.IsDevelopment())
	slog.Info("payment capability release state", "released", cfg.PaymentCapabilityReleased, "fixtures", fixturesEnabled, "env", cfg.Environment)
	devH := developer.NewHandlers(devSvc)

	handler := server.New(cfg, server.Deps{Pool: pool, Auth: auth, Dev: devH})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("developer-api listening", "port", cfg.Port, "env", cfg.Environment)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}

func initLogger(cfg *config.Config) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var base slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(cfg.LogFormat) == "text" {
		base = slog.NewTextHandler(os.Stdout, opts)
	} else {
		base = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(obs.NewContextHandler(base)))
}
