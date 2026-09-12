// Package internal_test enforces the two structural rules the whole design
// rests on, so a future change cannot quietly break them.
package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// packagesAllowedCharm are the only packages permitted to import a Charm
// library. Commands ask the renderer for output; they never style. That is what
// keeps a later theme change, or a full TUI, a single-file change.
var packagesAllowedCharm = map[string]bool{
	"internal/output": true,
	"internal/ciwait": true,
	// main wires fang's color scheme, which needs the fang and lipgloss types.
	"cmd/flow": true,
}

// packagesAllowedOsExec is the single audited gateway to os/exec. Routing
// everything through exec.Runner is what makes flow testable without a real
// git, a real herdr, or a real cluster.
var packagesAllowedOsExec = map[string]bool{
	"internal/exec": true,
}

// packagesAllowedGoGitHub confines the go-github dependency, whose import path
// carries its major version and so ripples through every file that names one of
// its types.
var packagesAllowedGoGitHub = map[string]bool{
	"internal/ghapi": true,
}

func TestImportBoundaries(t *testing.T) {
	root := repoRoot(t)

	forEachGoFile(t, root, func(pkgDir, path string, file *ast.File) {
		isTest := strings.HasSuffix(path, "_test.go")

		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}

			switch {
			case isCharm(importPath) && !packagesAllowedCharm[pkgDir]:
				t.Errorf("%s imports %s\n"+
					"Only internal/output and internal/ciwait may import a Charm library; "+
					"ask the renderer for output instead of styling here.", path, importPath)

			case importPath == "os/exec" && !packagesAllowedOsExec[pkgDir] && !isTest:
				t.Errorf("%s imports os/exec\n"+
					"Every external process must go through internal/exec.Runner, "+
					"which is what makes this testable with a fake.", path)

			case strings.HasPrefix(importPath, "github.com/google/go-github/") &&
				!packagesAllowedGoGitHub[pkgDir]:
				t.Errorf("%s imports %s\n"+
					"go-github must stay behind internal/ghapi's own types; "+
					"mock ghapi.API instead.", path, importPath)
			}
		}
	})
}

// TestTestsDoNotImportGoGitHub is called out separately in the spec: a test
// outside internal/ghapi reaching for go-github means the wrapping has leaked.
func TestTestsDoNotImportGoGitHub(t *testing.T) {
	root := repoRoot(t)

	forEachGoFile(t, root, func(pkgDir, path string, file *ast.File) {
		if !strings.HasSuffix(path, "_test.go") || packagesAllowedGoGitHub[pkgDir] {
			return
		}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(importPath, "github.com/google/go-github/") {
				t.Errorf("%s imports go-github; the ghapi wrapping has leaked", path)
			}
		}
	})
}

func isCharm(importPath string) bool {
	return strings.HasPrefix(importPath, "charm.land/") ||
		strings.HasPrefix(importPath, "github.com/charmbracelet/")
}

// forEachGoFile walks the module, calling fn with the repo-relative package
// directory, the file path, and its parsed AST.
func forEachGoFile(t *testing.T, root string, fn func(pkgDir, path string, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "bin", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(rel), filepath.ToSlash(mustRel(t, root, path)), parsed)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustRel(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}
