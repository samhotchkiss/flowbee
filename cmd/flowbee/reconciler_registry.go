package main

import "time"

// baseV2ReconcilerGrace is the closed registry for every unconditional v2
// reconciler tick in serve. Keep lifecycle stop/liveness here even before an
// epic exists: when dedicated workers are enabled, missing either name turns a
// clean startup into a fail-closed registry error instead of a visible hold.
func baseV2ReconcilerGrace() map[string]time.Duration {
	return map[string]time.Duration{
		"review_handoff":              3 * time.Minute,
		"review_verdict":              3 * time.Minute,
		"delivery_backstop":           3 * time.Minute,
		"alert_interactor_projection": time.Minute,
		"driver_executor":             30 * time.Second,
		"builder_lifecycle":           30 * time.Second,
		"epic_worker_stop":            30 * time.Second,
		"epic_worker_liveness":        30 * time.Second,
		"project_actor_lifecycle":     30 * time.Second,
		"builder_launch":              30 * time.Second,
		"epic_effects":                30 * time.Second,
		"project_breaker_probe":       time.Minute,
		"reconciler_watchdog":         time.Minute,
	}
}
