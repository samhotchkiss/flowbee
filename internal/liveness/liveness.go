// Package liveness is the deterministic-core realization of the §10 stall ladder
// and the two-rung kill rule (I-13). It is PURE (DESIGN §1.2): it imports no clock,
// randomness, ID minter, GitHub, or LLM package — the rung observations are folded
// from persisted facts by the runtime and passed IN as values, and EvaluateKill is
// a pure function of a RungSet. archcheck enforces this boundary.
//
// The ladder is ordered cheapest/most-gameable -> slowest/un-gameable, with kill
// authority concentrated at the un-gameable end (§10.1). Cheap rungs SUSPECT; only
// expensive rungs CONDEMN, and even they need a corroborating second opinion — the
// two-rung kill rule:
//
//	A kill (revoking a presumed-stalled agent's lease) requires TWO independent
//	rungs in agreement, and at least one must be Rung-2 (external) or Rung-3
//	(clock). Never two worker-reported rungs. (§10.3)
//
// The ONLY unilateral kill is the Rung-3 absolute lease cap. The two free
// fast-paths (awaiting_input -> cancel, agent_exited_zombie -> failed, §10.6) are
// handled separately on the heartbeat path; they are not "kills" (a kill is a lease
// revocation; the fast-paths are an honest cancel / an already-dead agent).
package liveness

// AgentHealth is the Rung-0 worker-local supervisor enum (§10.2). It is the richest
// and LEAST trustworthy signal (it lives inside the untrusted box) — a HINT only,
// with the single exception of the locally-provable agent_exited_zombie fast-path
// (handled on the heartbeat path, not here).
type AgentHealth string

const (
	HealthOK         AgentHealth = "ok"
	HealthZombie     AgentHealth = "zombie"
	HealthStdinBlock AgentHealth = "stdin_block"
	HealthCPUSpin    AgentHealth = "cpu_spin"
	HealthOOM        AgentHealth = "oom"
	HealthHungChild  AgentHealth = "hung_child"
	HealthUnknown    AgentHealth = ""
)

// Rung1Class is the corroboration-gated progress classification (§10.2). It is
// worker-reported (gameable): a clever stuck agent makes its vector advance forever,
// so Rung-1 NEVER kills alone. `spinning` means "Rung-1 sees activity, Rung-2 sees
// no matching convergence" — it only condemns paired with an un-gameable rung.
type Rung1Class string

const (
	Rung1Working  Rung1Class = "working"  // vector + cost advancing
	Rung1Frozen   Rung1Class = "frozen"   // both frozen — likely hung/blocked OR thinking
	Rung1Spinning Rung1Class = "spinning" // vector advancing, no external corroboration
	Rung1Unknown  Rung1Class = ""
)

// Rung2Verdict is the externally-anchored oracle's canonical verdict (§10.2,
// §10.7). The FIRST un-gameable rung, with partial kill standing. It ABSTAINS when
// blind (spec-flow with no SHA, a build job before its first ref push, a degraded
// sweep) — an abstaining Rung-2 contributes NO vote.
type Rung2Verdict string

const (
	Rung2Converging Rung2Verdict = "converging" // net non-reverting diff is advancing
	Rung2Stalled    Rung2Verdict = "stalled"    // no net meaningful diff in the window
	Rung2Abstain    Rung2Verdict = "abstain"    // nothing to observe / blind / spec-flow
)

// Rung3State is the Flowbee-clock-only deadline state (§10.2). Pure wall-clock
// arithmetic on Flowbee's clock only — the un-gameable backstop with FULL kill
// authority. A SoftCrossed crossing arms the warn->cancel ladder but does not kill
// outright (it still needs a second rung); only AbsoluteCap revokes unilaterally.
type Rung3State struct {
	SoftCrossed bool // per-phase soft deadline crossed (role/constraint-derived)
	AbsoluteCap bool // absolute lease cap hit — the un-gameable floor, full kill
	// HeartbeatStale is clock-truth that the worker has stopped checking in: it has not
	// heartbeated for longer than the reap window (several heartbeat intervals). A
	// CLEANLY-crashed worker (kill -9, OOM, power loss) reports no "unhealthy" hint — its
	// last health is "ok" and it simply goes silent — so the soft-deadline ladder, which
	// needs a corroborating rung, never fires and only the absolute cap (lease_ttl, ~20m)
	// reaps it. That makes crash recovery 20 min instead of a few. HeartbeatStale is
	// un-gameable (a worker cannot fake heartbeats it never sent) and presumes death, so
	// like the absolute cap it is a UNILATERAL kill — revoke + redispatch at once.
	HeartbeatStale bool
}

