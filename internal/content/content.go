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
	// AllowOwnSource drops ONLY the `flowbee_source` denylist class for THIS check.
	// That class (internal/, cmd/flowbee/, tools/, flows/, flowbee.yaml, content.go)
	// protects the CONTROL PLANE's own source — "the agent editing the orchestrator it
	// runs under." It is correct ONLY for the repo that actually contains Flowbee's
	// source. For any OTHER managed repo those are the repo's OWN paths (most Go repos
	// have internal/ + cmd/), so applying flowbee_source there wrongly forces every such
	// change to the human gate, defeating autonomous self-merge. An operator sets this
	// per-repo (allow_own_source_merge) for repos that are NOT the Flowbee control plane.
	// Default false = the shipped, fully-protected posture (no behavior change). The
	// UNIVERSAL classes (CI, lockfiles, dockerfiles, secrets) are NEVER dropped.
	AllowOwnSource bool
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
	if pol.AllowOwnSource {
		// drop ONLY flowbee_source hits — this repo's internal//cmd//tools//flows/ are
		// its OWN source, not the control plane's. Every other class stays in force.
		hits = withoutClass(hits, "flowbee_source")
	}
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
			"composer.lock",
			// go.mod is the dependency MANIFEST: a `replace` directive (redirect a dep to
			// a malicious fork) or a version bump to a compromised release is a supply-chain
			// escalation that go.sum alone does not always catch — protect both.
			"go.mod", "Cargo.toml", "package.json", "pyproject.toml", "Gemfile":
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
			// ALL of tools/ is flowbee's own dev/build source (archcheck, providerlint,
			// seedidentities, …) — a broad prefix so a NEW tool can't slip in un-gated, the
			// way tools/seedidentities/ did when only two subdirs were listed by name.
			strings.HasPrefix(p, "tools/") ||
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

