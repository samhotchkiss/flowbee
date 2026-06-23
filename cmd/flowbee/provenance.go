package main

import (
	"context"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// These may be injected by `flowbee build` with -ldflags. Plain `go build` still
// carries equivalent vcs.* settings in debug.BuildInfo when built from a git tree.
var sourceCommit = ""
var sourceDirty = ""

type provenance struct {
	Version               string `json:"version"`
	SourceCommit          string `json:"source_commit,omitempty"`
	TreeDirty             bool   `json:"tree_dirty"`
	TreeDirtyKnown        bool   `json:"tree_dirty_known"`
	OriginMainSHA         string `json:"origin_main_sha,omitempty"`
	BehindOriginMainBy    int    `json:"behind_origin_main_by,omitempty"`
	BehindOriginMainKnown bool   `json:"behind_origin_main_known"`
	Warning               string `json:"warning,omitempty"`
}

func currentProvenance(ctx context.Context, fetch bool) provenance {
	p := provenance{Version: buildVersion()}
	rev, dirty, dirtyKnown := vcsBuildInfo()
	if sourceCommit != "" {
		rev = sourceCommit
	}
	if sourceDirty != "" {
		dirty = sourceDirty == "true"
		dirtyKnown = true
	}
	if rev == "" {
		rev = commitFromVersion(p.Version)
	}
	p.SourceCommit = rev
	p.TreeDirty = dirty
	p.TreeDirtyKnown = dirtyKnown
	if rev != "" {
		p.OriginMainSHA, p.BehindOriginMainBy, p.BehindOriginMainKnown = originMainDelta(ctx, rev, fetch)
	}
	switch {
	case p.BehindOriginMainKnown && p.BehindOriginMainBy > 0:
		p.Warning = "running binary is " + strconv.Itoa(p.BehindOriginMainBy) +
			" commits behind origin/main (built from " + shortSHA(p.SourceCommit) +
			", dirty=" + strconv.FormatBool(p.TreeDirty) + ") - merged fixes may be missing"
	case p.TreeDirtyKnown && p.TreeDirty:
		p.Warning = "running binary was built from a dirty tree (built from " +
			shortSHA(p.SourceCommit) + ") - local-only changes may be present"
	}
	return p
}

func vcsBuildInfo() (rev string, dirty bool, dirtyKnown bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirtyKnown = true
			dirty = s.Value == "true"
		}
	}
	return rev, dirty, dirtyKnown
}

func commitFromVersion(v string) string {
	v = strings.TrimPrefix(v, "flowbee ")
	v = strings.TrimPrefix(v, "dev-")
	v = strings.TrimSuffix(v, "+dirty")
	if len(v) >= 7 && isHexish(v) {
		return v
	}
	return ""
}

func isHexish(s string) bool {
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func originMainDelta(ctx context.Context, commit string, fetch bool) (origin string, behind int, ok bool) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if fetch {
		_ = gitOut(cctx, "fetch", "--quiet", "origin", "main")
	}
	origin = strings.TrimSpace(gitOut(cctx, "rev-parse", "origin/main"))
	if origin == "" {
		return "", 0, false
	}
	count := strings.TrimSpace(gitOut(cctx, "rev-list", "--count", commit+"..origin/main"))
	n, err := strconv.Atoi(count)
	if err != nil {
		return origin, 0, false
	}
	return origin, n, true
}

func gitOut(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
