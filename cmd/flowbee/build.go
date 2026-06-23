package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runBuild is the canonical deploy build path. By default it fetches origin/main and
// refuses to build from a local tree that is behind origin/main or dirty, because that
// silently ships reverted fixes. Use --allow-dirty only for an intentional emergency
// local build.
func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	output := fs.String("output", "bin/flowbee", "output binary path")
	allowDirty := fs.Bool("allow-dirty", false, "allow building from a dirty or behind local tree")
	skipFetch := fs.Bool("skip-fetch", false, "skip fetching origin/main before the guard check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: flowbee build [--output PATH] [--allow-dirty]")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root, err := gitOut(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("locate git repo: %w", err)
	}
	root = strings.TrimSpace(root)
	if !*skipFetch {
		if _, err := gitOut(ctx, root, "fetch", "--quiet", "origin", "main"); err != nil {
			return fmt.Errorf("fetch origin/main: %w", err)
		}
	}
	head, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	behindOut, err := gitOut(ctx, root, "rev-list", "--count", "HEAD..origin/main")
	if err != nil {
		return fmt.Errorf("compare HEAD to origin/main: %w", err)
	}
	behind := strings.TrimSpace(behindOut)
	status, err := gitOut(ctx, root, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read worktree status: %w", err)
	}
	dirty := strings.TrimSpace(status) != ""
	if !*allowDirty && (behind != "0" || dirty) {
		return fmt.Errorf("refusing to build from local tree: HEAD=%s behind_origin_main_by=%s dirty=%v; run from clean origin/main or pass --allow-dirty for an intentional local build",
			shortSHA(head), behind, dirty)
	}
	if *allowDirty && (behind != "0" || dirty) {
		fmt.Fprintf(os.Stderr, "WARN: building from local tree behind_origin_main_by=%s dirty=%v HEAD=%s; merged fixes may be missing\n",
			behind, dirty, shortSHA(head))
	}

	outPath := *output
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(root, outPath)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, "./cmd/flowbee")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	fmt.Printf("built %s from %s (dirty=%v, behind_origin_main_by=%s)\n", outPath, shortSHA(head), dirty, behind)
	return nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("%v: %s", err, msg)
		}
		return "", err
	}
	return string(out), nil
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
