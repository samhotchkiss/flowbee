package worker

import "testing"

// TestWorkerMirrorFor: a fungible worker keeps one mirror PER repo (F9). Given the
// job's repo URL, the mirror is a sibling of the configured --mirror; with no repo
// URL it falls back to the configured single-repo path.
func TestWorkerMirrorFor(t *testing.T) {
	if g := workerMirrorFor("/home/sam/dev/flowbee", "https://github.com/samhotchkiss/russ.git"); g != "/home/sam/dev/russ" {
		t.Fatalf("multi-repo sibling = %s, want /home/sam/dev/russ", g)
	}
	if g := workerMirrorFor("/home/sam/dev/flowbee", ""); g != "/home/sam/dev/flowbee" {
		t.Fatalf("single-repo (no lease URL) = %s, want the configured path", g)
	}
	if g := workerMirrorFor("", "https://github.com/o/myrepo.git"); len(g) < 6 || g[len(g)-6:] != "myrepo" {
		t.Fatalf("temp-base derivation = %s, want .../myrepo", g)
	}
}
