// Package content is the content-integrity gate (DESIGN §9.2, I-11): it treats a
// worker's returned diff as UNTRUSTED DATA and clears it through three
// deterministic, non-LLM gates before that diff is auto-merge-eligible (the §5.4
// predicate, conditions 2–4):
//
//	(a) path denylist        — any hit forces a human gate (.github/workflows/**,
//	    lockfiles + lifecycle scripts, Dockerfiles, secrets, Flowbee's own source,
//	    and the denylist itself). The agent editing the rules it is judged by is the
//	    canonical privilege-escalation attempt.
//	(b) declared blast-radius — the eng_worker DECLARES the paths/scope it touches;
//	    Flowbee compares the DECLARATION against the ACTUAL touched paths. A diff
//	    that touches more than it declared is a tamper signal (denylist-class).
//	(c) deterministic static checks — applies-clean@base, parse markers, secret-scan
//	    (entropy + known-pattern), binary-blob allowlist, size/oversize bounds.
//
// Only a diff that is denylist-clear ∧ blast-radius-consistent ∧ static-checks-green
// is *eligible* for self_merge (and only then if §14 removed the human gate);
// everything else takes handoff regardless of the reviewer's requested disposition.
//
// This package is PURE and stdlib-only: it reads no clock, mints no IDs, and makes
// no network/GitHub call. The unified diff is parsed structurally; the static
// "applies-clean" fact is supplied as a value by the runtime (which holds the git
// fixture) — the gate is a deterministic function of its inputs so it can sit
// inside the deterministic core's EngineState as a precomputed Result. archcheck
// permits it in the core (no clock/rand/ULID/GitHub import).
package content

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Patch is the eng_worker's work-product as untrusted data (§7.3): a unified diff
// bound to base_sha plus the DECLARED blast-radius. There is NO pr field (Domain B
// owns PR existence). AppliesClean is the runtime-supplied static fact "the diff
// applied cleanly at base_sha" — the one check that needs the git fixture; the
// runtime resolves it and passes it IN so the gate stays a pure function.
type Patch struct {
	Diff         string // the unified diff text (untrusted)
	BaseSHA      string
	Declared     BlastRadius // the worker's DECLARED scope (a commitment, verified)
	AppliesClean bool        // runtime-resolved: the diff applied at base_sha (default true if unknown)
	// AppliesCleanKnown distinguishes "the runtime checked and it applied" from
	// "the runtime did not run the apply check" (in which case the check is not
	// failed on this account — absence of proof is not proof of failure). A patch
	// the runtime PROVED does not apply sets AppliesCleanKnown=true, AppliesClean=false.
	AppliesCleanKnown bool
}

// BlastRadius is the declared (or actual) scope of a diff: the set of touched
// paths and a coarse scope label. The DECLARATION is compared against the ACTUAL.
type BlastRadius struct {
	Paths []string `json:"paths,omitempty"`
	Scope string   `json:"scope,omitempty"`
}

// Result is the deterministic outcome of the content-integrity gate. It is DATA:
// the runtime computes it from the stored patch and threads it into EngineState so
// the pure gate (internal/job) can promote/deny self_merge over it. Eligible()
// folds the three conditions into the §5.4 conditions-2–4 predicate.
type Result struct {
	// DenylistClear is §5.4 condition 2: the diff touches no denylisted path.
	DenylistClear bool `json:"denylist_clear"`
	// BlastRadiusConsistent is §5.4 condition 3: the declared blast-radius matches
	// the actual touched paths (the diff touched nothing it did not declare).
	BlastRadiusConsistent bool `json:"blast_radius_consistent"`
	// StaticChecksPass is §5.4 condition 4: applies-clean ∧ parses ∧ secret-scan
	// clean ∧ binary allowlist ∧ size bounds — all deterministic, non-LLM.
	StaticChecksPass bool `json:"static_checks_pass"`

	// DenylistHits records WHICH denylisted paths were touched (audit + the human
	// reviewer's attention queue). Empty iff DenylistClear.
	DenylistHits []string `json:"denylist_hits,omitempty"`
	// UndeclaredPaths records actual touched paths absent from the declaration (the
	// tamper signal). Empty iff BlastRadiusConsistent.
	UndeclaredPaths []string `json:"undeclared_paths,omitempty"`
	// StaticFailures records WHY static checks failed (apply / parse / secret /
	// binary / size). Empty iff StaticChecksPass.
	StaticFailures []string `json:"static_failures,omitempty"`
}

