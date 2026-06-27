package project

import (
	"errors"
	"testing"
)

// TestIsUnreachableRev locks the permanent-vs-transient distinction the autonomous-merge
// verify relies on: a missing-object / bad-range git error (a squash-discarded epoch SHA)
// fails-open to handoff, while a transient network/lock error keeps retrying.
func TestIsUnreachableRev(t *testing.T) {
	unreachable := []string{
		"diff 6bf9..390e: exit status 128: fatal: Invalid revision range 6bf9..390e",
		"diff a..b: exit status 128: fatal: bad revision 'a..b'",
		"fatal: ambiguous argument '95126141': unknown revision or path not in the working tree",
		"fatal: bad object deadbeef",
	}
	for _, m := range unreachable {
		if !isUnreachableRev(errors.New(m)) {
			t.Errorf("expected UNREACHABLE for %q", m)
		}
	}
	transient := []string{
		"fetch origin: exit status 128: fatal: unable to access 'https://github.com/...': Could not resolve host",
		"diff a..b: exit status 128: fatal: Unable to create '.git/index.lock': File exists",
		"context deadline exceeded",
	}
	for _, m := range transient {
		if isUnreachableRev(errors.New(m)) {
			t.Errorf("expected TRANSIENT (retry) for %q", m)
		}
	}
	if isUnreachableRev(nil) {
		t.Error("nil error must not be unreachable")
	}
}
