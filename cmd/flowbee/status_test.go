package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestPrintStatusMergeHandoff(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "1", Repo: "acme/api", State: "merge_handoff"},
		{ID: "2", Repo: "acme/api", State: "merge_handoff"},
		{ID: "3", Repo: "acme/api", State: "running"},
		{ID: "4", Repo: "octo/infra", State: "needs_human"},
	}
	health := store.FleetHealth{LiveWorkers: 2, StaleWorkers: 1}

	var buf bytes.Buffer
	printStatus(&buf, jobs, health, false)
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
	printStatus(&buf, nil, store.FleetHealth{}, false)
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
	printStatus(&buf, jobs, store.FleetHealth{LiveWorkers: 1}, false)
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
	printStatus(&buf, nil, store.FleetHealth{}, true)
	out := buf.String()

	if !strings.Contains(out, "PAUSED") {
		t.Errorf("expected PAUSED banner in paused output:\n%s", out)
	}
}
