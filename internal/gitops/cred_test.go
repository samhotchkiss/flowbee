package gitops

import "testing"

// TestGitCredFor: a token-bearing https url is split into a CLEAN url (no token) + a
// credential-helper arg that reads the token from the environment, so the token never
// lands in argv. SSH and already-clean urls pass through with no helper.
func TestGitCredFor(t *testing.T) {
	clean, cred := gitCredFor("https://x-access-token:ghp_SECRET123@github.com/acme/widgets.git")
	if clean != "https://github.com/acme/widgets.git" {
		t.Fatalf("clean url = %q, want the token stripped", clean)
	}
	if len(cred) != 2 || cred[0] != "-c" {
		t.Fatalf("cred args = %v, want [-c credential.helper=...]", cred)
	}
	// the token must NOT appear in the args (it comes from the env helper).
	for _, a := range append([]string{clean}, cred...) {
		if a == "ghp_SECRET123" || (len(a) > 20 && contains(a, "ghp_SECRET123")) {
			t.Fatalf("token leaked into args: %q", a)
		}
	}
	// SSH passes through untouched.
	if c, cr := gitCredFor("git@github.com:acme/widgets.git"); c != "git@github.com:acme/widgets.git" || cr != nil {
		t.Fatalf("ssh url must pass through: clean=%q cred=%v", c, cr)
	}
	// already-clean https passes through.
	if c, cr := gitCredFor("https://github.com/acme/widgets.git"); cr != nil {
		t.Fatalf("clean https must pass through: clean=%q cred=%v", c, cr)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
