package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// fixed reference instant for deterministic age rendering.
var boardNow = time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

func TestBoardRowSeededJob(t *testing.T) {
	j := store.BoardJob{
		ID:          "job-1",
		Repo:        "acme/api",
		State:       "running",
		Role:        "spec_author",
		IssueNumber: 1421,
		Bounces:     0,
		UpdatedAt:   boardNow.Add(-2 * time.Minute),
	}
	got := boardRow(j, boardNow)
	want := []string{"acme/api", "1421", "running", "spec_author", "0", "2m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boardRow = %v, want %v", got, want)
	}
}

func TestBoardRowIssueless(t *testing.T) {
	j := store.BoardJob{
		ID:          "job-2",
		Repo:        "octo/infra",
		State:       "idle",
		Role:        "", // role-less → "-"
		IssueNumber: 0,  // issue-less → "-"
		Bounces:     0,
		UpdatedAt:   boardNow.Add(-12 * 24 * time.Hour),
	}
	got := boardRow(j, boardNow)
	want := []string{"octo/infra", "-", "idle", "-", "0", "12d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boardRow = %v, want %v", got, want)
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{47 * time.Second, "47s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{2 * time.Minute, "2m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h00m"},
		{time.Hour + 4*time.Minute, "1h04m"},
		{23*time.Hour + 59*time.Minute, "23h59m"},
		{24 * time.Hour, "1d"},
		{12 * 24 * time.Hour, "12d"},
	}
	for _, c := range cases {
		if got := formatAge(c.d); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSortBoardJobs(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "d", Repo: "acme/web", IssueNumber: 88},
		{ID: "c", Repo: "acme/api", IssueNumber: 0}, // issue-less → bottom of acme/api
		{ID: "b", Repo: "acme/api", IssueNumber: 1422},
		{ID: "a", Repo: "acme/api", IssueNumber: 1421},
		{ID: "e", Repo: "octo/infra", IssueNumber: 0},
	}
	sortBoardJobs(jobs)
	var order []string
	for _, j := range jobs {
		order = append(order, j.ID)
	}
	want := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("sort order = %v, want %v", order, want)
	}
}
