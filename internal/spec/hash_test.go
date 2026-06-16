package spec

import "testing"

// TestContentHashDeterministicAndSensitive: the BLAKE3 content hash is PURE
// (same bytes -> same hash) and SENSITIVE (a single-byte edit moves the hash) —
// the property that makes spec supersession mechanical and total (§11.5).
func TestContentHashDeterministicAndSensitive(t *testing.T) {
	a := ContentHash([]byte("# spec\nbody\n"))
	b := ContentHash([]byte("# spec\nbody\n"))
	if a != b {
		t.Fatalf("identical bytes must hash identically: %q vs %q", a, b)
	}
	if a[:7] != "blake3:" {
		t.Fatalf("hash must carry the blake3: prefix, got %q", a)
	}
	c := ContentHash([]byte("# spec\nbody!\n")) // one byte changed
	if a == c {
		t.Fatal("a single-byte edit must change the content hash (mechanical supersession)")
	}
	if ContentHash(nil) == a {
		t.Fatal("empty vs non-empty must differ")
	}
}
