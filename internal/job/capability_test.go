package job

import "testing"

func TestCapabilitiesSatisfy(t *testing.T) {
	cases := []struct {
		name     string
		attested []string
		required []string
		want     bool
	}{
		{"no requirement", []string{"role:eng_worker"}, nil, true},
		{"exact match", []string{"role:eng_worker", "model_family:codex"},
			[]string{"role:eng_worker", "model_family:codex"}, true},
		{"missing tag", []string{"role:eng_worker"},
			[]string{"role:eng_worker", "model_family:codex"}, false},
		{"wrong role", []string{"role:code_reviewer"}, []string{"role:eng_worker"}, false},
		{"wildcard family satisfied", []string{"role:eng_worker", "model_family:opus"},
			[]string{"role:eng_worker", "model_family:*"}, true},
		{"wildcard family unsatisfied", []string{"role:eng_worker"},
			[]string{"model_family:*"}, false},
		{"extra attested ignored", []string{"role:eng_worker", "arch:x86_64", "model_family:codex"},
			[]string{"role:eng_worker"}, true},
	}
	for _, c := range cases {
		if got := CapabilitiesSatisfy(c.attested, c.required); got != c.want {
			t.Errorf("%s: CapabilitiesSatisfy(%v,%v)=%v want %v", c.name, c.attested, c.required, got, c.want)
		}
	}
}

// TestDAGTransitions exercises the M2 state-machine edges.
func TestDAGTransitions(t *testing.T) {
	if got, err := Next(StateBlocked, TriggerDepsCleared); err != nil || got != StateReady {
		t.Fatalf("blocked->deps_cleared = %s,%v want ready", got, err)
	}
	if got, err := Next(StateReviewPending, TriggerCompleted); err != nil || got != StateDone {
		t.Fatalf("review_pending->completed = %s,%v want done", got, err)
	}
	if _, err := Next(StateReady, TriggerCompleted); err == nil {
		t.Fatal("ready->completed must be illegal")
	}
}
