package liveness

import "testing"

// TestEvaluateKill_TruthTable walks every row of the §10.3 two-rung kill rule (the
// keystone unit test, BUILD.md §7.2 risk 8): every kill needs two independent rungs
// with at least one un-gameable (Rung-2 or Rung-3); the absolute cap is the lone
// unilateral kill; abstain is not a vote; two worker-reported rungs never kill.
func TestEvaluateKill_TruthTable(t *testing.T) {
	cases := []struct {
		name       string
		rs         RungSet
		wantKill   bool
		wantUnilat bool
	}{
		// §10.3 table rows:
		{
			name:     "Rung1 frozen + Rung0 stdin_block => NO (both worker-reported)",
			rs:       RungSet{Health: HealthStdinBlock, Rung1: Rung1Frozen, Rung2: Rung2Abstain},
			wantKill: false,
		},
		{
			name:     "Rung1 spinning + Rung2 no-net-diff => YES (worker busy, oracle confirms)",
			rs:       RungSet{Rung1: Rung1Spinning, Rung2: Rung2Stalled},
			wantKill: true,
		},
		{
			name:     "Rung0 cpu_spin + Rung3 soft-deadline => YES (clock corroborates)",
			rs:       RungSet{Health: HealthCPUSpin, Rung2: Rung2Abstain, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: true,
		},
		{
			name:     "Rung3 soft-deadline + Rung2 abstain => NO (abstain is not a vote)",
			rs:       RungSet{Rung2: Rung2Abstain, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: false,
		},
		{
			name:       "Rung3 absolute cap alone => YES (unilateral)",
			rs:         RungSet{Rung2: Rung2Abstain, Rung3: Rung3State{AbsoluteCap: true}},
			wantKill:   true,
			wantUnilat: true,
		},
		{
			// frozen-as-HUNG (a non-ok health) + soft deadline: the clock corroborates
			// the freeze -> YES.
			name:     "Rung1 frozen (hung) + Rung3 soft-deadline => YES (clock corroborates the freeze)",
			rs:       RungSet{Health: HealthHungChild, Rung1: Rung1Frozen, Rung2: Rung2Abstain, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: true,
		},
		{
			// Guardrail B (§10.4): frozen-while-OK reads as "thinking" / CI-waiting, NOT
			// a stall. Even past the soft deadline it must NOT corroborate a kill.
			name:     "Rung1 frozen + Rung0 ok + soft-deadline => NO (frozen != dead; thinking)",
			rs:       RungSet{Health: HealthOK, Rung1: Rung1Frozen, Rung2: Rung2Abstain, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: false,
		},

		// additional coverage:
		{
			name:     "Rung3 soft-deadline + Rung2 stalled => YES (two un-gameable rungs)",
			rs:       RungSet{Rung2: Rung2Stalled, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: true,
		},
		{
			name:     "Rung3 soft-deadline alone, healthy, Rung2 converging => NO",
			rs:       RungSet{Health: HealthOK, Rung1: Rung1Working, Rung2: Rung2Converging, Rung3: Rung3State{SoftCrossed: true}},
			wantKill: false,
		},
		{
			name:     "Rung2 stalled alone (no worker suspicion) => NO (lone rung)",
			rs:       RungSet{Health: HealthOK, Rung1: Rung1Working, Rung2: Rung2Stalled},
			wantKill: false,
		},
		{
			name:     "Rung1 frozen + Rung0 ok (thinking) => NO (frozen != dead)",
			rs:       RungSet{Health: HealthOK, Rung1: Rung1Frozen, Rung2: Rung2Abstain},
			wantKill: false,
		},
		{
			name:     "circuit breaker tripped suppresses a soft-deadline+stalled kill",
			rs:       RungSet{Rung2: Rung2Stalled, Rung3: Rung3State{SoftCrossed: true}, CircuitBreakerTripped: true},
			wantKill: false,
		},
		{
			name:       "circuit breaker does NOT suppress the absolute cap",
			rs:         RungSet{Rung2: Rung2Abstain, Rung3: Rung3State{AbsoluteCap: true}, CircuitBreakerTripped: true},
			wantKill:   true,
			wantUnilat: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateKill(c.rs)
			if got.Kill != c.wantKill {
				t.Fatalf("Kill=%v want %v (reason=%q)", got.Kill, c.wantKill, got.Reason)
			}
			if got.Unilateral != c.wantUnilat {
				t.Fatalf("Unilateral=%v want %v", got.Unilateral, c.wantUnilat)
			}
		})
	}
}

// TestEvaluateKill_NoTwoWorkerReportedKill proves the invariant directly: no
// combination of ONLY worker-reported rungs (Rung-0 + Rung-1, any values, Rung-2
// abstaining, no Rung-3 crossing) can ever kill. This is the I-13 anti-lying floor.
func TestEvaluateKill_NoTwoWorkerReportedKill(t *testing.T) {
	healths := []AgentHealth{HealthOK, HealthStdinBlock, HealthCPUSpin, HealthOOM, HealthHungChild}
	rung1s := []Rung1Class{Rung1Working, Rung1Frozen, Rung1Spinning}
	for _, h := range healths {
		for _, r1 := range rung1s {
			rs := RungSet{Health: h, Rung1: r1, Rung2: Rung2Abstain}
			if EvaluateKill(rs).Kill {
				t.Fatalf("worker-reported-only rungs killed: health=%s rung1=%s", h, r1)
			}
		}
	}
}

// TestEvaluateFastPath covers the two §10.6 free fast-paths.
func TestEvaluateFastPath(t *testing.T) {
	if got := EvaluateFastPath(HealthOK, false, false); got != FastPathNone {
		t.Fatalf("healthy => none, got %s", got)
	}
	if got := EvaluateFastPath(HealthOK, true, false); got != FastPathCancel {
		t.Fatalf("awaiting_input => cancel, got %s", got)
	}
	if got := EvaluateFastPath(HealthOK, false, true); got != FastPathFailed {
		t.Fatalf("agent exited => failed, got %s", got)
	}
	if got := EvaluateFastPath(HealthZombie, false, false); got != FastPathFailed {
		t.Fatalf("zombie health => failed, got %s", got)
	}
	// a dead agent that is also nominally awaiting input fast-paths to failed.
	if got := EvaluateFastPath(HealthZombie, true, true); got != FastPathFailed {
		t.Fatalf("exited takes precedence => failed, got %s", got)
	}
}