// Eligible reports whether the diff cleared all three content-integrity conditions
// (§5.4 conditions 2–4). A diff that is not eligible takes handoff regardless of
// the reviewer's requested disposition — the structural meaning of "the returned
// diff is untrusted data."
func (r Result) Eligible() bool {
	return r.DenylistClear && r.BlastRadiusConsistent && r.StaticChecksPass
}

// Tampered reports whether the gate saw an active tamper signal: a denylisted path
// or an undeclared touched path. (A static-check failure, e.g. a non-applying
// patch, is a *quality* failure that denies self_merge but is not itself "tamper".)
func (r Result) Tampered() bool {
	return !r.DenylistClear || !r.BlastRadiusConsistent
}

// Limits bounds the deterministic static checks. Zero values fall back to the
// shipped defaults (DefaultLimits) so a caller may pass a zero Limits.
type Limits struct {
	// MaxDiffBytes is the oversize bound: a diff larger than this fails static
	// checks (an un-reviewable mega-diff is a tamper/quality signal). 0 => default.
	MaxDiffBytes int
	// MaxChangedFiles bounds the number of distinct files a single diff may touch.
	// 0 => default.
	MaxChangedFiles int
}

// DefaultLimits are the shipped size bounds.
var DefaultLimits = Limits{MaxDiffBytes: 1 << 20, MaxChangedFiles: 200}

func (l Limits) resolve() Limits {
	if l.MaxDiffBytes == 0 {
		l.MaxDiffBytes = DefaultLimits.MaxDiffBytes
	}
	if l.MaxChangedFiles == 0 {
		l.MaxChangedFiles = DefaultLimits.MaxChangedFiles
	}
	return l
}

// Policy is the OPERATOR-CONFIGURABLE content-integrity posture (F2): the size
// ceilings PLUS an installation-supplied EXTRA denylist of path prefixes that
// augment the shipped, always-on protected set (denyMatchers). The shipped set is
// non-negotiable (an agent must never weaken CI / lockfiles / secrets / Flowbee's
// own source — see denyMatchers); ExtraDenyPrefixes only ever ADDS to it. A path
// equal to, or beneath, any configured prefix is denylisted (forces the human gate,
// §9.2a). The zero Policy is exactly the shipped defaults (no extra denylist), so a
// caller may always pass a zero value and `Check` stays its backward-compatible self.
type Policy struct {
	Limits Limits
	// ExtraDenyPrefixes are additional protected path prefixes (normalized; a leading
	// "./" or "/" is stripped). They EXTEND — never replace — the shipped denylist.
	ExtraDenyPrefixes []string
}

// Check runs the full content-integrity gate over a patch and returns the
// deterministic Result. PURE: same (patch, limits) -> same Result, always. The
// runtime calls it once and threads the Result into EngineState. It is the
// zero-extra-denylist case of CheckWithPolicy (the shipped defaults).
func Check(p Patch, lim Limits) Result {
	return CheckWithPolicy(p, Policy{Limits: lim})
}

// CheckWithPolicy is Check under an operator-configured Policy (F2): the same
// deterministic gate, but with configurable size ceilings AND an installation
// EXTRA denylist that augments the shipped protected set. PURE: same (patch,
// policy) -> same Result, always.
func CheckWithPolicy(p Patch, pol Policy) Result {
	lim := pol.Limits.resolve()
	touched := TouchedPaths(p.Diff)

	r := Result{}

	// (a) path denylist — §9.2(a). Any hit (shipped set OR the operator's extra
	// prefixes) forces the human gate.
	hits := denylistHitsWith(touched, pol.ExtraDenyPrefixes)
	r.DenylistHits = hits
	r.DenylistClear = len(hits) == 0

	// (b) declared blast-radius vs actual — §9.2(b). A touched path the worker did
	// not declare is the tamper signal (the diff touched MORE than it declared).
	undeclared := undeclaredPaths(touched, p.Declared)
	r.UndeclaredPaths = undeclared
	r.BlastRadiusConsistent = len(undeclared) == 0

	// (c) deterministic static checks — §9.2(c).
	r.StaticFailures = staticChecks(p, touched, lim)
	r.StaticChecksPass = len(r.StaticFailures) == 0

	return r
}

