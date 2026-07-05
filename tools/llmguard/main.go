// Command llmguard enforces the backend LLM boundary: production provider SDKs
// and OpenRouter runtime clients may only be imported or constructed under
// internal/llm. Benchmark lab code is explicitly exempt.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var forbiddenImportFragments = []string{
	"openrouter",
	"anthropic",
	"go-openai",
	"openai-go",
	"generative-ai-go",
}

var forbiddenConstructionFragments = []string{
	"openrouterruntime.New",
	"openrouter.New",
	"anthropic.New",
	"openai.New",
	"genai.New",
}

var modelLiteralFragments = []string{
	"openrouter/",
	"anthropic/",
	"openai/",
	"claude-",
	"gpt-",
	"gemini-",
	"haiku",
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	violations, err := lintRoot(root)
	if err != nil {
		fmt.Printf("llmguard: %v\n", err)
		os.Exit(1)
	}
	for _, v := range violations {
		fmt.Println(v)
	}
	if len(violations) > 0 {
		fmt.Printf("llmguard: %d violation(s); use internal/llm for provider calls\n", len(violations))
		os.Exit(1)
	}
	fmt.Println("llmguard: provider boundary clean")
}

func lintRoot(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || allowedPath(path) {
			return nil
		}
		vs, err := lintFile(path)
		if err != nil {
			return err
		}
		violations = append(violations, vs...)
		return nil
	})
	return violations, err
}

func lintFile(path string) ([]string, error) {
	var violations []string
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		for _, frag := range forbiddenImportFragments {
			if strings.Contains(strings.ToLower(importPath), frag) {
				pos := fset.Position(imp.Pos())
				violations = append(violations, fmt.Sprintf("VIOLATION: %s imports provider package %q; route through internal/llm", pos, importPath))
			}
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(body)
	for _, frag := range forbiddenConstructionFragments {
		if strings.Contains(text, frag) {
			violations = append(violations, fmt.Sprintf("VIOLATION: %s constructs provider client %q outside internal/llm", path, frag))
		}
	}
	modelViolations, err := lintHardcodedModelLiterals(path)
	if err != nil {
		return nil, err
	}
	violations = append(violations, modelViolations...)
	return violations, nil
}

func lintHardcodedModelLiterals(path string) ([]string, error) {
	var violations []string
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if !modelNameLooksRoutable(name.Name) || i >= len(x.Values) {
					continue
				}
				if lit, ok := x.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING && modelLiteralLooksProviderBacked(lit.Value) {
					pos := fset.Position(lit.Pos())
					violations = append(violations, fmt.Sprintf("VIOLATION: %s hardcodes model literal for %s; resolve model_slot_binding through internal/llm", pos, name.Name))
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || !modelNameLooksRoutable(id.Name) || i >= len(x.Rhs) {
					continue
				}
				if lit, ok := x.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.STRING && modelLiteralLooksProviderBacked(lit.Value) {
					pos := fset.Position(lit.Pos())
					violations = append(violations, fmt.Sprintf("VIOLATION: %s hardcodes model literal for %s; resolve model_slot_binding through internal/llm", pos, id.Name))
				}
			}
		}
		return true
	})
	return violations, nil
}

func modelNameLooksRoutable(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "model") && !strings.Contains(lower, "family") && !strings.Contains(lower, "label")
}

func modelLiteralLooksProviderBacked(raw string) bool {
	value, err := strconv.Unquote(raw)
	if err != nil {
		return false
	}
	lower := strings.ToLower(value)
	for _, frag := range modelLiteralFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

func allowedPath(path string) bool {
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/internal/llm/") ||
		strings.HasPrefix(p, "internal/llm/") ||
		strings.Contains(p, "/reports/model-lab/grand-bakeoff/") ||
		strings.HasPrefix(p, "reports/model-lab/grand-bakeoff/") ||
		strings.Contains(p, "/tools/llmguard/") ||
		strings.HasPrefix(p, "tools/llmguard/")
}
