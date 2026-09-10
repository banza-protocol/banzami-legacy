package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// A Project's stable public name.
//
// `/v1/me` reported only the project SLUG, which is what a developer typed and
// what they can retype: renaming a project changed the only identifier an
// integration had, so anything that had filed records under it lost the link.
// A public surface needs an identifier that does not move.
//
// It is DERIVED rather than being the project's UUID, for the reason the same
// contract already gives for withholding one: a database key published as a
// public contract can never be re-keyed, and an integrator who receives one
// starts using it to address things the platform did not mean to expose. The
// derivation is the pattern already used for a Business's Sandbox handle — a
// hash, so distinct projects get distinct ids and nothing about the internal id
// is recoverable from the public one.
//
// Deterministic and total: the same project always derives the same id, and an
// operator answering a support question can find it by hashing candidates.
const publicProjectIDPrefix = "proj_"

// PublicProjectID returns the stable public identifier for a project.
//
// Returns "" for an empty project id rather than the hash of nothing — an
// absent project is absent, and publishing a constant id for it would make
// every unbound key look like the same project.
func PublicProjectID(projectID string) string {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return publicProjectIDPrefix + hex.EncodeToString(sum[:])[:24]
}
