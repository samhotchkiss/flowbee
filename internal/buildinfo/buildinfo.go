package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Info is the source provenance embedded in the running binary by the Go toolchain.
type Info struct {
	Version      string
	SourceCommit string
	TreeDirty    bool
}

// OriginStatus is the runtime comparison between Info.SourceCommit and origin/main.
type OriginStatus struct {
	BehindBy int
	Checked  bool
	Warning  string
	Err      string
}

// Current returns the running binary's version plus exact VCS provenance. The injected
// version is kept for release builds, but SourceCommit/TreeDirty still come from
// debug.ReadBuildInfo so a plain local rebuild remains auditable.
func Current(injectedVersion string) Info {
	var rev string
	var dirty bool
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	return Info{
		Version:      versionString(injectedVersion, rev, dirty),
		SourceCommit: rev,
		TreeDirty:    dirty,
	}
}

func versionString(injected, rev string, dirty bool) string {
	if injected != "" && injected != "dev" {
		return injected
	}
	if rev == "" {
		if injected != "" {
			return injected
		}
		return "dev"
	}
	short := rev
	if len(short) > 12 {
		short = short[:12]
	}
	if dirty {
		short += "+dirty"
	}
	return "dev-" + short
}

// CheckOriginMain fetches origin/main and counts commits between the binary's source
// commit and origin/main. It is intentionally best-effort: inability to inspect git is
// reported as Err, while a real behind/dirty finding becomes Warning.
func CheckOriginMain(ctx context.Context, repoDir string, info Info, fetch bool) OriginStatus {
	var st OriginStatus
	if info.SourceCommit == "" {
		st.Err = "source commit unavailable"
		return st
	}
	if repoDir == "" {
		repoDir = "."
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if fetch {
		if out, err := git(ctx, repoDir, "fetch", "--quiet", "origin", "main"); err != nil {
			st.Err = fmt.Sprintf("fetch origin/main: %v%s", err, outputSuffix(out))
			return st
		}
	}
	out, err := git(ctx, repoDir, "rev-list", "--count", info.SourceCommit+"..origin/main")
	if err != nil {
		st.Err = fmt.Sprintf("compare %s..origin/main: %v%s", short(info.SourceCommit), err, outputSuffix(out))
		return st
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		st.Err = "parse behind count: " + err.Error()
		return st
	}
	st.Checked = true
	st.BehindBy = n
	if n > 0 {
		st.Warning = BehindWarning(n, info.SourceCommit, info.TreeDirty)
		return st
	}
	if info.TreeDirty {
		st.Warning = DirtyWarning(info.SourceCommit)
	}
	return st
}

func BehindWarning(n int, commit string, dirty bool) string {
	return fmt.Sprintf("WARN: running binary is %d commits behind origin/main (built from %s, dirty=%v) - merged fixes may be missing",
		n, short(commit), dirty)
}

func DirtyWarning(commit string) string {
	return fmt.Sprintf("WARN: running binary was built from a dirty tree (built from %s, dirty=true) - uncommitted code may differ from origin/main",
		short(commit))
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func outputSuffix(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	return ": " + out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