// ── (a) path denylist ──────────────────────────────────────────────────────────

// denyMatchers is the §9.2(a) protected set. Each matcher tests one normalized
// path. The denylist is itself in the protected set (the canonical
// privilege-escalation attempt: the agent editing the rules it is judged by).
var denyMatchers = []struct {
	name  string
	match func(path string) bool
}{
	// .github/workflows/** and CI config — a diff that weakens its own gate.
	{"ci_workflow", func(p string) bool {
		return strings.HasPrefix(p, ".github/workflows/") ||
			strings.HasPrefix(p, ".github/actions/") ||
			p == ".github/workflows" ||
			hasAnyBase(p, ".gitlab-ci.yml", "azure-pipelines.yml", ".circleci/config.yml") ||
			strings.HasPrefix(p, ".circleci/")
	}},
	// lockfiles + lifecycle scripts — arbitrary code execution at install time.
	{"lockfile_or_lifecycle", func(p string) bool {
		base := baseName(p)
		switch base {
		case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "npm-shrinkwrap.json",
			"Gemfile.lock", "poetry.lock", "Pipfile.lock", "go.sum", "Cargo.lock",
			"composer.lock":
			return true
		}
		if strings.HasSuffix(base, ".lock") {
			return true
		}
		// lifecycle hook scripts (preinstall/postinstall/...) anywhere in the tree.
		for _, hook := range []string{"preinstall", "postinstall", "preuninstall", "postuninstall", "prepare", "prepublish"} {
			if base == hook || base == hook+".js" || base == hook+".sh" {
				return true
			}
		}
		return false
	}},
	// Dockerfiles / base-image refs / devcontainer — supply-chain substitution.
	{"dockerfile_or_devcontainer", func(p string) bool {
		base := baseName(p)
		if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") ||
			strings.HasSuffix(base, ".dockerfile") || base == ".dockerignore" {
			return true
		}
		return strings.HasPrefix(p, ".devcontainer/") || base == "devcontainer.json"
	}},
	// secrets / credential material / signing keys — exfiltration or substitution.
	{"secret_material", func(p string) bool {
		base := baseName(p)
		if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
			return true
		}
		switch {
		case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".key"),
			strings.HasSuffix(base, ".pfx"), strings.HasSuffix(base, ".p12"),
			strings.HasSuffix(base, ".keystore"), strings.HasSuffix(base, ".jks"):
			return true
		}
		switch base {
		case "id_rsa", "id_ed25519", "id_dsa", "id_ecdsa", ".npmrc", ".netrc", ".pypirc":
			return true
		}
		lower := strings.ToLower(p)
		return strings.Contains(lower, "/secrets/") || strings.HasPrefix(lower, "secrets/") ||
			strings.Contains(lower, "/credentials") || strings.HasPrefix(lower, "credentials")
	}},
	// Flowbee's own source & config (flows, lenses, the denylist itself) — the
	// agent editing the orchestrator it runs under is privilege escalation.
	{"flowbee_source", func(p string) bool {
		return strings.HasPrefix(p, "internal/") ||
			strings.HasPrefix(p, "cmd/flowbee/") ||
			strings.HasPrefix(p, "tools/archcheck/") ||
			strings.HasPrefix(p, "tools/providerlint/") ||
			strings.HasPrefix(p, "flows/") ||
			baseName(p) == "flowbee.yaml" ||
			// the denylist source itself is the canonical escalation target.
			p == "internal/content/content.go"
	}},
}

// DenylistHits returns the denylist class names hit by the touched paths, sorted
// and de-duplicated. Empty => denylist-clear. It uses only the shipped protected
// set (no operator-configured extra prefixes).
func DenylistHits(touched []string) []string {
	return denylistHitsWith(touched, nil)
}

// denylistHitsWith is DenylistHits plus an operator-supplied EXTRA set of denied
// path prefixes (F2). A touched path equal to, or beneath, an extra prefix is hit
// under the synthetic class "configured". The extra set only ADDS to the shipped
// denylist; it can never remove a shipped protection.
func denylistHitsWith(touched, extraPrefixes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range touched {
		np := normalizePath(p)
		if np == "" {
			continue
		}
		for _, m := range denyMatchers {
			if m.match(np) {
				if !seen[np+"|"+m.name] {
					seen[np+"|"+m.name] = true
					out = append(out, m.name+":"+np)
				}
			}
		}
		if matchesAnyPrefix(np, extraPrefixes) {
			if !seen[np+"|configured"] {
				seen[np+"|configured"] = true
				out = append(out, "configured:"+np)
			}
		}
	}
	sort.Strings(out)
	return out
}

