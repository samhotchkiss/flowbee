package maintenance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionEllieJudgeCallsStayBehindLedgerGate(t *testing.T) {
	root := ellieRoot(t)
	var offenders []string
	walkGoFiles(t, root, func(path string) {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel := relSlash(t, root, path)
		if rel == "maintenance/candidate.go" || strings.HasSuffix(rel, "_test.go") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isJudgeCall(call.Fun) {
				offenders = append(offenders, rel)
			}
			return true
		})
	})
	if len(offenders) > 0 {
		t.Fatalf("production Ellie judge calls must go through maintenance.RunLLMSweep ledger gate; direct calls in %s", strings.Join(unique(offenders), ", "))
	}
}

func TestProductionEllieDoesNotReintroduceWallClockSweepCursor(t *testing.T) {
	root := ellieRoot(t)
	var offenders []string
	forbidden := []string{"ReadSweepCursor", "DefaultInterval"}
	walkGoFiles(t, root, func(path string) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, s := range forbidden {
			if strings.Contains(string(body), s) {
				offenders = append(offenders, relSlash(t, root, path)+":"+s)
			}
		}
	})
	if len(offenders) > 0 {
		t.Fatalf("wall-clock cursor sweep markers reintroduced in production Ellie code: %s", strings.Join(offenders, ", "))
	}
}

func isJudgeCall(expr ast.Expr) bool {
	switch fun := expr.(type) {
	case *ast.Ident:
		return strings.EqualFold(fun.Name, "judge")
	case *ast.SelectorExpr:
		return strings.EqualFold(fun.Sel.Name, "judge")
	default:
		return false
	}
}

func ellieRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Clean(filepath.Join(".."))
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("stat ellie root: %v", err)
	}
	return root
}

func walkGoFiles(t *testing.T, root string, visit func(string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			visit(path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

func relSlash(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("rel %s: %v", path, err)
	}
	return filepath.ToSlash(rel)
}

func unique(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