// RungSet is the folded snapshot of every rung's current observation for a job,
// resolved by the runtime from persisted facts (the heartbeat's last Rung-0/1
// fields, the last Rung-2 sweep verdict, and the Rung-3 clock comparison done by
// the runtime against Flowbee's clock). It is the SOLE input to EvaluateKill.
type RungSet struct {
	Health AgentHealth  // Rung-0 (hint)
	Rung1  Rung1Class   // Rung-1 (hint)
	Rung2  Rung2Verdict // Rung-2 (un-gameable, abstains when blind)
	Rung3  Rung3State   // Rung-3 (un-gameable clock)

	// CircuitBreakerTripped reports the fleet-wide Rung-2 circuit breaker (§10.2):
	// if Rung-2 abstains for too many jobs at once (a wholesale reconcile outage),
	// Flowbee STOPS trusting clock-plus-Rung2 combinations rather than letting Rung-3
	// kill into a blind spot. When tripped, only the absolute cap may kill.
	CircuitBreakerTripped bool
}

// KillDecision is the pure verdict of the ladder: whether to kill (revoke), and if
// so, whether the absolute cap drove it (a unilateral kill that needs no second
// rung and bypasses the WARN->CANCEL grace) or a two-rung agreement did.
type KillDecision struct {
	Kill bool
	// Unilateral is true only for the absolute-cap kill (§10.3): no interpretation
	// of "progressing" survives it, and it bypasses the cancel grace -> straight
	// REVOKE. A two-rung kill is NOT unilateral (it goes WARN->CANCEL->REVOKE).
	Unilateral bool
	Reason     string
}

// worker-reported reports whether a rung signal is worker-reported (gameable) and
// thus cannot, on its own or paired only with another worker-reported rung, justify
// a kill (§10.3: "never two worker-reported rungs"). Rung-0 and Rung-1 are
// worker-reported; Rung-2 and Rung-3 are un-gameable.

// rung0Suspect reports whether Rung-0's health enum raises suspicion (a non-ok,
// non-zombie hint). zombie is handled by the fast-path, not the ladder.
func rung0Suspect(h AgentHealth) bool {
	switch h {
	case HealthStdinBlock, HealthCPUSpin, HealthOOM, HealthHungChild:
		return true
	default:
		return false
	}
}

// rung1Suspect reports whether Rung-1's class raises a kill-corroborating suspicion,
// given the Rung-0 health. `spinning` is always suspect (vector advancing with no
// external convergence — the canonical busywork signature). `frozen` is suspect
// ONLY when health is non-ok (a hung/blocked agent): per Guardrail B (§10.4),
// `agent_health: ok` + frozen reads as "thinking" (a legitimate long reasoning step
// or a CI-wait), NOT a stall — frozen-while-ok must NOT corroborate a kill. This is
// the structural realization of "frozen != dead" (§10.4).
func rung1Suspect(c Rung1Class, h AgentHealth) bool {
	switch c {
	case Rung1Spinning:
		return true
	case Rung1Frozen:
		return h != HealthOK && h != HealthUnknown
	default:
		return false
	}
}

