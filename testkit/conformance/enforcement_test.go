package conformance

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEveryExporterRunsTheSuite is the enforcement wiring: every module
// under adapters/export/ MUST call conformance.RunExporter from a test, so
// a new exporter package cannot merge without proving it conforms. This
// test runs in the core module's `go test ./...` (CI's core checks), needs
// no gate-surface change, and fails the build of any exporter that skips
// the suite.
//
// "Runs the suite" is judged structurally: the module contains a *_test.go
// that references conformance.RunExporter. That is deliberately shallow —
// it guarantees the call site exists and CI executes it; whether the
// harness is faithful is the harness author's responsibility and the
// suite's own assertions catch a lying harness at run time.
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
		t.Fatalf("exporter module(s) %v do not call conformance.RunExporter in any *_test.go — every exporter must pass the conformance suite", offenders)
	}
	if checked == 0 {
		t.Skip("no exporter modules found under adapters/export — nothing to enforce yet")
	}
	t.Logf("conformance enforced across %d exporter module(s)", checked)
}

// moduleRefsSuite reports whether any *_test.go in the module directory (top
// level only — exporters are single-package modules) references
// conformance.RunExporter.
func moduleRefsSuite(t *testing.T, modDir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(modDir, "*_test.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", modDir, err)
	}
	for _, f := range matches {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if strings.Contains(string(b), "conformance.RunExporter") {
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
