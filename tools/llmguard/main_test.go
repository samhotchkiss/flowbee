package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLlmguardRejectsDirectOpenRouterOutsideRouter(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/feature/call.go", `package feature

import openrouter "github.com/acme/openrouter-go"

func call() {
	_ = openrouter.New
}
`)
	violations, err := lintRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected direct provider construction violation")
	}
	if !strings.Contains(violations[0], "internal/llm") {
		t.Fatalf("violation should point to internal/llm: %v", violations)
	}
}

func TestLlmguardAllowsRouterAndBenchmarkLab(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/llm/openrouter.go", `package llm

import openrouter "github.com/acme/openrouter-go"

func call() {
	_ = openrouter.New
}
`)
	write(t, root, "reports/model-lab/grand-bakeoff/run.go", `package main

import anthropic "github.com/acme/anthropic-go"

func main() {
	_ = anthropic.New
}
`)
	violations, err := lintRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

func TestLlmguardRejectsHardcodedModelConstantOutsideRouter(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/feature/models.go", `package feature

const defaultClassifierModel = "openrouter/anthropic/claude-3-5-haiku"
`)
	violations, err := lintRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected hardcoded model literal violation")
	}
	if !strings.Contains(violations[0], "model_slot_binding") {
		t.Fatalf("violation should point to model_slot_binding: %v", violations)
	}
}

func TestLlmguardAllowsWorkerModelFamilyLabels(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cmd/flowbee/fleet.go", `package main

const fleetBuilderFamily = "sonnet"
`)
	violations, err := lintRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