// EvaluateKill is THE deterministic two-rung kill rule (I-13, §10.3). Pure: same
// RungSet -> same KillDecision, always. It enforces the §10.3 truth table:
//
//   - Absolute cap (Rung-3) -> KILL, unilateral. The hard ceiling; no second rung
//     needed, never gated by the circuit breaker (it is clock-truth, never blind).
//   - Soft deadline (Rung-3) + a corroborating second rung (Rung-0/1 suspicion OR
//     Rung-2 stalled) -> KILL (two-rung). The clock is the un-gameable corroborator.
//   - Rung-2 stalled + Rung-1 suspicion (spinning/frozen) -> KILL (two-rung). The
//     external oracle is the un-gameable corroborator for the worker's busy-claim.
//   - Rung-2 abstain is NOT a vote: soft deadline + abstain -> NO kill. Wait for a
//     real second rung or the absolute cap.
//   - Two worker-reported rungs (Rung-0 + Rung-1) -> NO kill. Suspicion only.
//
// The fleet-wide circuit breaker, when tripped, suppresses every clock-plus-Rung2
// combination (widening deadlines instead) — only the absolute cap survives it.
func EvaluateKill(rs RungSet) KillDecision {
	// 1. The unilateral, un-gameable, breaker-proof kills (§10.3) — pure clock-truth,
	//    never "blind": the absolute lease cap, and a worker that has gone SILENT past the
	//    reap window (a clean crash — it reports no unhealthy hint, so the soft-deadline
	//    ladder never corroborates; without this it waits the full lease_ttl to recover).
	if rs.Rung3.AbsoluteCap {
		return KillDecision{Kill: true, Unilateral: true, Reason: "absolute lease cap (Rung-3)"}
	}
	if rs.Rung3.HeartbeatStale {
		return KillDecision{Kill: true, Unilateral: true, Reason: "heartbeat stale (worker presumed dead)"}
	}

	// The circuit breaker (§10.2): on a wholesale reconcile outage Rung-2 abstains
	// everywhere; rather than let the clock kill into a blind spot, suppress every
	// non-absolute kill (the soft deadline + Rung-2 combos). Only the absolute cap
	// (handled above) survives.
	if rs.CircuitBreakerTripped {
		return KillDecision{Kill: false, Reason: "Rung-2 circuit breaker tripped: deadlines widened"}
	}

	// Is Rung-2 actually voting? An abstain is NOT a vote (§10.3).
	rung2Stalled := rs.Rung2 == Rung2Stalled

	// 2. Soft deadline (Rung-3, un-gameable) + a corroborating second rung. The clock
	//    is the independent corroborator; the second rung may be a worker-reported
	//    suspicion (Rung-0 or Rung-1) OR Rung-2 stalled — at least one un-gameable
	//    rung is present (Rung-3 itself), so the I-13 floor is satisfied.
	if rs.Rung3.SoftCrossed {
		switch {
		case rung2Stalled:
			return KillDecision{Kill: true, Reason: "soft deadline (Rung-3) + Rung-2 stalled"}
		case rung1Suspect(rs.Rung1, rs.Health):
			return KillDecision{Kill: true, Reason: "soft deadline (Rung-3) + Rung-1 suspicion"}
		case rung0Suspect(rs.Health):
			return KillDecision{Kill: true, Reason: "soft deadline (Rung-3) + Rung-0 hint"}
		default:
			// soft deadline alone, or soft deadline + Rung-2 abstain: NOT a kill.
			return KillDecision{Kill: false, Reason: "soft deadline crossed but no second rung (Rung-2 abstains)"}
		}
	}

	// 3. Rung-2 stalled (un-gameable) + a worker-reported suspicion. The external
	//    oracle confirms "no convergence" while the worker claims "busy" — the
	//    canonical spinning signature, externally confirmed. Rung-2 is the
	//    un-gameable rung, so I-13's floor is met.
	if rung2Stalled && (rung1Suspect(rs.Rung1, rs.Health) || rung0Suspect(rs.Health)) {
		return KillDecision{Kill: true, Reason: "Rung-2 stalled + worker-reported suspicion"}
	}

	// Otherwise: at most worker-reported suspicion, or a lone un-gameable rung with
	// no corroborator, or Rung-2 abstaining. No kill (§10.3 / §10.4 false-positive
	// bias: let a stalled job run a little long rather than kill healthy work).
	return KillDecision{Kill: false, Reason: "no two-rung agreement"}
}

// FastPath is the §10.6 two free fast-paths, evaluated on the heartbeat path BEFORE
// the ladder. They bypass the two-rung rule because the evidence is conclusive on
// its face. PURE.
type FastPath string

const (
	FastPathNone FastPath = ""
	// FastPathCancel: the agent is awaiting human/interactive input that will never
	// come -> directive: cancel, clean release, route per policy. No deadline wait.
	FastPathCancel FastPath = "awaiting_input"
	// FastPathFailed: the Rung-0 locally-provable exit (the supervisor waitpid'd and
	// saw the agent PID died). Straight to failed; the worker proved it on its own
	// machine. Not a "kill" — the agent is already dead.
	FastPathFailed FastPath = "agent_exited_zombie"
)

// EvaluateFastPath returns the §10.6 fast-path implied by the worker's heartbeat
// observations, if any. agent_exited_zombie takes precedence (a dead agent cannot
// also be awaiting input). PURE.
func EvaluateFastPath(health AgentHealth, awaitingInput bool, exited bool) FastPath {
	if exited || health == HealthZombie {
		return FastPathFailed
	}
	if awaitingInput {
		return FastPathCancel
	}
	return FastPathNone
}
