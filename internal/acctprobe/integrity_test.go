package acctprobe

import "testing"

func TestMarkIntegrityDuplicateIdentity(t *testing.T) {
	// two config dirs resolving to the same claude account (same fingerprint) must
	// both be held — routing two "slots" that are one login double-counts capacity.
	a := &Result{Identity: Identity{Provider: ProviderClaude, Fingerprint: "same-fp", ConfigDir: "/a"}, TrustState: TrustVerified}
	b := &Result{Identity: Identity{Provider: ProviderClaude, Fingerprint: "same-fp", ConfigDir: "/b"}, TrustState: TrustVerifiedLocal}
	c := &Result{Identity: Identity{Provider: ProviderClaude, Fingerprint: "other-fp", ConfigDir: "/c"}, TrustState: TrustVerified}
	d := &Result{Identity: Identity{Provider: ProviderCodex, Fingerprint: "same-fp", ConfigDir: "/d"}, TrustState: TrustVerified}

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

func TestMarkIntegritySkipsUnboundIdentity(t *testing.T) {
	// results with no fingerprint (identity unbindable) must never collide with each
	// other into a false duplicate.
	a := &Result{Identity: Identity{Provider: ProviderClaude, Fingerprint: ""}, TrustState: TrustHeld}
	b := &Result{Identity: Identity{Provider: ProviderClaude, Fingerprint: ""}, TrustState: TrustHeld}
	if w := MarkIntegrity([]*Result{a, b}); len(w) != 0 {
		t.Errorf("unbound identities must not be flagged as duplicates: %v", w)
	}
}
