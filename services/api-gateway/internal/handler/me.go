package handler

// GET /v1/me — the released developer-key consumption surface (ADR-046).
// Authenticated by a Console-issued Sandbox API key. Returns the resolved
// identity context and nothing else (no financial state). Proves the end-to-end
// path: Console key → Gateway auth → resolved context → public Sandbox API.

import (
	"net/http"

	"github.com/banzami/banzami/services/api-gateway/internal/apierror"
	"github.com/banzami/banzami/services/api-gateway/internal/middleware"
)

// MeHandler serves the developer identity endpoint.
type MeHandler struct{}

func NewMeHandler() *MeHandler { return &MeHandler{} }

// Me returns the authenticated key's environment, workspace, project and scopes.
func (h *MeHandler) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := middleware.GetDeveloperPrincipal(r.Context())
	if !ok {
		apierror.Respond(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	// Least-privilege: GET /v1/me requires the identity:read scope (the only
	// scope backed by a released route). A key without it is neutrally denied.
	if !p.HasScope("identity:read") {
		apierror.Respond(w, r, http.StatusForbidden, "FORBIDDEN", "insufficient scope")
		return
	}
	// Public contract (RT02.1): the MINIMUM integration identity only — API
	// environment, project-safe identifier, allowed scopes, key status. It never
	// exposes internal Core/database identifiers (workspace_id, project UUID, key
	// UUID), workspace members, service topology, PII, or raw key material.
	writeJSON(w, http.StatusOK, map[string]any{
		"environment": p.Environment,
		"project":     p.ProjectSlug,
		// The slug is what a developer typed and can retype. project_id does not
		// move when they rename the project, so an integration has something
		// stable to file its own records under. Derived, not the internal UUID —
		// see PublicProjectID.
		"project_id": PublicProjectID(p.ProjectID),
		"scopes":     p.Scopes,
		"key_status": p.KeyStatus,
	})
}
