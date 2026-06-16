package worker

import "testing"

// TestWorkerMirrorFor: a fungible worker keeps one BARE mirror PER repo (F9), and
// the path MUST be ".git"-suffixed so it never collides with a working-tree checkout
// at <dir>/<repo> (the #35 bug: ~/dev/russ is a working tree, and `git --git-dir
// <working-tree>` fails). --mirror is the mirrors directory.
func TestWorkerMirrorFor(t *testing.T) {
	// --mirror as a mirrors directory -> <dir>/<repo>.git
	if g := workerMirrorFor("/home/sam/.flowbee/mirrors", "https://github.com/samhotchkiss/russ.git"); g != "/home/sam/.flowbee/mirrors/russ.git" {
		t.Fatalf("mirrors-dir = %s, want /home/sam/.flowbee/mirrors/russ.git", g)
	}
	// a ".git" bare-mirror path -> use its PARENT dir (so it never nests russ.git inside flowbee.git)
	if g := workerMirrorFor("/tmp/x.git", "https://github.com/o/flowbee.git"); g != "/tmp/flowbee.git" {
		t.Fatalf("bare-path parent = %s, want /tmp/flowbee.git", g)
	}
	// the result is ALWAYS bare (.git) and never equals a working-tree path like ~/dev/<repo>
	g := workerMirrorFor("/home/sam/dev/flowbee", "https://github.com/samhotchkiss/russ.git")
	if g == "/home/sam/dev/russ" {
		t.Fatalf("must NOT return the working-tree path ~/dev/russ (the #35 collision): %s", g)
	}
	if len(g) < 4 || g[len(g)-4:] != ".git" {
		t.Fatalf("worker mirror must be a bare .git path, got %s", g)
	}
	// single-repo with no lease URL -> the configured bare mirror is used as-is.
	if g := workerMirrorFor("/srv/flowbee-mirror.git", ""); g != "/srv/flowbee-mirror.git" {
		t.Fatalf("single-repo configured bare mirror = %s, want as-is", g)
	}
}
