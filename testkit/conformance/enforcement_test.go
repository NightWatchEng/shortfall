package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEveryExporterRunsTheSuite is the enforcement wiring: every module
// under adapters/export/ MUST reference conformance.RunExporter from a test,
// so a new exporter package cannot merge without wiring up the suite. This
// test runs in the core module's `go test ./...` (CI's core checks), needs
// no gate-surface change, and fails the build of any exporter that skips it.
//
// What it guarantees, precisely: each exporter module's test sources contain
// a real (parsed, non-comment, non-string) reference to
// conformance.RunExporter. The check is an AST scan, not a substring grep,
// so a mention in a comment or a string literal does NOT satisfy it. This
// catches the failure it exists to catch — an exporter added with no suite
// wiring at all.
//
// What it does NOT prove: that the reference actually executes. A test that
// references RunExporter but t.Skip()s before reaching it, or a bare
// `var _ = conformance.RunExporter`, would satisfy this tripwire without
// running the suite. Execution is proven elsewhere: the adapter's own
// `go test` run (CI runs every module) is where a real, unskipped call
// enters the suite and its assertions catch a lying harness. This test is
// the "did you wire it at all" gate, not a proof of coverage.
func TestEveryExporterRunsTheSuite(t *testing.T) {
	root := repoRoot(t)
	exportDir := filepath.Join(root, "adapters", "export")
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("no adapters/export directory yet — nothing to enforce")
		}
		t.Fatalf("reading %s: %v", exportDir, err)
	}

	var offenders []string
	var checked int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		modDir := filepath.Join(exportDir, e.Name())
		if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err != nil {
			continue // not a Go module (no exporter to conform)
		}
		checked++
		if !moduleRefsSuite(t, modDir) {
			offenders = append(offenders, e.Name())
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("exporter module(s) %v do not reference conformance.RunExporter in any *_test.go — every exporter must wire up the conformance suite", offenders)
	}
	if checked == 0 {
		t.Skip("no exporter modules found under adapters/export — nothing to enforce yet")
	}
	t.Logf("conformance enforced across %d exporter module(s)", checked)
}

// moduleRefsSuite reports whether any *_test.go in the module directory (top
// level only — exporters are single-package modules) contains a real code
// reference to conformance.RunExporter. It parses each file and looks for
// the selector expression in the AST, so a mention in a comment or a string
// literal does not count — only a genuine reference in the source does.
func moduleRefsSuite(t *testing.T, modDir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(modDir, "*_test.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", modDir, err)
	}
	fset := token.NewFileSet()
	for _, f := range matches {
		// Parse without comments: the AST then holds only real code, so a
		// commented-out or documented mention cannot satisfy the check.
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		if fileRefsSuite(file) {
			return true
		}
	}
	return false
}

// fileRefsSuite reports whether the parsed file contains a selector
// expression of the form <ident>.RunExporter where the ident resolves to
// the conformance package's import name (default "conformance", or a rename
// in the import spec). A string/comment mention is invisible to the AST.
func fileRefsSuite(file *ast.File) bool {
	name := conformanceImportName(file)
	if name == "" {
		return false // module doesn't import the suite at all
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RunExporter" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// conformanceImportName returns the local name the file imports the
// testkit/conformance package under (its rename if aliased, else the
// package's own name "conformance"), or "" if the file does not import it.
func conformanceImportName(file *ast.File) string {
	const path = "github.com/NightWatchEng/shortfall/testkit/conformance"
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		// imp.Path.Value is the quoted literal, e.g. "\"...\"".
		if len(imp.Path.Value) < 2 || imp.Path.Value[1:len(imp.Path.Value)-1] != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name // aliased import
		}
		return "conformance"
	}
	return ""
}

// repoRoot resolves the repository root from this test file's own location
// (…/testkit/conformance/enforcement_test.go → three parents up), so the
// test is independent of the working directory `go test` runs it from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate repo root")
	}
	// enforcement_test.go -> conformance -> testkit -> <root>
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
