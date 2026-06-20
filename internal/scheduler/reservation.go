package scheduler

import "strings"

// F8 — blast-radius reservations (the "avoid first" arm of merge-conflict handling,
// DESIGN §E / build-list E). The cheapest conflict is the one never created: before
// the scheduler co-dispatches two builds, it checks their declared WRITE-SETS (the
// blast-radius paths). Two builds whose write-sets OVERLAP must NOT run at the same
// time — the second would rebase onto the first and (likely) conflict. A WIDE-blast
// build (a refactor that declares a broad prefix, or `scope: wide`) single-flights:
// while it holds a reservation NOTHING else dispatches against the overlapping tree.
//
// This is a PURE, deterministic-core concern (no clock, no rand, no I/O): the
// reservation set is folded from persisted facts (the active in-flight builds' write
// sets) and passed IN; the filter is a pure function over (candidate write-set,
// active reservations). The store wires the live reservation set into the candidate
// query; the atomic claim remains the correctness backstop.

// WriteSet is the declared blast-radius of a build expressed as a reservation: the
// set of touched path prefixes, plus a Wide flag for a coarse refactor that declared
// no concrete paths (or declared `scope: wide`). A Wide write-set overlaps EVERY
// other write-set (it single-flights the whole tree). An EMPTY, non-wide write-set
// (a build that has declared nothing yet) is treated as Wide too — absence of a
// declaration is the SAFE (conservative) default: it cannot be proven disjoint, so
// it single-flights rather than risk a silent co-dispatch into a conflict.
type WriteSet struct {
	// Paths are the declared touched path prefixes (normalized: no leading "./" or
	// "/"). A path equal to, or beneath, a reserved prefix overlaps it.
	Paths []string
	// Wide marks a coarse blast-radius (a refactor) that overlaps everything. A wide
	// write-set single-flights: it reserves the whole tree while it is in flight.
	Wide bool
}

// IsWide reports whether the write-set reserves the whole tree. A write-set with the
// Wide flag, OR one that declared no concrete paths, is wide (the conservative
// default — an undeclared write-set cannot be proven disjoint from anything).
func (w WriteSet) IsWide() bool {
	return w.Wide || len(normalizedNonEmpty(w.Paths)) == 0
}

// Reservation is one in-flight build's held write-set (folded from the active
// leases). JobID identifies the holder so a candidate never reserves against itself.
type Reservation struct {
	JobID    string
	WriteSet WriteSet
}

// Overlaps reports whether two write-sets conflict (their declared trees intersect).
// A wide write-set on either side overlaps everything. Otherwise two path-sets
// overlap iff any path of one is equal to, or nested under, any path of the other
// (a parent-directory reservation covers its children, and vice-versa). PURE.
func (w WriteSet) Overlaps(other WriteSet) bool {
	if w.IsWide() || other.IsWide() {
		return true
	}
	for _, a := range normalizedNonEmpty(w.Paths) {
		for _, b := range normalizedNonEmpty(other.Paths) {
			if pathsOverlap(a, b) {
				return true
			}
		}
	}
	return false
}

