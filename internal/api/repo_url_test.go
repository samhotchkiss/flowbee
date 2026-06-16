package api

import "testing"

// TestWorkerRepoURL: the lease ships SSH or HTTPS per the fleet's auth, and NEVER
// embeds a credential (the worker authenticates with its own key/helper).
func TestWorkerRepoURL(t *testing.T) {
	if g := workerRepoURL("samhotchkiss", "russ", true); g != "git@github.com:samhotchkiss/russ.git" {
		t.Fatalf("ssh = %s", g)
	}
	if g := workerRepoURL("samhotchkiss", "russ", false); g != "https://github.com/samhotchkiss/russ.git" {
		t.Fatalf("https = %s", g)
	}
	for _, ssh := range []bool{true, false} {
		if g := workerRepoURL("o", "r", ssh); containsToken(g) {
			t.Fatalf("URL must never embed a token: %s", g)
		}
	}
}

func containsToken(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ':' && i > 6 && s[i+1] != '/' { // crude: "scheme://user:tok@" form
			// allow "git@" and "https://"; flag an embedded "...:token@..."
			rest := s[i:]
			for j := 1; j < len(rest); j++ {
				if rest[j] == '@' {
					return true
				}
				if rest[j] == '/' {
					break
				}
			}
		}
	}
	return false
}
