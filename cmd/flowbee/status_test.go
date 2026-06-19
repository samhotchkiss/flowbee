package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestPrintStatusModelBreakdown: the fleet line shows the live-worker per-backend tally
// (sorted, stable) so an operator sees the fleet is on codex; no models => no suffix.
func TestPrintStatusModelBreakdown(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{LiveWorkers: 16, ByModel: map[string]int{"codex": 14, "sonnet": 2}}, nil, false)
	if got := buf.String(); !strings.Contains(got, "16 live") || !strings.Contains(got, "(codex:14, sonnet:2)") {
		t.Errorf("expected live count + sorted model breakdown, got:\n%s", got)
	}
	var buf2 bytes.Buffer
	printStatus(&buf2, nil, store.FleetHealth{LiveWorkers: 3}, nil, false)
	if got := buf2.String(); strings.Contains(got, "(") {
		t.Errorf("no models => no breakdown suffix, got:\n%s", got)
	}
}

// TestPrintStatusAbandoned: dropped GitHub writes surface in the human view (sorted, pointing
// at the recovery command); none => no line.
func TestPrintStatusAbandoned(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, map[string]int{"issues.create": 4, "mergeQueue.enqueue": 6}, false)
	out := buf.String()
	if !strings.Contains(out, "abandoned GitHub writes: issues.create:4, mergeQueue.enqueue:6") || !strings.Contains(out, "flowbee retry-outbox") {
		t.Errorf("expected the abandoned line + recovery hint, got:\n%s", out)
	}
	var buf2 bytes.Buffer
	printStatus(&buf2, nil, store.FleetHealth{}, nil, false)
	if strings.Contains(buf2.String(), "abandoned") {
		t.Errorf("no abandoned actions => no line, got:\n%s", buf2.String())
	}
}

func TestPrintStatusMergeHandoff(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "1", Repo: "acme/api", State: "merge_handoff"},
		{ID: "2", Repo: "acme/api", State: "merge_handoff"},
		{ID: "3", Repo: "acme/api", State: "running"},
		{ID: "4", Repo: "octo/infra", State: "needs_human"},
	}
	health := store.FleetHealth{LiveWorkers: 2, StaleWorkers: 1}

	var buf bytes.Buffer
	printStatus(&buf, jobs, health, nil, false)
	out := buf.String()

	for _, want := range []string{
		"2 merge_handoff",
		"1 needs_human",
		"2 live",
		"1 stale",
		"acme/api",
		"octo/infra",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestPrintStatusEmpty(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, nil, false)
	out := buf.String()

	if !strings.Contains(out, "no jobs") {
		t.Errorf("expected 'no jobs' in empty output:\n%s", out)
	}
	if !strings.Contains(out, "0 merge_handoff") {
		t.Errorf("expected '0 merge_handoff' in empty output:\n%s", out)
	}
}

func TestPrintStatusRepoStateCounts(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "1", Repo: "corp/svc", State: "running"},
		{ID: "2", Repo: "corp/svc", State: "running"},
		{ID: "3", Repo: "corp/svc", State: "ready"},
	}
	var buf bytes.Buffer
	printStatus(&buf, jobs, store.FleetHealth{LiveWorkers: 1}, nil, false)
	out := buf.String()

	if !strings.Contains(out, "running:2") {
		t.Errorf("expected 'running:2' in output:\n%s", out)
	}
	if !strings.Contains(out, "ready:1") {
		t.Errorf("expected 'ready:1' in output:\n%s", out)
	}
}

func TestPrintStatusPausedBanner(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, nil, true)
	out := buf.String()

	if !strings.Contains(out, "PAUSED") {
		t.Errorf("expected PAUSED banner in paused output:\n%s", out)
	}
}
