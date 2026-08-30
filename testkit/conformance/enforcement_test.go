package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEveryExporterRunsTheSuite is the enforcement wiring: every module
// under adapters/export/ must reference conformance.RunExporter from a test,
// so a new exporter package cannot merge without wiring up the suite. It runs
// in the core module's `go test ./...` and fails the build of any exporter
// that skips it.
//
// It guarantees only that each exporter module's test sources contain a real
// (parsed, non-comment, non-string) reference to conformance.RunExporter —
// the "did you wire it at all" gate. It does not prove the reference
// executes (a t.Skip before it, or `var _ = conformance.RunExporter`, would
// pass); execution is proven by the adapter's own `go test` run in CI.
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
	return moduleRefs(t, modDir, conformancePath, "conformance", "RunExporter")
}

// moduleRefs is moduleRefsSuite generalized over the package and symbol, so
// a second contract can be enforced the same way without a second copy of
// the AST walk.
func moduleRefs(t *testing.T, modDir, importPath, defaultName, symbol string) bool {
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
		if fileRefs(file, importPath, defaultName, symbol) {
			return true
		}
	}
	return false
}

// fileRefsSuite reports whether the parsed file contains a selector
// expression of the form <ident>.RunExporter where the ident resolves to
// the conformance package's import name (default "conformance", or a rename
// in the import spec). A string/comment mention is invisible to the AST.
func fileRefs(file *ast.File, importPath, defaultName, symbol string) bool {
	name := importNameFor(file, importPath, defaultName)
	if name == "" {
		return false // module doesn't import the suite at all
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != symbol {
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
func importNameFor(file *ast.File, path, defaultName string) string {
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
		return defaultName
	}
	return ""
}

const (
	conformancePath = "github.com/NightWatchEng/shortfall/testkit/conformance"
	testkitPath     = "github.com/NightWatchEng/shortfall/testkit"
)

// TestEveryExporterChecksTheEventContract is the same enforcement for the
// outcome event's wire contract. ADR-0002 says every transport produces the
// same fields from the same Outcome; testkit.CheckOutcomeEvent is what
// proves it, and without this a fourth exporter could merge emitting
// whatever it liked — which is how biz.sla.deadline came to ship on one
// transport alone.
func TestEveryExporterChecksTheEventContract(t *testing.T) {
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
			continue
		}
		// An exporter that declares Events=false has no event to check.
		if !moduleDeclaresEvents(t, modDir) {
			continue
		}
		checked++
		if !moduleRefs(t, modDir, testkitPath, "testkit", "CheckOutcomeEvent") {
			offenders = append(offenders, e.Name())
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("event-capable exporter module(s) %v do not reference testkit.CheckOutcomeEvent in any *_test.go — every transport must be held to the outcome event's field set (ADR-0002)", offenders)
	}
	if checked == 0 {
		t.Skip("no event-capable exporter modules found — nothing to enforce yet")
	}
	t.Logf("event contract enforced across %d exporter module(s)", checked)
}

// moduleDeclaresEvents reports whether the module's source says
// Events: true. A metrics-only exporter (prometheus) has no outcome event to
// hold to the contract, and requiring one of it would be a check that could
// only be satisfied vacuously.
func moduleDeclaresEvents(t *testing.T, modDir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(modDir, "*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", modDir, err)
	}
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if strings.Contains(string(b), "Events:            true") || strings.Contains(string(b), "Events: true") {
			return true
		}
	}
	return false
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
