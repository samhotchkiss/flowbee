package scheduler

import (
	"testing"
	"time"
)

// TestWriteSetOverlap covers the path-intersection rules: equal paths, nested
// directories, and disjoint trees.
func TestWriteSetOverlap(t *testing.T) {
	cases := []struct {
		name string
		a, b WriteSet
		want bool
	}{
		{"equal path", WriteSet{Paths: []string{"internal/a.go"}}, WriteSet{Paths: []string{"internal/a.go"}}, true},
		{"disjoint", WriteSet{Paths: []string{"internal/a.go"}}, WriteSet{Paths: []string{"internal/b.go"}}, false},
		{"nested dir", WriteSet{Paths: []string{"internal/store"}}, WriteSet{Paths: []string{"internal/store/flow.go"}}, true},
		{"sibling dirs", WriteSet{Paths: []string{"internal/store"}}, WriteSet{Paths: []string{"internal/api"}}, false},
		{"wide vs anything", WriteSet{Wide: true}, WriteSet{Paths: []string{"x"}}, true},
		{"empty is wide", WriteSet{}, WriteSet{Paths: []string{"x"}}, true},
		{"normalized leading slash", WriteSet{Paths: []string{"/internal/a.go"}}, WriteSet{Paths: []string{"internal/a.go"}}, true},
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("%s: Overlaps=%v want %v", c.name, got, c.want)
		}
		// overlap is symmetric.
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Errorf("%s (sym): Overlaps=%v want %v", c.name, got, c.want)
		}
	}
}

// TestReservationFilterSerializesOverlap: two ready candidates whose write-sets
// overlap an in-flight reservation are excluded; a disjoint one is kept.
func TestReservationFilterSerializesOverlap(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{
		cand("overlap", 5, now),
		cand("disjoint", 5, now),
	}
	ws := map[string]WriteSet{
		"overlap":  {Paths: []string{"internal/store/flow.go"}},
		"disjoint": {Paths: []string{"internal/api/server.go"}},
	}
	active := []Reservation{
		{JobID: "inflight", WriteSet: WriteSet{Paths: []string{"internal/store"}}},
	}
	got := ReservationFilter(cands, active, ws)
	if len(got) != 1 || got[0].JobID != "disjoint" {
		t.Fatalf("expected only disjoint to survive, got %v", ids(got))
	}
}

// TestReservationFilterWideSingleFlights: a wide in-flight reservation excludes
// EVERY other candidate (a refactor single-flights the tree).
func TestReservationFilterWideSingleFlights(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{cand("a", 5, now), cand("b", 5, now)}
	ws := map[string]WriteSet{
		"a": {Paths: []string{"internal/a.go"}},
		"b": {Paths: []string{"internal/b.go"}},
	}
	active := []Reservation{{JobID: "refactor", WriteSet: WriteSet{Wide: true}}}
	if got := ReservationFilter(cands, active, ws); len(got) != 0 {
		t.Fatalf("a wide reservation must single-flight, got %v", ids(got))
	}
}

// TestReservationFilterSelfNeverExcludes: a candidate that already holds the
// reservation is not excluded by its own reservation.
func TestReservationFilterSelfNeverExcludes(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{cand("self", 5, now)}
	ws := map[string]WriteSet{"self": {Paths: []string{"internal/store"}}}
	active := []Reservation{{JobID: "self", WriteSet: WriteSet{Paths: []string{"internal/store"}}}}
	if got := ReservationFilter(cands, active, ws); len(got) != 1 {
		t.Fatalf("a job must not be excluded by its own reservation, got %v", ids(got))
	}
}

// TestReservationFilterUndeclaredPassesSpecificReservation: an undeclared candidate
// is NOT withheld by a SPECIFIC-path reservation (overlap can't be proven; the
// downstream resolve_conflict path is the safety net). It would only be withheld by a
// wide reservation.
func TestReservationFilterUndeclaredPassesSpecificReservation(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{cand("undeclared", 5, now)}
	active := []Reservation{{JobID: "other", WriteSet: WriteSet{Paths: []string{"internal/a.go"}}}}
	if got := ReservationFilter(cands, active, nil); len(got) != 1 {
		t.Fatalf("an undeclared candidate must pass a specific-path reservation, got %v", ids(got))
	}
}

// TestReservationFilterUndeclaredBlockedByWide: an undeclared candidate IS withheld
// by a wide (tree-wide single-flight) reservation.
func TestReservationFilterUndeclaredBlockedByWide(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{cand("undeclared", 5, now)}
	active := []Reservation{{JobID: "refactor", WriteSet: WriteSet{Wide: true}}}
	if got := ReservationFilter(cands, active, nil); len(got) != 0 {
		t.Fatalf("an undeclared candidate must be single-flighted by a wide reservation, got %v", ids(got))
	}
}

// TestReservationFilterNoReservations: with nothing in flight, every candidate
// passes untouched.
func TestReservationFilterNoReservations(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []Candidate{cand("a", 5, now), cand("b", 5, now)}
	if got := ReservationFilter(cands, nil, nil); len(got) != 2 {
		t.Fatalf("no reservations -> all pass, got %v", ids(got))
	}
}
