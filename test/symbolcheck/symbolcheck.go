// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package symbolcheck is the prose slice of the docs-accuracy review lens.
// docsnippets compiles the fenced Go blocks; the sentences around them name
// symbols too, and a name in prose that no longer resolves reads exactly
// like one that does. This checker resolves every selector-shaped
// backticked identifier in the governed docs' prose against the real
// packages, so a rename or a deletion fails the ordinary test step naming
// the doc, the line and the identifier.
//
// Two shapes resolve, both through go/types over a real go/packages load
// rather than over the source text, so a coincidental substring elsewhere
// in the tree never satisfies a reference:
//
//   - `pkg.Symbol` and `pkg.Type.Member`, where pkg is a package of this
//     module or of one of the nested adapter modules. Member may be an
//     exported field or an exported method, promoted members and
//     pointer-receiver methods included. A `pkg.Symbol` naming no
//     package-level symbol also resolves against the methods of the
//     package's exported types, which is how `go doc emit.Record` reads it
//     and the form the guides use; the method still has to exist.
//   - `Type.Member` with the package left off, the form the architecture
//     pages use once the owning package is established — `Leg.Count`.
//     Every indexed package is searched for an exported type of that name.
//
// A trailing "()" is stripped before matching, so `vc.Validate()` reads as
// a method reference. receiverTypes extends resolution to the doc-local
// variables the guides write against, so `em.Record` is checked as
// emit.Std.Record rather than exempted.
//
// Not covered, by decision rather than by oversight:
//
//   - A bare single identifier — `NotAvailableReason`, `Compute`. In prose
//     that shape is an ordinary capitalised word, and separating the two
//     needs a resolver for English, not one for Go. Prose that names a
//     marker type's field without its type therefore passes here and stays
//     with the judged slice of the lens.
//   - Unexported selectors — `biz.vc`, `biz.amount_minor`, `invoice.pay`.
//     These are the docs' metric-attribute, wire-key and flow names, which
//     share the selector shape exactly and are validated by the registry
//     and semantic-convention checks instead.
//   - Anything inside a fenced block. docsnippets compiles those, and
//     compiling is the stronger check.
//   - Whether a true sentence is told about a name that resolves. The
//     answer here is "this name exists", never "this claim is right":
//     prose citing a real field and describing the wrong unit passes, and
//     that judgment stays with the reviewers.
//   - The ADR tree. See notGoverned.
//
// allowSelectors carries the identifiers that are selector-shaped, in
// scope, and legitimately not this repository's; every entry states why.
// It is consulted before resolution, so an allowlisted name is never
// checked against the tree at all.
package symbolcheck

import (
	"bufio"
	"errors"
	"fmt"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/go/packages"
)

// The two shapes this checker resolves.
//
// qualifiedRE is a lowercase package qualifier followed by one or two
// exported members: `engine.Compute`, `emit.Std.Flush`. The lowercase
// qualifier keeps filenames (`README.md`) out and the exported members
// keep attribute and wire keys (`biz.amount_minor`) out.
//
// bareMemberRE is a type member written without its package: `Leg.Count`.
// Both halves must be exported, which is what separates it from a
// filename — the extension of one is never capitalised.
var (
	qualifiedRE  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:\.[A-Z][A-Za-z0-9_]*){1,2}$`)
	bareMemberRE = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*\.[A-Z][A-Za-z0-9_]*$`)
)

// selectorShaped reports whether s is an identifier this checker resolves.
func selectorShaped(s string) bool {
	return qualifiedRE.MatchString(s) || bareMemberRE.MatchString(s)
}

// ref is one selector-shaped identifier found in prose, with enough
// position to name in a failure.
type ref struct {
	doc  string // repo-relative path
	line int    // 1-based line the identifier sits on
	text string // resolvable form: the raw text without a trailing "()"
	raw  string // as written in the doc, for the failure message
}

// allowSelectors names every selector-shaped identifier the governed docs
// use that is not a symbol of this repository, mapped to the reason it is
// exempt. A name here is never resolved, so an entry that shadows a real
// package (stripe) silently stops checking that name — add one only for an
// identifier that genuinely belongs to someone else's API.
var allowSelectors = map[string]string{
	"b.N":               "stdlib testing.B: what BENCH_TIME sets, in the bench guide",
	"b.RunParallel":     "stdlib testing.B: the concurrency benchmarks' driver",
	"context.Context":   "stdlib: what the value context rides on",
	"http.Client":       "stdlib net/http: what the egress fence wraps",
	"http.RoundTripper": "stdlib net/http: the interface the fence implements",
	"sync.Mutex":        "stdlib: named in the contention discussion",

	"decimal.Decimal":           "shopspring/decimal: the Go answer to Java BigDecimal",
	"otel.GetTextMapPropagator": "go.opentelemetry.io/otel: the propagator reused at ingress",
	"stripe.Backend":            "stripe-go, not adapters/payment/stripe: the SDK interface wrapped",
	"stripe.SetBackend":         "stripe-go: the SDK call that installs it",

	"event.Created": "Stripe webhook payload field, not a Go symbol: read for event time",
	"event.ID":      "Stripe webhook payload field: read for idempotency",
}

