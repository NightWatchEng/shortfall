package docsnippets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NightWatchEng/shortfall/registry"
)

// checkedDocs maps each governed doc to the stub source for its
// doc-implied identifiers ("" when the doc's fences are self-contained).
// Adding a fence with a new doc-implied identifier extends the stub —
// the compile failure names it.
var checkedDocs = map[string]string{
	"README.md": "",
	"docs/adapters.md": `package docsnippet

import (
	"context"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/registry"
)

var (
	ctx = context.Background()
	reg registry.Registry
	req engine.Request
)
`,
	"docs/integration-webhook-lambdas.md": `package docsnippet

import (
	"context"
	"io"
)

// ProviderWebhook stands in for the reader's decoded webhook payload.
type ProviderWebhook struct {
	PaymentIntentID string
	AccountID       string
	Segment         string
	AmountMinor     int64
}

func hash(s string) string                        { return "h:" + s }
func body(ProviderWebhook) io.Reader              { return nil }
func processWebhook(context.Context) error        { return nil }
func classify(error) string                       { return "" }
`,
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestDocGoFencesCompile is the promoted mechanical slice: every Go
// fence in every governed doc compiles verbatim (statements wrapped,
// declarations as files) against the real modules.
func TestDocGoFencesCompile(t *testing.T) {
	root := repoRoot(t)
	for doc, stubs := range checkedDocs {
		t.Run(strings.ReplaceAll(doc, "/", "_"), func(t *testing.T) {
			fences, err := extractFences(filepath.Join(root, doc))
			if err != nil {
				t.Fatal(err)
			}
			var goFences []fence
			for _, f := range fences {
				if f.lang == "go" {
					goFences = append(goFences, f)
				}
			}
			if len(goFences) == 0 {
				t.Fatalf("%s: no Go fences found — governed docs must have some, or leave the map", doc)
			}
			if err := compileGoFences(root, t.TempDir(), goFences, stubs); err != nil {
				t.Fatalf("%s: %v", doc, err)
			}
		})
	}
}

// TestDocRegistryFencesValidate loads every complete registry example
// (version: + flows:, no "..." elision) through registry.Load.
//
// docs/portability.md is governed here but deliberately NOT in
// checkedDocs: it specifies the cross-language contract for readers
// writing Java and Python, so it carries no Go fences at all (and
// checkedDocs fails a doc that has none). Its registry example still has
// to load, because a port copying a document that does not validate is
// exactly the failure this checker exists to prevent.
func TestDocRegistryFencesValidate(t *testing.T) {
	root := repoRoot(t)
	docs := []string{
		"README.md",
		"docs/integration-webhook-lambdas.md",
		"docs/quickstart.md",
		"docs/registry.md",
		"docs/adapters.md",
		"docs/portability.md",
	}
	found := 0
	for _, doc := range docs {
		t.Run(strings.ReplaceAll(doc, "/", "_"), func(t *testing.T) {
			fences, err := extractFences(filepath.Join(root, doc))
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fences {
				if !registryFence(f) {
					continue
				}
				found++
				p := filepath.Join(t.TempDir(), "registry.yaml")
				if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := registry.Load(p); err != nil {
					t.Errorf("%s:%d registry fence fails validation: %v", doc, f.line, err)
				}
			}
		})
	}
	if found == 0 {
		t.Fatal("no registry fences found anywhere — the validator would be vacuous")
	}
}

// TestExtractFences pins the parser's input/output behavior, unclosed
// fences included — a fence the parser drops is a fence outside
// governance.
func TestExtractFences(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		wantLangs []string
		wantBody  string
		wantErr   string
	}{
		{"plain fence", "```go\nx()\n```\n", []string{"go"}, "x()\n", ""},
		{"info string", "```go {linenos=true}\nx()\n```\n", []string{"go"}, "x()\n", ""},
		{"indented fence", "- item\n\n  ```go\n  x()\n  ```\n", []string{"go"}, "x()\n", ""},
		{"two fences", "```go\na()\n```\n```yaml\nk: v\n```\n", []string{"go", "yaml"}, "a()\n", ""},
		{"unclosed trailing fence", "```go\nx()\n", nil, "", "never closed"},
		{"closed then unclosed", "```go\na()\n```\n```go\nb()\n", nil, "", "never closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "doc.md")
			if err := os.WriteFile(p, []byte(c.src), 0o644); err != nil {
				t.Fatal(err)
			}
			fences, err := extractFences(p)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var langs []string
			for _, f := range fences {
				langs = append(langs, f.lang)
			}
			if strings.Join(langs, ",") != strings.Join(c.wantLangs, ",") {
				t.Fatalf("langs = %v, want %v", langs, c.wantLangs)
			}
			if fences[0].body != c.wantBody {
				t.Fatalf("body = %q, want %q", fences[0].body, c.wantBody)
			}
		})
	}
}

// TestFileLevel pins the declaration-vs-statements classification,
// leading-comment handling included.
func TestFileLevel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"func decl", "func main() {}\n", true},
		{"statements", "x := 1\n_ = x\n", false},
		{"comment then decl", "// doc\nfunc f() {}\n", true},
		{"indented comment then decl", "  // doc\nvar x int\n", true},
		{"blank then statements", "\nx()\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fileLevel(c.body); got != c.want {
				t.Fatalf("fileLevel = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCheckerCatchesBroken proves the checker is not a vacuous pass:
// each deliberately broken fixture fence must be rejected.
func TestCheckerCatchesBroken(t *testing.T) {
	root := repoRoot(t)
	fences, err := extractFences("testdata/broken.md")
	if err != nil {
		t.Fatal(err)
	}
	byLine := func(line int) fence {
		for _, f := range fences {
			if f.line == line {
				return f
			}
		}
		t.Fatalf("fixture fence at line %d not found", line)
		return fence{}
	}
	cases := []struct {
		name    string
		line    int
		check   func(f fence) error
		wantErr string
	}{
		{
			"uncompilable structure", 3,
			func(f fence) error { return compileGoFences(root, t.TempDir(), []fence{f}, "") },
			"go build",
		},
		{
			"declared and not used", 9,
			func(f fence) error { return compileGoFences(root, t.TempDir(), []fence{f}, "") },
			"declared and not used",
		},
		{
			"registry missing reconcile", 13,
			func(f fence) error {
				if !registryFence(f) {
					t.Fatal("fixture registry fence not recognized")
				}
				p := filepath.Join(t.TempDir(), "registry.yaml")
				if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
					return err
				}
				_, err := registry.Load(p)
				return err
			},
			"reconcile source is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.check(byLine(c.line))
			if err == nil {
				t.Fatal("broken fixture passed — the checker is vacuous")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not name the defect (%q)", err, c.wantErr)
			}
		})
	}
}
