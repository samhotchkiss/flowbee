package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Git is isolated from the no-argument bootstrap coordinator so architecture
// checks can mechanically distinguish repository discovery from the narrowly
// approved human tmux attach seam. It never accepts shell text.
func (productionBareBootstrapSystem) Git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	body, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(body)), nil
}