// receiverTypes maps the doc-local variable names the guides write against
// to the type each one stands for, so a method or field named on a running
// example resolves for real instead of taking a blanket exemption.
var receiverTypes = map[string]string{
	"em": "emit.Std",             // integration guide: em, err := emit.New(...)
	"tr": "emit.InFlightTracker", // integration guide: tr := emit.NewInFlightTracker(...)
	"vc": "biz.ValueContext",     // money path: the step table's stamped context
}

// notGoverned names the doc trees whose prose is left alone. An ADR
// records a decision as it was made and is superseded rather than edited
// (ADR-0008), so it may legitimately name a symbol a later decision
// renamed; failing CI on that would push maintainers to rewrite history to
// keep a checker green. docs/adr/README.md indexes those records in their
// own vocabulary and is amended only by appending a row, so it shares the
// exemption rather than standing as the one governed file inside an exempt
// tree — it cites no selector today, so the choice costs no coverage.
var notGoverned = map[string]bool{"docs/adr": true}

// governedDocs returns the repo-relative markdown whose prose is checked:
// README.md, adapters/README.md, and every .md under docs/ outside
// notGoverned. That is the documentation which describes the library's API
// to a reader; CONTRIBUTING.md, SECURITY.md and CODE_OF_CONDUCT.md
// describe the project rather than the API and are not in the set.
// Discovery under docs/ is a walk, so a page that lands is governed on
// arrival rather than when someone remembers to extend a list.
func governedDocs(root string) ([]string, error) {
	docs := []string{"README.md", "adapters/README.md"}
	for _, d := range docs {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); err != nil {
			return nil, err
		}
	}

	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if notGoverned[rel] {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(rel, ".md") {
			docs = append(docs, rel)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return docs, nil
}

// scanProse returns every selector-shaped identifier written in an inline
// code span outside a fenced block. An unclosed fence is an error rather
// than a silent skip: it would hide the rest of the file from the check.
func scanProse(root, doc string) ([]ref, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(doc)))
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	var (
		refs    []ref
		inFence bool
		lineNum int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}

		if inFence {
			continue
		}

		// Splitting on the backtick puts span contents at odd indices; a
		// final odd segment has no closing backtick and is not a span.
		segs := strings.Split(line, "`")
		for i := 1; i+1 < len(segs); i += 2 {
			raw := strings.TrimSpace(segs[i])
			text := strings.TrimSuffix(raw, "()")
			if !selectorShaped(text) {
				continue
			}

			refs = append(refs, ref{doc: doc, line: lineNum, text: text, raw: raw})
		}
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	if inFence {
		return nil, fmt.Errorf("%s: fence opened and never closed", doc)
	}

	return refs, nil
}

// harnessPrefix is the module tree loadIndex skips. test/ holds the
// checkers themselves, which export nothing the documentation describes.
const harnessPrefix = "test/"

// moduleDirs returns the repo-relative directory of every module, using
// the discovery scripts/ci-go.sh uses, so the set of packages a doc may
// cite cannot drift from the set CI builds.
func moduleDirs(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "go.mod", "*/go.mod").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}

		dir := filepath.ToSlash(filepath.Dir(line))
		if strings.HasPrefix(dir, harnessPrefix) {
			continue
		}

		dirs = append(dirs, dir)
	}

	if len(dirs) == 0 {
		return nil, errors.New("no go.mod found — refusing to resolve against nothing")
	}

	return dirs, nil
}

// symbolIndex is every package of the checked modules, grouped by the
// short package name a doc would write. Two modules may declare the same
// name (query/sql and database/sql-shaped adapters), so the value is a
// list and a reference resolves if any candidate accepts it.
type symbolIndex struct {
	byName map[string][]*types.Package
}

// loadIndex type-checks every module outside test/ and indexes the result.
// A package that fails to load is an error rather than an omission: an
// omitted package would turn every symbol it exports into a doc failure.
func loadIndex(root string) (*symbolIndex, error) {
	dirs, err := moduleDirs(root)
	if err != nil {
		return nil, err
	}

	ix := &symbolIndex{byName: map[string][]*types.Package{}}
	seen := map[string]bool{}
	for _, dir := range dirs {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedTypes |
				packages.NeedImports | packages.NeedDeps,
			Dir: filepath.Join(root, filepath.FromSlash(dir)),
		}
		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", dir, err)
		}

		for _, p := range pkgs {
			if len(p.Errors) > 0 {
				return nil, fmt.Errorf("loading %s: %s: %v", dir, p.PkgPath, p.Errors[0])
			}

			if p.Types == nil || seen[p.PkgPath] {
				continue
			}

			seen[p.PkgPath] = true
			name := p.Types.Name()
			ix.byName[name] = append(ix.byName[name], p.Types)
		}
	}

	if len(seen) == 0 {
		return nil, errors.New("no packages loaded — refusing to pass vacuously")
	}

	return ix, nil
}

