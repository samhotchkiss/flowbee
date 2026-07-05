package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestListRowShowsDistinguishingFields(t *testing.T) {
	j := store.BoardJob{
		ID:          "job-1",
		Repo:        "acme/api",
		State:       "ready",
		Role:        "eng_worker",
		IssueNumber: 42,
		UpdatedAt:   boardNow.Add(-3 * time.Minute),
	}
	got := listRow(j, boardNow)
	want := []string{"job-1", "acme/api", "42", "ready", "eng_worker", "3m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listRow = %v, want %v", got, want)
	}
}

func TestPrintListMultipleJobs(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "newer", Repo: "acme/api", State: "ready", Role: "eng_worker", IssueNumber: 11, UpdatedAt: boardNow.Add(-time.Minute)},
		{ID: "older", Repo: "acme/web", State: "review_pending", Role: "code_reviewer", UpdatedAt: boardNow.Add(-time.Hour)},
	}

	var buf bytes.Buffer
	if err := printList(&buf, jobs, boardNow); err != nil {
		t.Fatalf("printList: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"ID", "REPO", "ISSUE", "STATE", "ROLE", "UPDATED",
		"newer", "acme/api", "11", "ready", "eng_worker", "1m",
		"older", "acme/web", "-", "review_pending", "code_reviewer", "1h00m",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestPrintListEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := printList(&buf, nil, boardNow); err != nil {
		t.Fatalf("printList: %v", err)
	}
	if got, want := buf.String(), "No jobs found.\n"; got != want {
		t.Fatalf("empty list output = %q, want %q", got, want)
	}
}

func TestListJobsReadsBoardSnapshot(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	issue := 77
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID:          "list-job",
		Kind:        job.KindBuild,
		Flow:        "build",
		Stage:       "build",
		Role:        job.RoleEngWorker,
		Repo:        "acme/api",
		IssueNumber: &issue,
		Now:         boardNow,
	}); err != nil {
		t.Fatal(err)
	}

	jobs, err := listJobs(ctx, st)
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1: %#v", len(jobs), jobs)
	}
	got := jobs[0]
	if got.ID != "list-job" || got.Repo != "acme/api" || got.IssueNumber != 77 || got.State != string(job.StateReady) {
		t.Fatalf("unexpected listed job: %+v", got)
	}
}

func TestListLoadErrorHidesRawSQLiteNoTable(t *testing.T) {
	secretDBURL := "postgres://flowbee:secret@example.test/flowbee"
	err := listLoadError(errors.New("no such table: jobs"))
	msg := err.Error()
	if !strings.Contains(msg, "no initialized flowbee database") || !strings.Contains(msg, "flowbee serve") {
		t.Fatalf("expected actionable initialized-DB message, got %q", msg)
	}
	if strings.Contains(msg, "no such table") {
		t.Fatalf("raw SQLite table error leaked to user: %q", msg)
	}
	if strings.Contains(msg, secretDBURL) || strings.Contains(msg, "secret@example.test") {
		t.Fatalf("database URL details leaked to user: %q", msg)
	}
}

func TestListLoadErrorHidesRawBackendError(t *testing.T) {
	err := listLoadError(errors.New("pq: password authentication failed for postgres://flowbee:secret@example.test/flowbee"))
	msg := err.Error()
	if !strings.Contains(msg, "could not load jobs") || !strings.Contains(msg, "FLOWBEE_CONFIG") {
		t.Fatalf("expected concise load guidance, got %q", msg)
	}
	for _, leaked := range []string{"pq:", "password", "secret@example.test"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("raw backend detail %q leaked to user: %q", leaked, msg)
		}
	}
}
