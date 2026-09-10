// A Project's public identifier has to survive the developer renaming it.
//
// /v1/me reported only the slug, which is the one thing a developer can change,
// so an integration that filed its own records under it lost the link on the
// first rename. And the identifier must not be the internal UUID: the same
// contract already withholds those, because a database key published as a
// public contract can never be re-keyed.
package handler

import (
	"strings"
	"testing"
)

func TestPublicProjectID_IsStableAndOpaque(t *testing.T) {
	const uuid = "6367749d-1111-4222-8333-444455556666"
	id := PublicProjectID(uuid)

	if id != PublicProjectID(uuid) {
		t.Fatal("the same project must always derive the same id")
	}
	if !strings.HasPrefix(id, "proj_") {
		t.Fatalf("id %q is not recognisably a project id", id)
	}
	// Nothing of the internal identifier may survive into the public one.
	if strings.Contains(id, uuid) || strings.Contains(id, strings.ReplaceAll(uuid, "-", "")) {
		t.Fatalf("the internal id leaked into %q", id)
	}
	for _, part := range strings.Split(uuid, "-") {
		if len(part) >= 8 && strings.Contains(id, part) {
			t.Fatalf("a fragment of the internal id leaked into %q", id)
		}
	}
}

func TestPublicProjectID_DistinctProjectsDistinctIds(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{
		"6367749d-1111-4222-8333-444455556666",
		"6367749d-1111-4222-8333-444455556667", // one character apart
		"00000000-0000-0000-0000-000000000000",
		"a", "ab",
	} {
		id := PublicProjectID(p)
		if prev, dup := seen[id]; dup {
			t.Fatalf("%q and %q derive the same public id %q", prev, p, id)
		}
		seen[id] = p
	}
}

// An absent project is absent. Hashing the empty string would give every
// unbound key the same id and make them all look like one project.
func TestPublicProjectID_EmptyStaysEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		if got := PublicProjectID(in); got != "" {
			t.Fatalf("PublicProjectID(%q) = %q, want empty", in, got)
		}
	}
}
