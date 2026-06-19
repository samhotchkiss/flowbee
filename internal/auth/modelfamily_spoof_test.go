package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestModelFamilyBoundToEnrolledIdentity is the regression lock for the I-10
// anti-affinity defeat (audit finding: a worker could self-assert ANY model_family to
// slip past the §5.5 same-family exclusion). The fix binds model_family to the enrolled
// identity: an "identity:family" enrolled entry makes FamilyFor authoritative, and
// Middleware stashes it so the lease handler clamps the self-asserted query param.
func TestModelFamilyBoundToEnrolledIdentity(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"box.one:codex", "box.two"}, false)

	// the bound family is authoritative; an un-bound entry stays unconstrained (legacy).
	if f, ok := a.FamilyFor("box.one"); !ok || f != "codex" {
		t.Fatalf("FamilyFor(box.one) = %q,%v want codex,true", f, ok)
	}
	if f, ok := a.FamilyFor("box.two"); ok || f != "" {
		t.Fatalf("FamilyFor(box.two) = %q,%v want \"\",false (no family declared)", f, ok)
	}

	// end-to-end clamp: box.one authenticates and self-asserts model_family=opus on the
	// SAME request. The handler must see the credential-bound "codex", never the spoof.
	tok := a.Mint("box.one")
	for _, spoof := range []string{"opus", "gemini", "anything"} {
		r := req(t, "100.64.0.2:5555", "Bearer "+tok, "box.one")
		q := r.URL.Query()
		q.Set("model_family", spoof)
		r.URL.RawQuery = q.Encode()

		var seen string
		var sawFamily bool
		h := Middleware(a, http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
			// emulate the lease handler's clamp: start from the query param, override with
			// the credential-bound family when present (server.go does exactly this).
			fam := rr.URL.Query().Get("model_family")
			if bound, ok := FamilyFrom(rr); ok {
				fam = bound
				sawFamily = true
			}
			seen = fam
		}))
		h.ServeHTTP(httptest.NewRecorder(), r)

		if !sawFamily {
			t.Fatalf("spoof=%q: no credential-bound family stashed — clamp would not engage", spoof)
		}
		if seen != "codex" {
			t.Fatalf("spoof=%q: handler saw family=%q, want the bound codex (spoof not clamped)", spoof, seen)
		}
	}
}

// TestModelFamilyUnboundIdentityFallsThrough: an enrolled identity with NO declared
// family leaves model_family worker-asserted (backward compatible — the operator simply
// hasn't opted that identity into the binding). No family is stashed.
func TestModelFamilyUnboundIdentityFallsThrough(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"box.two"}, false)
	tok := a.Mint("box.two")
	r := req(t, "100.64.0.2:5555", "Bearer "+tok, "box.two")
	q := r.URL.Query()
	q.Set("model_family", "whatever")
	r.URL.RawQuery = q.Encode()

	h := Middleware(a, http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		if _, ok := FamilyFrom(rr); ok {
			t.Fatalf("unbound identity must stash no family")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), r)
}