// matchesAnyPrefix reports whether path equals, or sits beneath, any of the
// normalized prefixes. An empty prefix matches nothing (a guard against an
// accidental "deny everything").
func matchesAnyPrefix(path string, prefixes []string) bool {
	for _, raw := range prefixes {
		np := normalizePath(raw)
		if np == "" {
			continue
		}
		if path == np {
			return true
		}
		prefix := np
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// IsDenylisted reports whether a single path is in the protected set (a helper for
// callers that want a boolean rather than the class).
func IsDenylisted(path string) bool {
	np := normalizePath(path)
	if np == "" {
		return false
	}
	for _, m := range denyMatchers {
		if m.match(np) {
			return true
		}
	}
	return false
}

// ── (b) declared blast-radius vs actual ─────────────────────────────────────────

// undeclaredPaths returns touched paths NOT covered by the declaration. A
// declaration covers a path if the path equals a declared path or sits under a
// declared directory prefix. A "worktree"/"*" scope declaration with no paths is
// NOT a blanket cover — the declaration must enumerate (or prefix) what it touches,
// else a worker could declare "everything" and defeat the check. An empty
// declaration covers nothing, so any touched path is undeclared.
func undeclaredPaths(touched []string, decl BlastRadius) []string {
	var out []string
	for _, t := range touched {
		nt := normalizePath(t)
		if nt == "" {
			continue
		}
		if !declarationCovers(nt, decl) {
			out = append(out, nt)
		}
	}
	sort.Strings(out)
	return dedup(out)
}

func declarationCovers(path string, decl BlastRadius) bool {
	for _, d := range decl.Paths {
		nd := normalizePath(d)
		if nd == "" {
			continue
		}
		if path == nd {
			return true
		}
		// a declared directory prefix (with or without a trailing slash) covers
		// everything beneath it.
		prefix := nd
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// ── (c) deterministic static checks ─────────────────────────────────────────────

// secretPatterns are known-pattern secret signatures (provider-agnostic credential
// shapes). A match in an ADDED line fails the secret-scan.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                            // AWS access key id
	regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*[A-Za-z0-9/+]{20,}`),                       // AWS secret
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),                                          // PEM private key
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),                                                  // GitHub token
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                                // Slack token
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password)\s*[=:]\s*["']?[A-Za-z0-9/+_\-]{24,}`), // generic
}

// staticChecks runs the deterministic non-LLM checks (§9.2(c)) and returns the
// list of failure reasons (empty => all green).
func staticChecks(p Patch, touched []string, lim Limits) []string {
	var fail []string

	// applies-clean@base — the runtime resolves this against the git fixture. We
	// only fail when the runtime PROVED it does not apply; an unknown apply state
	// is not a failure on this account.
	if p.AppliesCleanKnown && !p.AppliesClean {
		fail = append(fail, "patch does not apply cleanly at base_sha")
	}

	// the diff must be a parseable unified diff that names at least one file. A diff
	// claiming changes but naming no file path is malformed (un-applyable narration).
	if strings.TrimSpace(p.Diff) != "" && len(touched) == 0 {
		fail = append(fail, "diff parses to no touched paths (malformed unified diff)")
	}

	// secret-scan — known-pattern + entropy over ADDED lines (untrusted data).
	for _, line := range addedLines(p.Diff) {
		if hit := scanSecret(line); hit != "" {
			fail = append(fail, "secret-scan: "+hit)
			break // one finding is enough to deny; keep the message bounded.
		}
	}

	// binary-blob allowlist — a unified diff that introduces a binary file is denied
	// (no binary outside an allowlist; the v1 allowlist is empty).
	if introducesBinary(p.Diff) {
		fail = append(fail, "binary blob outside the allowlist")
	}

	// size / oversize bounds.
	if len(p.Diff) > lim.MaxDiffBytes {
		fail = append(fail, "diff exceeds the size bound")
	}
	if len(touched) > lim.MaxChangedFiles {
		fail = append(fail, "diff touches too many files")
	}

	return fail
}

func scanSecret(line string) string {
	for _, re := range secretPatterns {
		if re.MatchString(line) {
			return "known credential pattern"
		}
	}
	// entropy heuristic: a long, high-entropy token assigned to a secret-ish key.
	if looksLikeAssignedSecret(line) {
		return "high-entropy token"
	}
	return ""
}

var assignRe = regexp.MustCompile(`(?i)(secret|token|key|password|passwd|credential)\w*\s*[=:]\s*["']?([A-Za-z0-9/+_\-]{20,})`)

func looksLikeAssignedSecret(line string) bool {
	m := assignRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	return shannonEntropy(m[2]) >= 3.5
}

// shannonEntropy returns the Shannon entropy (bits/char) of s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		pi := c / n
		h -= pi * math.Log2(pi)
	}
	return h
}

