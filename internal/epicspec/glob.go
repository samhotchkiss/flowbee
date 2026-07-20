package epicspec

import (
	"regexp"
	"strings"
	"sync"
)

// MatchGlob reports whether path matches glob using doublestar-style semantics: `**`
// matches ACROSS path separators (zero or more characters, including "/"), a single
// `*` matches WITHIN one path segment (never "/"), and `?` matches exactly one
// non-separator character. This is the SCOPE-HONESTY per-file check's real glob
// matcher (task brief point 2: "use a real glob match per-file here, not the
// launch-time overlap heuristic") — the repo vendors no glob library (confirmed: no
// doublestar/zglob in go.mod), so this is a deliberately MINIMAL hand-rolled
// implementation covering exactly the forms author-epic/SKILL.md's scope: examples
// use ("internal/foo/**", "cmd/bar/**.go"), not a general-purpose glob engine.
//
// Both glob and path are matched as given (normalizePath-style cleanup is the
// caller's job, matching internal/content's own division of labor between path
// normalization and the classifiers that consume normalized paths).
func MatchGlob(glob, path string) bool {
	if glob == "" || path == "" {
		return false
	}
	return globRegexp(glob).MatchString(path)
}

// globCache memoizes the glob->regexp compilation: CheckScope calls MatchGlob once
// per (glob, touched-path) pair, and a review-time diff can touch dozens of files
// against a handful of scope globs, so recompiling the same pattern's regexp per
// file is wasted work. Package-level and concurrency-safe (sync.Map) since epicspec
// has no other shared mutable state and callers (project.go's merge gate, a
// potential future concurrent caller) must not be forced to serialize on it.
var globCache sync.Map // string -> *regexp.Regexp

func globRegexp(glob string) *regexp.Regexp {
	if v, ok := globCache.Load(glob); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(compileGlob(glob))
	globCache.Store(glob, re)
	return re
}

// compileGlob translates a doublestar-style glob into an ANCHORED regexp source.
// `**/` becomes `(?:.*/)?` — ZERO or more whole segments, per real doublestar
// semantics: a leading `**/*.go` must match a root-level `main.go`, and `a/**/b`
// must match `a/b`, not only `a/x/b` (a naive `.*` for the pair would force the
// following `/` to be consumed, silently flagging every root-level file as out of
// scope — review F4). A `**` NOT followed by `/` becomes `.*` (crosses "/",
// covering the `internal/foo/**` and `cmd/bar/**.go` author forms); a lone `*`
// becomes `[^/]*`; `?` becomes `[^/]`; every other rune is regexp-escaped literally.
func compileGlob(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(glob) {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			if i+2 < len(glob) && glob[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 3
			} else {
				b.WriteString(".*")
				i += 2
			}
		case c == '*':
			b.WriteString("[^/]*")
			i++
		case c == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return b.String()
}
