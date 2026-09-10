// The Console's store writes rows into environment-scoped tables, so it has to
// know which universe it is in.
//
// webhook_endpoints.environment defaults to 'LIVE'. A Console-created endpoint
// that omitted the column was recorded as a real-money subscriber. Refusing is
// the correct alternative to guessing: registering a webhook is configuration,
// not a payment, and nothing is lost by making the operator declare ENVIRONMENT.
package developer

import (
	"context"
	"errors"
	"testing"

	"github.com/banzami/banzami/services/common/env"
)

func TestCreateWebhookEndpoint_RefusesAnUndeclaredEnvironment(t *testing.T) {
	// pool is nil on purpose: with the guard removed this panics instead of
	// quietly passing.
	for _, raw := range []string{"", "production", "development"} {
		s := &pgStore{pool: nil, environment: env.Parse(raw)}
		_, err := s.CreateWebhookEndpoint(context.Background(),
			"11111111-1111-1111-1111-111111111111",
			"https://merchant.example.com/hooks",
			[]string{"payment.succeeded"}, "stored-secret")
		if !errors.Is(err, ErrEnvironmentUndeclared) {
			t.Fatalf("ENVIRONMENT=%q: want ErrEnvironmentUndeclared, got %v", raw, err)
		}
	}
}

func TestStoreEnvironment_IsCanonicalRegardlessOfDeploymentCasing(t *testing.T) {
	for raw, want := range map[string]string{"sandbox": "SANDBOX", "SANDBOX": "SANDBOX", "LIVE": "LIVE"} {
		if got := (&pgStore{environment: env.Parse(raw)}).environment.String(); got != want {
			t.Fatalf("ENVIRONMENT=%q → %q, want %q", raw, got, want)
		}
	}
}