// introducesBinary reports whether the unified diff adds a Git binary patch (the
// "GIT binary patch" or "Binary files ... differ" markers).
func introducesBinary(diff string) bool {
	return strings.Contains(diff, "GIT binary patch") ||
		strings.Contains(diff, "Binary files ") && strings.Contains(diff, " differ")
}

// ── unified-diff parsing (structural, deterministic) ────────────────────────────

// TouchedPaths parses a unified/git diff and returns the de-duplicated, normalized
// set of paths it touches. It understands `diff --git a/x b/y`, `+++ b/x`,
// `--- a/x`, and rename headers. It ignores /dev/null (an add/delete sentinel). The
// "b/" (post-image) path is preferred; a delete falls back to the "a/" path.
func TouchedPaths(diff string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		np := normalizePath(p)
		if np == "" || np == "/dev/null" {
			return
		}
		if !seen[np] {
			seen[np] = true
			out = append(out, np)
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			a, b := parseDiffGit(line)
			add(b)
			if b == "" {
				add(a)
			}
		case strings.HasPrefix(line, "+++ "):
			add(stripDiffPathPrefix(strings.TrimSpace(line[4:])))
		case strings.HasPrefix(line, "--- "):
			// only used as a fallback for deletes (+++ is /dev/null).
			p := stripDiffPathPrefix(strings.TrimSpace(line[4:]))
			if p == "/dev/null" {
				continue
			}
		case strings.HasPrefix(line, "rename to "):
			add(strings.TrimSpace(line[len("rename to "):]))
		case strings.HasPrefix(line, "rename from "):
			add(strings.TrimSpace(line[len("rename from "):]))
		}
	}
	// the +++/--- lines are the authority for deletes: re-scan for delete targets
	// whose +++ was /dev/null.
	scanDeletes(diff, add)
	sort.Strings(out)
	return out
}

func scanDeletes(diff string, add func(string)) {
	lines := strings.Split(diff, "\n")
	for i := 0; i < len(lines)-1; i++ {
		if strings.HasPrefix(lines[i], "--- ") && strings.HasPrefix(lines[i+1], "+++ ") {
			from := stripDiffPathPrefix(strings.TrimSpace(lines[i][4:]))
			to := stripDiffPathPrefix(strings.TrimSpace(lines[i+1][4:]))
			if to == "/dev/null" && from != "/dev/null" {
				add(from)
			}
		}
	}
}

// parseDiffGit extracts the a/ and b/ paths from a `diff --git a/x b/y` line.
func parseDiffGit(line string) (a, b string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	fields := strings.SplitN(rest, " ", 2)
	if len(fields) != 2 {
		return "", ""
	}
	return stripDiffPathPrefix(fields[0]), stripDiffPathPrefix(fields[1])
}

// stripDiffPathPrefix removes a leading a/ or b/ and a trailing tab-annotation.
func stripDiffPathPrefix(p string) string {
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	if p == "/dev/null" {
		return p
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return p
}

// addedLines returns the content of '+' lines (added/modified), excluding the
// '+++' file header.
func addedLines(diff string) []string {
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			out = append(out, line[1:])
		}
	}
	return out
}

// ── path helpers ────────────────────────────────────────────────────────────────

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "/dev/null" {
		return p
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func hasAnyBase(p string, names ...string) bool {
	base := baseName(p)
	for _, n := range names {
		if base == n || p == n {
			return true
		}
	}
	return false
}

func dedup(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
