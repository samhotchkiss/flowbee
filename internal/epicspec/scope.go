package epicspec

import "strings"

// ScopeOverlap reports whether two epics' declared `scope:` glob lists conflict
// under the CONSERVATIVE rule the design doc calls for: "glob-vs-glob overlap:
// conservative — treat any shared path prefix up to a wildcard as overlap."
//
// Concretely: for each glob, take the literal text before its first wildcard
// character (*, ?, [) — "internal/foo/**" -> "internal/foo/", "cmd/bar/**.go" ->
// "cmd/bar/". Two globs overlap if one prefix is a plain STRING prefix of the
// other (in either direction), INCLUDING equality. This is deliberately NOT
// directory-boundary aware (unlike scheduler.WriteSet.Overlaps' pathsOverlap,
// which requires a "/" boundary): "internal/foo*" and "internal/foobar/**" DO
// overlap under this rule ("internal/foo" is a string-prefix of
// "internal/foobar/") even though they name disjoint directories — see
// globPrefix's doc for exactly what counts as the "prefix up to a wildcard". The
// bias throughout is toward FALSE POSITIVES (refuse to launch): a multi-day epic
// that silently collides with another for days is far more expensive than an
// operator narrowing an over-eager scope: list and retrying.
//
// A glob with NO literal prefix at all (bare "*" or "**") is conservatively
// treated as overlapping EVERY other glob — it reserves the whole tree, mirroring
// scheduler.WriteSet's "no declared path == wide == overlaps everything" rule.
//
// Prefixes are compared CASE-FOLDED (review m8): the control plane and several
// launch hosts run macOS/case-insensitive filesystems, where "Backend/**" and
// "backend/**" name the SAME tree — two epics declaring those must collide, not
// pass as disjoint. On a case-SENSITIVE box this makes the rule slightly more
// conservative (a false-positive refusal for genuinely distinct Backend/ vs
// backend/ dirs), which is the bias this whole function already takes.
func ScopeOverlap(a, b []string) (overlaps bool, globA, globB string) {
	for _, ga := range a {
		pa := strings.ToLower(globPrefix(ga))
		for _, gb := range b {
			pb := strings.ToLower(globPrefix(gb))
			if pa == "" || pb == "" || strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa) {
				return true, ga, gb
			}
		}
	}
	return false, "", ""
}

// globPrefix returns the literal (non-wildcard) text before a glob's first
// wildcard character. "internal/foo/**" -> "internal/foo/"; "cmd/bar/**.go" ->
// "cmd/bar/"; a glob with no wildcard at all returns itself unchanged (an exact
// path is its own one-element "glob").
func globPrefix(glob string) string {
	if idx := strings.IndexAny(glob, "*?["); idx >= 0 {
		return glob[:idx]
	}
	return glob
}
