package acctprobe

import "testing"

func TestMarkIntegrityDuplicateIdentity(t *testing.T) {
	// two config dirs resolving to the same claude account (same AccountKey) must both
	// be held — routing two "slots" that are one login double-counts capacity.
	a := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-1", ConfigDir: "/a"}, TrustState: TrustVerified}
	b := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-1", ConfigDir: "/b"}, TrustState: TrustVerifiedLocal}
	c := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-2", ConfigDir: "/c"}, TrustState: TrustVerified}
	// same AccountKey STRING but a different provider is not a collision.
	d := &Result{Identity: Identity{Provider: ProviderCodex, AccountKey: "acct-1", ConfigDir: "/d"}, TrustState: TrustVerified}

	warnings := MarkIntegrity([]*Result{a, b, c, d})
	if len(warnings) != 1 {
		t.Fatalf("warnings=%v want exactly 1 (the claude dup)", warnings)
	}
	for _, r := range []*Result{a, b} {
		if r.TrustState != TrustHeld || r.Hold != ReasonDuplicateIdentity {
			t.Errorf("dup %q not held: trust=%v hold=%v", r.Identity.ConfigDir, r.TrustState, r.Hold)
		}
		if r.Routable() {
			t.Errorf("dup %q must not be routable", r.Identity.ConfigDir)
		}
	}
	// a distinct fingerprint and a different provider with the same fp string are NOT
	// collisions.
	if c.TrustState != TrustVerified || d.TrustState != TrustVerified {
		t.Errorf("non-colliding results were altered: c=%v d=%v", c.TrustState, d.TrustState)
	}
}

// TestMarkIntegrityTwoSeatsOneOrgNotDuplicate is the M3 guard: two DISTINCT accounts
// that are seats in ONE org (same Org/OrgKey, different AccountKey) must NOT collide —
// keying on the org would falsely hold healthy team capacity fleet-wide.
func TestMarkIntegrityTwoSeatsOneOrgNotDuplicate(t *testing.T) {
	orgKey := "org-team-shared-0001"
	a := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-alice", OrgKey: orgKey, Fingerprint: fingerprint("acct-alice"), ConfigDir: "/a"}, TrustState: TrustVerified}
	b := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-bob", OrgKey: orgKey, Fingerprint: fingerprint("acct-bob"), ConfigDir: "/b"}, TrustState: TrustVerified}
	if w := MarkIntegrity([]*Result{a, b}); len(w) != 0 {
		t.Fatalf("two seats in one org must not be flagged duplicate: %v", w)
	}
	if a.TrustState != TrustVerified || b.TrustState != TrustVerified {
		t.Errorf("distinct-account seats were wrongly held: a=%v b=%v", a.TrustState, b.TrustState)
	}
}

func TestMarkIntegritySkipsUnboundIdentity(t *testing.T) {
	// results with no AccountKey (identity unbindable) must never collide with each
	// other into a false duplicate.
	a := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: ""}, TrustState: TrustHeld}
	b := &Result{Identity: Identity{Provider: ProviderClaude, AccountKey: ""}, TrustState: TrustHeld}
	if w := MarkIntegrity([]*Result{a, b}); len(w) != 0 {
		t.Errorf("unbound identities must not be flagged as duplicates: %v", w)
	}
}
