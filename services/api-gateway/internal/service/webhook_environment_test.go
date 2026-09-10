// A webhook endpoint belongs to exactly one financial universe.
//
// webhook_endpoints.environment carries DEFAULT 'LIVE'. The registration path
// omitted the column, so every Sandbox endpoint was stored as a LIVE subscriber
// — the wrong record, and the wrong dispatch set.
//
// The fix has two halves and both need proving: the value comes from the process
// configuration, and a process that cannot name its environment refuses to write
// at all rather than inheriting the default.
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/banzami/banzami/services/common/env"
)

// An undeclared environment must stop the write before anything is generated —
// no secret, no row, no database call. The pool is deliberately nil: if the guard
// is removed, this test panics rather than silently passing, which is the louder
// of the two failures.
func TestRegisterEndpoint_RefusesWhenTheEnvironmentIsUndeclared(t *testing.T) {
	for _, raw := range []string{"", "production", "development", "staging", "prod"} {
		t.Run(raw, func(t *testing.T) {
			parsed := env.Parse(raw)
			if parsed.IsKnown() {
				t.Fatalf("precondition: %q must not parse to a financial environment", raw)
			}
			svc := &PostgresWebhookService{pool: nil, environment: parsed}
			_, err := svc.RegisterEndpoint(context.Background(), RegisterEndpointRequest{
				MerchantID: "11111111-1111-1111-1111-111111111111",
				URL:        "https://merchant.example.com/hooks",
				Events:     []string{"payment.succeeded"},
			})
			if !errors.Is(err, ErrEnvironmentUndeclared) {
				t.Fatalf("want ErrEnvironmentUndeclared, got %v", err)
			}
		})
	}
}

// The declared environment is what gets written. Proven at the boundary this
// package owns: the value the writer would bind.
func TestServiceEnvironment_IsTheCanonicalWireValue(t *testing.T) {
	for raw, want := range map[string]string{
		"SANDBOX": "SANDBOX",
		"sandbox": "SANDBOX", // the deployment sets lowercase; the row must not
		"LIVE":    "LIVE",
	} {
		svc := &PostgresWebhookService{environment: env.Parse(raw)}
		if got := svc.environment.String(); got != want {
			t.Fatalf("ENVIRONMENT=%q → %q, want %q", raw, got, want)
		}
	}
}

// NormaliseStackEnv and env.Parse deliberately disagree, and the writer uses the
// stricter one. This records that on purpose: a startup gate may infer LIVE from
// "production", but a persisted financial row must say what the deployment
// declared, not what a synonym table concluded.
func TestPersistedEnvironmentIsStricterThanTheStartupGate(t *testing.T) {
	if NormaliseStackEnv("production") != "LIVE" {
		t.Fatal("precondition: the startup gate treats production as LIVE")
	}
	if env.Parse("production").IsKnown() {
		t.Fatal("a persisted environment must not be inferred from \"production\"")
	}
}
