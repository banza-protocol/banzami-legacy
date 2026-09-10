// GET /v1/me is the whole released developer-key surface, so what it names a
// project is the name every integration will store.
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/banzami/banzami/services/api-gateway/internal/middleware"
)

const testProjectUUID = "6367749d-aaaa-4bbb-8ccc-ddddeeeeffff"

func getMe(p *middleware.DeveloperPrincipal) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	if p != nil {
		req = req.WithContext(middleware.ContextWithDeveloperPrincipal(req.Context(), p))
	}
	rec := httptest.NewRecorder()
	NewMeHandler().Me(rec, req)
	return rec
}

func devPrincipal(slug string) *middleware.DeveloperPrincipal {
	return &middleware.DeveloperPrincipal{
		Environment: "SANDBOX",
		WorkspaceID: "ws-internal-uuid",
		ProjectID:   testProjectUUID,
		ProjectSlug: slug,
		KeyStatus:   "ACTIVE",
		Scopes:      []string{"identity:read"},
	}
}

// The slug is what a developer typed and can retype. Renaming a project used to
// change the only identifier /v1/me published, so anything that had filed
// records under it lost the link.
func TestMe_ProjectIdSurvivesARename(t *testing.T) {
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	before := decode(getMe(devPrincipal("doa-sandbox")))
	after := decode(getMe(devPrincipal("doa-producao")))

	if before["project"] == after["project"] {
		t.Fatal("precondition: the slug should have changed")
	}
	id, _ := before["project_id"].(string)
	if id == "" {
		t.Fatal("/v1/me publishes no stable project identifier")
	}
	if after["project_id"] != id {
		t.Fatalf("project_id changed on rename: %v → %v", id, after["project_id"])
	}
}

// The contract's own stated rule: no internal Core/database identifiers.
func TestMe_PublishesNoInternalIdentifiers(t *testing.T) {
	rec := getMe(devPrincipal("doa-sandbox"))
	raw := rec.Body.String()
	for _, leaked := range []string{
		testProjectUUID,
		strings.ReplaceAll(testProjectUUID, "-", ""),
		"ws-internal-uuid",
		"workspace_id",
	} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("/v1/me leaked %q: %s", leaked, raw)
		}
	}
}

func TestMe_RequiresIdentityReadScope(t *testing.T) {
	p := devPrincipal("doa-sandbox")
	p.Scopes = []string{"payments:write"}
	if rec := getMe(p); rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 without identity:read, got %d", rec.Code)
	}
	if rec := getMe(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 unauthenticated, got %d", rec.Code)
	}
}
