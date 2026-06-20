package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

func testCard() history.Card {
	at := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	return history.Card{
		JobID:    "job-1",
		Kind:     job.KindBuild,
		Flow:     "build",
		Role:     job.RoleEngWorker,
		Title:    "Add JSON card output",
		Status:   job.StateReviewPending,
		PRNumber: 17,
		BaseSHA:  "base123",
		HeadSHA:  "head456",
		Attempts: 2,
		Bounces:  1,
		Timeline: []history.TimelineEntry{
			{Seq: 1, Kind: ledger.KindJobCreated, At: at, Note: "Job created."},
			{Seq: 2, Kind: ledger.KindPROpened, At: at.Add(time.Minute), Note: "PR #17 opened."},
		},
	}
}

func TestPrintCardJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := printCard(&buf, testCard(), true); err != nil {
		t.Fatalf("print json card: %v", err)
	}

	var out struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Role     string `json:"role"`
		Kind     string `json:"kind"`
		Flow     string `json:"flow"`
		PRNumber int    `json:"pr_number"`
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		Attempts int    `json:"attempts"`
		Bounces  int    `json:"bounces"`
		Timeline []struct {
			Seq  int    `json:"seq"`
			Kind string `json:"kind"`
			At   string `json:"at"`
			Note string `json:"note"`
		} `json:"timeline"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid json %q: %v", buf.String(), err)
	}
	if out.ID != "job-1" || out.State != "review_pending" || out.Role != "eng_worker" ||
		out.Kind != "build" || out.Flow != "build" || out.PRNumber != 17 ||
		out.BaseSHA != "base123" || out.HeadSHA != "head456" ||
		out.Attempts != 2 || out.Bounces != 1 {
		t.Fatalf("unexpected json shape: %+v", out)
	}
	if len(out.Timeline) != 2 || out.Timeline[0].Seq != 1 ||
		out.Timeline[0].Kind != "job_created" || out.Timeline[0].Note != "Job created." ||
		out.Timeline[1].Kind != "pr_opened" {
		t.Fatalf("unexpected timeline: %+v", out.Timeline)
	}
}

func TestCardJSONFlagAfterJobID(t *testing.T) {
	jsonOut, args, err := cardJSONFlag([]string{"job-1", "--json"})
	if err != nil {
		t.Fatalf("parse json flag: %v", err)
	}
	if !jsonOut {
		t.Fatalf("expected json flag to be true")
	}
	if len(args) != 1 || args[0] != "job-1" {
		t.Fatalf("expected only job id to remain, got %#v", args)
	}
}

func TestPrintCardMarkdownUnchanged(t *testing.T) {
	card := testCard()
	var buf bytes.Buffer
	if err := printCard(&buf, card, false); err != nil {
		t.Fatalf("print markdown card: %v", err)
	}
	if got, want := buf.String(), history.Render(card); got != want {
		t.Fatalf("default card output changed:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