// resolve reports whether one selector names something real, and says what
// was wrong when it does not.
func (ix *symbolIndex) resolve(text string) error {
	parts := strings.Split(text, ".")
	if bareMemberRE.MatchString(text) {
		return ix.resolveBareMember(parts[0], parts[1])
	}

	if t, ok := receiverTypes[parts[0]]; ok {
		parts = append(strings.Split(t, "."), parts[1:]...)
	}

	if len(parts) > 3 {
		return fmt.Errorf("more than two members deep; only pkg.Symbol and pkg.Type.Member resolve")
	}

	pkgs := ix.byName[parts[0]]
	if len(pkgs) == 0 {
		return fmt.Errorf("no package named %q in this repository", parts[0])
	}

	var reasons []string
	for _, p := range pkgs {
		err := lookup(p, parts[1:])
		if err == nil {
			return nil
		}

		reasons = append(reasons, err.Error())
	}

	return errors.New(strings.Join(reasons, "; "))
}

// lookup walks one package's scope down the member path. The type member
// lookup is addressable, so a doc naming T.M is satisfied by a method on
// *T as well as on T, and by a promoted field or method of an embedded
// type.
func lookup(p *types.Package, path []string) error {
	obj := p.Scope().Lookup(path[0])
	if obj == nil || !obj.Exported() {
		if len(path) == 1 && methodOnSomeType(p, path[0]) {
			return nil
		}

		return fmt.Errorf("package %s exports no %s, and no exported type in it "+
			"has a method %s", p.Path(), path[0], path[0])
	}

	if len(path) == 1 {
		return nil
	}

	name, ok := obj.(*types.TypeName)
	if !ok {
		return fmt.Errorf("%s.%s is not a type, so it has no member %s", p.Path(), path[0], path[1])
	}

	member, _, _ := types.LookupFieldOrMethod(name.Type(), true, p, path[1])
	if member == nil || !member.Exported() {
		return fmt.Errorf("%s.%s has no exported field or method %s", p.Path(), path[0], path[1])
	}

	return nil
}

// checkRefs resolves every reference that is not allowlisted and returns
// one failure line per unresolved one, in the order encountered. Each line
// is file:line followed by the identifier exactly as the doc writes it, so
// an editor jumps straight to the sentence that has to change.
func checkRefs(ix *symbolIndex, refs []ref) []string {
	var out []string
	for _, r := range refs {
		if _, ok := allowSelectors[r.text]; ok {
			continue
		}

		if err := ix.resolve(r.text); err != nil {
			out = append(out, fmt.Sprintf("%s:%d: `%s` does not resolve: %v", r.doc, r.line, r.raw, err))
		}
	}

	return out
}

// methodOnSomeType reports whether any exported type of p has an exported
// method called name. The guides write a method package-qualified —
// `emit.Record` for (*emit.Std).Record — which is how `go doc emit.Record`
// resolves it and which this repository's review has judged correct style
// (docs-accuracy, README.md:204, 2026-08-30). Resolution follows go doc
// rather than forcing the receiver into every sentence; the reference is
// still checked, so deleting or renaming the method still fails.
func methodOnSomeType(p *types.Package, name string) bool {
	scope := p.Scope()
	for _, n := range scope.Names() {
		tn, ok := scope.Lookup(n).(*types.TypeName)
		if !ok || !tn.Exported() {
			continue
		}

		obj, _, _ := types.LookupFieldOrMethod(tn.Type(), true, p, name)
		if fn, ok := obj.(*types.Func); ok && fn.Exported() {
			return true
		}
	}

	return false
}

// resolveBareMember resolves a type member written without its package —
// the money path's `Leg.Count`. Every indexed package is searched for an
// exported type of that name; the reference resolves if any of them has
// the member. Package-free is how the architecture pages write a field
// they have already named the owning package for a paragraph earlier, and
// it is the shape one of the defects this checker exists for took.
func (ix *symbolIndex) resolveBareMember(typeName, member string) error {
	found := false
	for _, pkgs := range ix.byName {
		for _, p := range pkgs {
			obj, ok := p.Scope().Lookup(typeName).(*types.TypeName)
			if !ok || !obj.Exported() {
				continue
			}

			found = true
			m, _, _ := types.LookupFieldOrMethod(obj.Type(), true, p, member)
			if m != nil && m.Exported() {
				return nil
			}
		}
	}

	if !found {
		return fmt.Errorf("no exported type named %s in this repository", typeName)
	}

	return fmt.Errorf("no exported type named %s in this repository has a "+
		"field or method %s", typeName, member)
}
