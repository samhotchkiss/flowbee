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

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	output := fs.String("o", "bin/flowbee", "output binary path")
	local := fs.Bool("local", false, "build from the current working tree instead of a clean origin/main worktree")
	allowDirty := fs.Bool("allow-dirty", false, "with --local: allow a dirty or behind working tree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if *local {
		return buildLocal(ctx, out, *allowDirty)
	}
	return buildFromOriginMain(ctx, out)
}

func buildFromOriginMain(ctx context.Context, out string) error {
	if err := runGit(ctx, "fetch", "--quiet", "origin", "main"); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "flowbee-origin-main-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := runGit(ctx, "worktree", "add", "--detach", "--quiet", tmp, "origin/main"); err != nil {
		return err
	}
	defer func() { _ = runGit(context.Background(), "worktree", "remove", "--force", tmp) }()
	commit, err := gitTrim(ctx, "-C", tmp, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := goBuild(ctx, tmp, out, commit, false); err != nil {
		return err
	}
	fmt.Printf("built %s from clean origin/main %s\n", out, shortSHA(commit))
	return nil
}

func buildLocal(ctx context.Context, out string, allowDirty bool) error {
	if err := runGit(ctx, "fetch", "--quiet", "origin", "main"); err != nil {
		return err
	}
	commit, err := gitTrim(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	dirty := strings.TrimSpace(gitOut(ctx, "status", "--porcelain")) != ""
	_, behind, known := originMainDelta(ctx, commit, false)
	if !allowDirty && (dirty || (known && behind > 0)) {
		return fmt.Errorf("refusing local build: dirty=%v behind_origin_main_by=%d; use default `flowbee build` to build clean origin/main, or pass --local --allow-dirty intentionally", dirty, behind)
	}
	if err := goBuild(ctx, ".", out, commit, dirty); err != nil {
		return err
	}
	fmt.Printf("built %s from local %s dirty=%v behind_origin_main_by=%d\n", out, shortSHA(commit), dirty, behind)
	return nil
}

func goBuild(ctx context.Context, dir, out, commit string, dirty bool) error {
	short := shortSHA(commit)
	ldflags := fmt.Sprintf("-X main.version=%s -X main.sourceCommit=%s -X main.sourceDirty=%v", short, commit, dirty)
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", out, "./cmd/flowbee")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, b)
	}
	return nil
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, b)
	}
	return nil
}

func gitTrim(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, b)
	}
	return strings.TrimSpace(string(b)), nil
}
