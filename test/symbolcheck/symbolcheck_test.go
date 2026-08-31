// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package symbolcheck

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// repoRoot is the tree every test resolves against: the real repository,
// not a fixture of it.
const repoRoot = "../.."

// sharedIndex type-checks the repository once for the whole package. The
// load is a few seconds and every test needs the same answer from it.
var sharedIndex = sync.OnceValues(func() (*symbolIndex, error) {
	return loadIndex(repoRoot)
})

func index(t *testing.T) *symbolIndex {
	t.Helper()

	ix, err := sharedIndex()
	if err != nil {
		t.Fatalf("loading the repository's packages: %v", err)
	}

	return ix
}

// writeDoc puts one markdown fixture on disk under the name a failure
// message will quote.
func writeDoc(t *testing.T, src string) (root, doc string) {
	t.Helper()

	root = t.TempDir()
	doc = "fixture.md"
	if err := os.WriteFile(filepath.Join(root, doc), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	return root, doc
}

// TestCheckFixtures runs the whole pipeline — scan, allowlist, resolve —
// over one-line documents against the real package index. want is the
// substring the single failure must contain; "" means the document must
// produce none.
func TestCheckFixtures(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "package symbol resolves",
			src:  "The entry point is `engine.Compute`.\n",
		},
		{
			name: "package symbol does not exist",
			src:  "The entry point is `engine.Recompute`.\n",
			want: "fixture.md:1: `engine.Recompute` does not resolve: package " +
				"github.com/NightWatchEng/shortfall/engine exports no Recompute",
		},
		{
			name: "package does not exist",
			src:  "Hand it to `nope.Thing` and it lands.\n",
			want: `no package named "nope" in this repository`,
		},
		{
			name: "method reference resolves",
			src:  "`emit.Std.Flush` releases the mutex first.\n",
		},
		{
			name: "method reference does not exist",
			src:  "`emit.Std.Flushh` releases the mutex first.\n",
			want: "emit.Std has no exported field or method Flushh",
		},
		{
			name: "struct field reference resolves",
			src:  "`biz.ValueContext.Estimated` suppresses the value point.\n",
		},
		{
			name: "package-qualified method resolves the way go doc reads it",
			src:  "`emit.Record` is called on every stage transition.\n",
		},
		{
			name: "package-qualified method that exists nowhere fails",
			src:  "`emit.Reccord` is called on every stage transition.\n",
			want: "and no exported type in it has a method Reccord",
		},
		{
			name: "bare type member resolves",
			src:  "The metrics-only fallback for `Leg.Count` carries a caveat.\n",
		},
		{
			name: "bare type member on the wrong type fails",
			src:  "The deferred leg reports `Leg.NotAvailable`.\n",
			want: "no exported type named Leg in this repository has a field or method NotAvailable",
		},
		{
			name: "promoted field resolves through the embedded type",
			src:  "`DeferredLeg.Count` counts in-flight transactions.\n",
		},
		{
			name: "receiver variable resolves against its type",
			src:  "Defer `em.Close` and call `em.Record` at every transition.\n",
		},
		{
			name: "receiver variable with a member its type lacks fails",
			src:  "Defer `em.Recordd` at every transition.\n",
			want: "emit.Std has no exported field or method Recordd",
		},
		{
			name: "trailing parens are a method reference",
			src:  "`vc.Validate()` runs first — the PII guard lives here.\n",
		},
		{
			name: "trailing parens on a member that does not exist",
			src:  "`vc.Validatee()` runs first.\n",
			want: "fixture.md:1: `vc.Validatee()` does not resolve",
		},
		{
			name: "standard library selector is allowlisted",
			src:  "The value context rides on a `context.Context`.\n",
		},
		{
			name: "fenced block is not prose",
			src:  "Before.\n\n```go\n// `engine.Recompute` and `nope.Thing`\n```\n\nAfter.\n",
		},
		{
			name: "tilde fence is not prose",
			src:  "Before.\n\n~~~go\n// `engine.Recompute`\n~~~\n\nAfter.\n",
		},
		{
			name: "unexported selector is not a Go symbol reference",
			src:  "The member key is `biz.vc` and the field is `biz.amount_minor`.\n",
		},
		{
			name: "filename is not a selector",
			src:  "See `README.md`, `go.mod` and `main.go`.\n",
		},
		{
			name: "a symbol outside a code span is not checked",
			src:  "Prose about engine.Recompute without backticks.\n",
		},
		{
			name: "the failure names the line it is on",
			src:  "one\ntwo\nthree `engine.Recompute` four\n",
			want: "fixture.md:3:",
		},
	}
	ix := index(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, doc := writeDoc(t, c.src)
			refs, err := scanProse(root, doc)
			if err != nil {
				t.Fatal(err)
			}

			got := checkRefs(ix, refs)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no failure, got:\n%s", strings.Join(got, "\n"))
				}

				return
			}

			if len(got) != 1 {
				t.Fatalf("want exactly one failure containing %q, got %d:\n%s",
					c.want, len(got), strings.Join(got, "\n"))
			}

			if !strings.Contains(got[0], c.want) {
				t.Fatalf("failure %q does not contain %q", got[0], c.want)
			}
		})
	}
}

