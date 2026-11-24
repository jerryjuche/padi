package gitops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitexecIsOnlyGitEntryPoint enforces the spec's lint rule: no source file
// outside gitexec.go may call exec.Command, and no git call may use an
// agent-writable workspaces/*/repo path as its working directory.
func TestGitexecIsOnlyGitEntryPoint(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		isGitexec := filepath.Base(path) == "gitexec.go"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		bad := false
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Command" {
				pkg, ok := sel.X.(*ast.Ident)
				if ok && pkg.Name == "exec" && !isGitexec {
					bad = true
				}
			}
			// Host-side git must not take a workspace clone as its dir.
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "Run" && len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok {
					value := strings.ToLower(lit.Value)
					if strings.Contains(value, "workspaces") && strings.Contains(value, "/repo") {
						bad = true
					}
				}
			}
			return true
		})
		if bad {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("git entry-point lint failed; use gitops.Run and gateway-owned dirs: %s", strings.Join(offenders, ", "))
	}
}
