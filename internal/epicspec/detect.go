package epicspec

import "strings"

// SlugFromBranch extracts the epic slug from a PR's head branch name, recognizing
// ONLY the exact epic-lane convention "epic/<slug>" that `flowbee epic start` mints
// (cmd/flowbee/epic.go's AddEpicRun: Branch: "epic/"+slug). This is the PURE half of
// the epic-lane Phase 3 Epic-PR detection helper (task brief point 1: "a PR whose
// head branch matches branch epic/<slug> where an epics row exists for that
// slug+repo") — the row-existence half lives in store.EpicForRepoBranch, which
// calls this first and then looks the slug up.
//
// ok=false for anything that is not exactly one non-empty, slash-free segment after
// the "epic/" prefix: a bare "epic" with no slash, an empty slug ("epic/"), a
// near-miss that merely CONTAINS or STARTS THE WORD "epic" without the delimiting
// slash ("epicfoo", "epic-foo", "myepic/foo"), and a slug that itself carries a
// further "/" (a hand-created namespaced branch, never what `flowbee epic start`
// produces) are all deliberately rejected. This function's job is to recognize the
// EXACT convention, not to fuzzy-detect the word "epic" in a branch name — a
// near-miss must never be silently treated as an epic PR (that would judge an
// ordinary PR against a stranger's contract, or worse, let an ordinary PR dodge
// review by masquerading as "not epic" — see the "near-miss branch names" test).
func SlugFromBranch(branch string) (slug string, ok bool) {
	const prefix = "epic/"
	if !strings.HasPrefix(branch, prefix) {
		return "", false
	}
	rest := branch[len(prefix):]
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}
