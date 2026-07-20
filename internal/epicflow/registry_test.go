package epicflow

import "testing"

func TestEveryNonTerminalStateHasNextActionOrVisibleHold(t *testing.T) {
	if err := ValidateRegistry(); err != nil {
		t.Fatal(err)
	}
	for _, p := range Registry {
		if p.VisibleHold && p.State != "paused" && p.State != "needs_human" {
			t.Fatalf("normal state %s cannot masquerade as a human hold", p.State)
		}
	}
}