// TestScanProse pins the scanner on its own: what counts as a span, what
// counts as a fence, and that a fence which never closes is an error
// rather than a silent skip of everything after it.
func TestScanProse(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    []string
		wantErr string
	}{
		{"one span", "a `engine.Compute` b\n", []string{"engine.Compute"}, ""},
		{"two spans on a line", "`a.Bee` and `c.Dee`\n", []string{"a.Bee", "c.Dee"}, ""},
		{"trailing parens normalized", "`vc.Validate()`\n", []string{"vc.Validate"}, ""},
		{"unterminated span ignored", "a `engine.Compute\n", nil, ""},
		{"fence skipped", "```go\n`a.Bee`\n```\n`c.Dee`\n", []string{"c.Dee"}, ""},
		{"indented fence skipped", "  ```go\n`a.Bee`\n  ```\n", nil, ""},
		{"unclosed fence is an error", "```go\n`a.Bee`\n", nil, "never closed"},
		{"three-deep selector not scanned", "`a.Bee.Cee.Dee`\n", nil, ""},
		{"lowercase member not scanned", "`biz.vc`\n", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, doc := writeDoc(t, c.src)
			refs, err := scanProse(root, doc)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, r := range refs {
				got = append(got, r.text)
			}

			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("scanned %v, want %v", got, c.want)
			}
		})
	}
}

// TestGovernedDocs pins the scope decision: README.md and the guides are
// in, the ADR tree is out, and the set is discovered rather than listed.
func TestGovernedDocs(t *testing.T) {
	docs, err := governedDocs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	in := map[string]bool{}
	for _, d := range docs {
		in[d] = true
		if strings.HasPrefix(d, "docs/adr/") {
			t.Errorf("%s is under the exempt ADR tree", d)
		}
	}

	for _, want := range []string{
		"README.md",
		"adapters/README.md",
		"docs/adapters.md",
		"docs/architecture/money-path.md",
		"docs/performance.md",
	} {
		if !in[want] {
			t.Errorf("%s is not governed", want)
		}
	}
}

// TestThisRepoDocsCiteRealSymbols is the drift guard: every selector the
// real documentation writes in prose must resolve against the real tree,
// on every PR rather than at review time.
func TestThisRepoDocsCiteRealSymbols(t *testing.T) {
	ix := index(t)
	docs, err := governedDocs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	var (
		refs   []ref
		failed []string
	)
	for _, doc := range docs {
		found, err := scanProse(repoRoot, doc)
		if err != nil {
			t.Fatal(err)
		}

		refs = append(refs, found...)
	}

	failed = checkRefs(ix, refs)
	if len(failed) > 0 {
		t.Errorf("%d documented symbol(s) do not resolve:\n%s\n\n"+
			"Each line is doc:line followed by the identifier as the doc writes it. "+
			"Correct the prose to name the symbol that exists; if the identifier is "+
			"not this repository's — standard library, a third-party SDK, a provider "+
			"payload field — add it to allowSelectors in symbolcheck.go with the reason.",
			len(failed), strings.Join(failed, "\n"))
	}

	// A scanner that silently stopped matching would report zero failures
	// forever. The floor is far below the current count and only has to
	// prove the scan reached the docs.
	if len(refs) < 50 {
		t.Fatalf("only %d selector(s) scanned across %d docs — the check is vacuous",
			len(refs), len(docs))
	}
}

// TestAllowlistIsLive fails on an allowlist entry no governed doc uses.
// A stale exemption is a hole nobody is watching: the identifier it names
// could come back as a real symbol reference and never be checked.
func TestAllowlistIsLive(t *testing.T) {
	docs, err := governedDocs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	used := map[string]bool{}
	for _, doc := range docs {
		refs, err := scanProse(repoRoot, doc)
		if err != nil {
			t.Fatal(err)
		}

		for _, r := range refs {
			used[r.text] = true
		}
	}

	for sel, why := range allowSelectors {
		if !used[sel] {
			t.Errorf("allowSelectors[%q] (%s) is not cited by any governed doc — remove it", sel, why)
		}
	}

	for name := range receiverTypes {
		found := false
		for sel := range used {
			if strings.HasPrefix(sel, name+".") {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("receiverTypes[%q] is not cited by any governed doc — remove it", name)
		}
	}
}