// withoutClass drops every hit of the given denylist class (hits are "class:path").
// Used to relax the flowbee_source class for a repo that is NOT the control plane.
func withoutClass(hits []string, class string) []string {
	prefix := class + ":"
	var out []string
	for _, h := range hits {
		if !strings.HasPrefix(h, prefix) {
			out = append(out, h)
		}
	}
	return out
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
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),                                                  // GitHub token (classic/oauth/server)
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`),                                                // GitHub fine-grained PAT (current default; gh[pousr]_ misses it)
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                                // Slack token
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`),                                                  // Anthropic API key
	regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9]{20,}`),                                               // OpenAI API key (incl. project keys)
	regexp.MustCompile(`\b[rs]k_live_[A-Za-z0-9]{20,}`),                                               // Stripe live secret / restricted key
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),                                                      // Google API key (context-free)
	regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{20,}`),                                // SendGrid API key
	regexp.MustCompile(`ya29\.[A-Za-z0-9_\-]{20,}`),                                                   // Google OAuth access token
	// NOTE: these are CONTEXT-FREE prefix shapes (no left-hand-side keyword needed), closing the
	// "secret assigned to an innocuously-named variable" evasion FOR THESE PROVIDERS. The generic
	// entropy/keyword check below is still LHS-anchored; a prefix-LESS high-entropy secret on a
	// non-secret-ish var (e.g. `config = "<base64>"`) is NOT caught deterministically — a
	// context-free entropy scan was considered but rejected (false-positives on legitimate
	// base64/hash literals). That residual relies on the human merge gate + the LLM reviewer.
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password)\s*[=:]\s*["']?[A-Za-z0-9/+_\-]{24,}`), // generic keyword-anchored
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

	// parse markers (§9.2) — a diff that introduces leftover git conflict markers
	// (<<<<<<< / >>>>>>> / the diff3 base |||||||) corrupts the file. This is reachable from
	// a conflict_resolver that under-resolves a multi-hunk conflict (ordinary LLM fallibility,
	// or a run cut short after the 3-way apply): CI does not catch a marker in a non-compiled
	// file (markdown/json/yaml/fixtures/docs) and the code_reviewer is a non-deterministic LLM,
	// so reject it deterministically — the check this package's doc comment promises. The
	// ======= separator is intentionally NOT matched: it is ambiguous with legitimate content
	// (markdown setext rules, ASCII dividers) and every real conflict is already caught by its
	// surrounding angle-bracket markers.
	for _, line := range addedLines(p.Diff) {
		if isConflictMarker(line) {
			fail = append(fail, "unresolved git conflict marker in the diff")
			break
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

// isConflictMarker reports whether a line is a leftover git conflict marker at column 0:
// exactly seven '<', '>', or '|' (the diff3 base) followed by a space or end-of-line. Exactly
// seven so ASCII art (8+ of the char) is not flagged; '=======' is deliberately excluded
// (see the parse-markers note in staticChecks).
func isConflictMarker(line string) bool {
	for _, c := range []byte{'<', '>', '|'} {
		if len(line) < 7 {
			continue
		}
		seven := true
		for i := 0; i < 7; i++ {
			if line[i] != c {
				seven = false
				break
			}
		}
		if seven && (len(line) == 7 || line[7] == ' ') {
			return true
		}
	}
	return false
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
			// rename headers carry a bare path (no a/ b/), but git C-quotes them too,
			// so unquote before classifying — else a renamed-in non-ASCII path bypasses.
			add(unquoteGitPath(strings.TrimSpace(line[len("rename to "):])))
		case strings.HasPrefix(line, "rename from "):
			add(unquoteGitPath(strings.TrimSpace(line[len("rename from "):])))
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

// parseDiffGit extracts the a/ and b/ paths from a `diff --git a/x b/y` line. Git does NOT
// quote a SPACE in a pathname (even under core.quotepath=false), so a naive split on the first
// space corrupts a space-containing path. That matters because a MODE-CHANGE-only hunk (chmod)
// carries NO +++/---/rename header to recover the path from — the `diff --git` line is the sole
// path source — so a corrupted classification would clear a space-named denylisted file (e.g.
// ".github/workflows/deploy v2.yml") at the autonomous-merge gate. Git emits the two sides as
// `a/<P> b/<P>` with an IDENTICAL <P> for every in-repo (non-rename) diff, so recover <P>
// SYMMETRICALLY first; fall back to the first-space split only when that form doesn't hold (a
// true rename, whose path is carried in the rename headers and parsed independently).
func parseDiffGit(line string) (a, b string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if p := symmetricDiffGitPath(rest); p != "" {
		return p, p
	}
	fields := strings.SplitN(rest, " ", 2)
	if len(fields) != 2 {
		return "", ""
	}
	return stripDiffPathPrefix(fields[0]), stripDiffPathPrefix(fields[1])
}

// symmetricDiffGitPath recovers <P> from `a/<P> b/<P>` (identical sides) — the form git emits
// for every non-rename diff — making a space in <P> safe. Returns "" when the line is not
// symmetric (a rename a/X b/Y with X≠Y, or a C-quoted path that doesn't start with a bare "a/"),
// both of which the caller handles via the first-space split + rename/+++/--- headers.
func symmetricDiffGitPath(rest string) string {
	if !strings.HasPrefix(rest, "a/") {
		return ""
	}
	// rest == "a/" + P + " b/" + P  ⇒  len(rest) == 5 + 2*len(P)  ("a/"=2, " b/"=3).
	n := len(rest) - 5
	if n < 2 || n%2 != 0 {
		return ""
	}
	p := rest[2 : 2+n/2]
	if p != "" && rest == "a/"+p+" b/"+p {
		return p
	}
	return ""
}

// stripDiffPathPrefix removes a leading a/ or b/ and a trailing tab-annotation.
func stripDiffPathPrefix(p string) string {
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	// Decode git's C-quoting BEFORE any prefix logic. git wraps a path holding a
	// byte >= 0x80 (the default core.quotepath=true) in double quotes with octal
	// escapes; the leading '"' would defeat the a/ b/ strip and every denylist
	// classifier, and normalizePath's backslash rewrite would further mangle the
	// octal escapes. Unquoting restores the literal path bytes.
	p = unquoteGitPath(p)
	if p == "/dev/null" {
		return p
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return p
}

// unquoteGitPath decodes a git C-quoted pathname. git quotes any path containing
// a byte >= 0x80 (with core.quotepath on, the default) or a control/special char:
// it wraps the path in double quotes and backslash-escapes with \a \b \t \n \v \f
// \r \" \\ plus octal \NNN for raw bytes. If p is not so quoted it is returned
// unchanged. This is a security boundary: a non-ASCII byte in a filename was a
// total content-gate bypass (a C-quoted ".github/workflows/é.yml" matched no
// classifier and self-merged to main), so the parser must see the true bytes.
func unquoteGitPath(p string) string {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p
	}
	body := p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			b.WriteByte('\\')
			break
		}
		switch e := body[i]; e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'v':
			b.WriteByte('\v')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			if e >= '0' && e <= '7' { // octal \NNN, 1-3 digits
				val := int(e - '0')
				for k := 0; k < 2 && i+1 < len(body) && body[i+1] >= '0' && body[i+1] <= '7'; k++ {
					i++
					val = val*8 + int(body[i]-'0')
				}
				b.WriteByte(byte(val))
			} else {
				b.WriteByte(e) // unknown escape: keep the char literally
			}
		}
	}
	return b.String()
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