// ReservationFilter removes from cands every candidate whose write-set OVERLAPS an
// active reservation held by a DIFFERENT job. The candidate's own reservation (if it
// already holds one) never excludes itself. This is the §E "won't co-dispatch two
// builds whose write-sets overlap" rule, applied as a pure pre-filter before the
// priority/aging ranking.
//
// KNOWN EDGE (no reservation-side anti-starvation): a filtered candidate is not ranked
// that round, so aging cannot rescue it while it stays filtered. An explicitly WIDE
// candidate (scope:wide refactor) overlaps every reservation, so a SUSTAINED stream of
// overlapping in-flight builds could withhold it indefinitely. In practice in-flight
// builds COMPLETE and their reservations clear (opening a window), and reservations
// exist only for builds that DECLARED a write-set (uncommon), so indefinite starvation
// needs a pathological continuous-overlap workload; the conflict_resolver is the
// correctness backstop if an overlapping pair ever does co-dispatch. A horizon-based
// admission (admit a candidate aged past a threshold despite reservations) would close
// it fully but needs a clock here and trades conflict-avoidance for liveness.
//
// The write-set for each candidate is supplied by `writeSetOf` (folded from the
// job's declared blast-radius). A candidate with NO declared write-set (an empty,
// non-wide entry, or no entry) is NOT excluded by a SPECIFIC-path reservation — its
// write-set is unknown, so overlap cannot be proven, and the downstream conflict
// handling (resolve_conflict + integrated-head re-review) is the safety net. It IS
// excluded by a WIDE reservation: a refactor that single-flights the tree blocks
// everything, declared or not (it genuinely conflicts with any concurrent edit).
func ReservationFilter(cands []Candidate, active []Reservation, writeSetOf map[string]WriteSet) []Candidate {
	if len(active) == 0 {
		return cands
	}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		ws, declared := candidateWriteSet(c.JobID, writeSetOf)
		if reservationConflicts(c.JobID, ws, declared, active) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// candidateWriteSet returns a candidate's write-set and whether it was meaningfully
// declared (concrete paths or an explicit wide scope). An undeclared candidate gets
// an empty write-set and declared=false.
func candidateWriteSet(jobID string, writeSetOf map[string]WriteSet) (WriteSet, bool) {
	ws, ok := writeSetOf[jobID]
	if !ok {
		return WriteSet{}, false
	}
	declared := ws.Wide || len(normalizedNonEmpty(ws.Paths)) > 0
	return ws, declared
}

// reservationConflicts reports whether a candidate must be withheld. A DECLARED
// candidate is withheld iff its write-set overlaps any active reservation held by a
// different job. An UNDECLARED candidate is withheld only by a WIDE active
// reservation (a tree-wide single-flight). PURE.
func reservationConflicts(jobID string, ws WriteSet, declared bool, active []Reservation) bool {
	_, blocked := blockingReservation(jobID, ws, declared, active)
	return blocked
}

// blockingReservation returns the id of the FIRST active reservation that withholds the
// candidate (ok=true), or "" / false when it isn't blocked.
func blockingReservation(jobID string, ws WriteSet, declared bool, active []Reservation) (string, bool) {
	for _, r := range active {
		if r.JobID == jobID {
			continue // a job never reserves against itself
		}
		if declared {
			if ws.Overlaps(r.WriteSet) {
				return r.JobID, true
			}
			continue
		}
		// undeclared candidate: only a wide reservation single-flights it out.
		if r.WriteSet.IsWide() {
			return r.JobID, true
		}
	}
	return "", false
}

// BlockedBy explains ReservationFilter for a single candidate: the id of the active
// reservation withholding jobID (ok=true), using the candidate's write-set from writeSetOf,
// or "" / false when it's leasable. The explainability behind a `flowbee reservations` view
// + the starvation log, so a withheld ready job is never a silent mystery (russ #213).
func BlockedBy(jobID string, active []Reservation, writeSetOf map[string]WriteSet) (string, bool) {
	ws, declared := candidateWriteSet(jobID, writeSetOf)
	return blockingReservation(jobID, ws, declared, active)
}

// pathsOverlap reports whether two normalized paths intersect: equal, or one is a
// directory prefix of the other. PURE.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return isPrefixDir(a, b) || isPrefixDir(b, a)
}

// isPrefixDir reports whether `parent` is a directory prefix of `child` (child sits
// beneath parent/). PURE.
func isPrefixDir(parent, child string) bool {
	p := parent
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return strings.HasPrefix(child, p)
}

// normalizedNonEmpty returns the non-empty, normalized paths of in.
func normalizedNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		np := normalizeResvPath(p)
		if np != "" {
			out = append(out, np)
		}
	}
	return out
}

// normalizeResvPath strips a leading "./" or "/" and surrounding whitespace (the
// same normalization the content gate uses, kept local so the deterministic core
// stays self-contained).
func normalizeResvPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}
