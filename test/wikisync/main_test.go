// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageName(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"docs/quickstart.md", "quickstart"},
		{"docs/adr/0002-outcome-event-transport.md", "adr-0002-outcome-event-transport"},
		{"docs/architecture/c4-l2-containers.md", "architecture-c4-l2-containers"},
		{"docs/adr/README.md", "adr-README"},
		{"README.md", "README"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := pageName(c.path); got != c.want {
				t.Errorf("pageName(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestDocPagesRejectsColliding(t *testing.T) {
	root := writeRepo(t)
	// docs/adr-0004-metric-label-set.md flattens to the same page name as
	// docs/adr/0004-metric-label-set.md; the generator must refuse rather
	// than let one silently overwrite the other.
	clash := filepath.Join(root, "docs", "adr-0004-metric-label-set.md")
	if err := os.WriteFile(clash, []byte("impostor"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := docPages(root); err == nil {
		t.Fatal("docPages accepted two sources mapping to one wiki page")
	} else if !strings.Contains(err.Error(), "adr-0004-metric-label-set") {
		t.Errorf("collision error does not name the page: %v", err)
	}
}

// writeRepo lays out a minimal repo tree for link-resolution tests.
func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"README.md":          "see [adapters](docs/adapters.md)",
		"docs/quickstart.md": "read [the transport ADR](adr/0002-outcome-event-transport.md) and [money](money.md#units)",
		"docs/money.md":      "plain page",
		"docs/adapters.md":   "code at [exporters](../adapters/export) and [vector](../testkit/vectors/outcome-event.json); web [semconv](https://opentelemetry.io/)",
		"docs/adr/0002-outcome-event-transport.md": "see [labels](0004-metric-label-set.md) and [money](../money.md)",
		"docs/adr/0004-metric-label-set.md":        "history",
		// The ADR index is what makes an ADR page reachable; navFor
		// checks each ADR against it rather than exempting the prefix.
		"docs/adr/README.md":                 "[0002](0002-outcome-event-transport.md) [0004](0004-metric-label-set.md)",
		"adapters/export/README.md":          "not docs",
		"testkit/vectors/outcome-event.json": "{}",
	}
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRewriteLinks(t *testing.T) {
	root := writeRepo(t)
	pages := collectPages(t, root)
	cases := []struct {
		src, in, want string
	}{
		// sibling .md in the same directory becomes a page link
		{"docs/adr/0002-outcome-event-transport.md", "[labels](0004-metric-label-set.md)", "[labels](adr-0004-metric-label-set)"},
		// parent-relative .md keeps its anchor
		{"docs/quickstart.md", "[money](money.md#units)", "[money](money#units)"},
		// subdirectory .md from docs root
		{"docs/quickstart.md", "[t](adr/0002-outcome-event-transport.md)", "[t](adr-0002-outcome-event-transport)"},
		// README links into docs/
		{"README.md", "[adapters](docs/adapters.md)", "[adapters](adapters)"},
		// non-markdown directory target becomes a tree URL
		{"docs/adapters.md", "[exporters](../adapters/export)", "[exporters](" + repoURL + "/tree/main/adapters/export)"},
		// non-markdown file target becomes a blob URL
		{"docs/adapters.md", "[v](../testkit/vectors/outcome-event.json)", "[v](" + repoURL + "/blob/main/testkit/vectors/outcome-event.json)"},
		// absolute URLs pass through
		{"docs/adapters.md", "[w](https://opentelemetry.io/)", "[w](https://opentelemetry.io/)"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := rewriteLine(root, c.src, pages, c.in); got != c.want {
				t.Errorf("rewriteLine(%s, %q)\n got %q\nwant %q", c.src, c.in, got, c.want)
			}
		})
	}
}

func TestRewriteSkipsInlineCodeSpans(t *testing.T) {
	root := writeRepo(t)
	pages := collectPages(t, root)
	in := "write `[label](money.md)` to link [money](money.md)"
	want := "write `[label](money.md)` to link [money](money)"
	if got := rewriteLine(root, "docs/quickstart.md", pages, in); got != want {
		t.Errorf("inline code span rewritten:\n got %q\nwant %q", got, want)
	}
}

func TestRewriteSkipsCodeFences(t *testing.T) {
	root := writeRepo(t)
	pages := collectPages(t, root)
	in := "before [m](money.md)\n```go\n// [m](money.md) stays verbatim\n```\nafter [m](money.md)"
	got := rewriteDoc(root, "docs/quickstart.md", pages, in)
	if !strings.Contains(got, "// [m](money.md) stays verbatim") {
		t.Errorf("fenced content was rewritten:\n%s", got)
	}
	if strings.Count(got, "](money)") != 2 {
		t.Errorf("prose links not rewritten twice:\n%s", got)
	}
}

func TestGenerateEmitsEveryPagePlusHomeAndSidebar(t *testing.T) {
	root := writeRepo(t)
	out := t.TempDir()
	if err := generate(root, out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Home.md", "_Sidebar.md", "README.md",
		"quickstart.md", "money.md", "adapters.md",
		"adr-0002-outcome-event-transport.md", "adr-0004-metric-label-set.md",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("missing generated page %s", want)
		}
	}
	body, err := os.ReadFile(filepath.Join(out, "quickstart.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "docs/quickstart.md") || !strings.Contains(string(body), "source of truth") {
		t.Errorf("mirrored page lacks source-of-truth footer:\n%s", body)
	}
	home, err := os.ReadFile(filepath.Join(out, "Home.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), "[Quickstart](quickstart)") {
		t.Errorf("Home navigation misses quickstart:\n%s", home)
	}
	if !strings.Contains(string(home), "### Start here") {
		t.Errorf("Home navigation lost its curated sections:\n%s", home)
	}
}

func TestNavRefusesAnUnreachablePage(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, root string)
		wantNamed string
	}{
		// A guide no section lists is mirrored but reachable only by URL.
		{"guide missing from the curated sections", func(t *testing.T, root string) {
			writeDoc(t, root, "docs/brand-new-guide.md", "unlisted")
		}, "brand-new-guide"},
		// An ADR is exempt from the sections because the ADR index lists
		// it, so one the index omits is just as unreachable.
		{"ADR missing from the ADR index", func(t *testing.T, root string) {
			writeDoc(t, root, "docs/adr/0099-unlisted-decision.md", "history")
		}, "adr-0099-unlisted-decision"},
		// Markdown does not render a link inside a fence, so neither may
		// the reachability check count one.
		{"ADR linked only from a fenced example", func(t *testing.T, root string) {
			writeDoc(t, root, "docs/adr/0099-unlisted-decision.md", "history")
			writeDoc(t, root, "docs/adr/README.md",
				"[0002](0002-outcome-event-transport.md) [0004](0004-metric-label-set.md)\n"+
					"```\n[0099](0099-unlisted-decision.md)\n```\n")
		}, "adr-0099-unlisted-decision"},
		// A code span is not a link either, and the span branch is a
		// separate code path from the fence branch above.
		{"ADR linked only from an inline code span", func(t *testing.T, root string) {
			writeDoc(t, root, "docs/adr/0099-unlisted-decision.md", "history")
			writeDoc(t, root, "docs/adr/README.md",
				"[0002](0002-outcome-event-transport.md) [0004](0004-metric-label-set.md)\n"+
					"spell it `[0099](0099-unlisted-decision.md)` to show the shape\n")
		}, "adr-0099-unlisted-decision"},
		// The index carries the adr- prefix but is reached the ordinary
		// way; exempting it would let one deleted nav line orphan every ADR.
		{"ADR index itself missing from the curated sections", func(t *testing.T, root string) {
			withoutNavEntry(t, "adr-README")
		}, "adr-README"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeRepo(t)
			c.setup(t, root)
			err := generate(root, t.TempDir())
			if err == nil {
				t.Fatal("generate accepted a page nothing links to")
			}
			if !strings.Contains(err.Error(), c.wantNamed) {
				t.Errorf("error does not name the unreachable page %q: %v", c.wantNamed, err)
			}
		})
	}
}

// writeDoc creates one file under root, making its directory as needed.
func writeDoc(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withoutNavEntry drops one curated entry for the duration of a test, so a
// case can assert what happens to a page no section names.
func withoutNavEntry(t *testing.T, page string) {
	t.Helper()
	saved := sections
	t.Cleanup(func() { sections = saved })
	trimmed := make([]section, 0, len(saved))
	for _, s := range saved {
		kept := make([]navEntry, 0, len(s.entries))
		for _, e := range s.entries {
			if e.page != page {
				kept = append(kept, e)
			}
		}
		trimmed = append(trimmed, section{title: s.title, entries: kept})
	}
	sections = trimmed
}

func TestDocsInternalIsNotMirrored(t *testing.T) {
	root := writeRepo(t)
	// An internal record must not reach the wiki at all — not as an orphan
	// the nav check would catch, and not as a published page either.
	writeDoc(t, root, "docs/internal/go-public-checklist.md", "founder bookkeeping")
	out := t.TempDir()
	if err := generate(root, out); err != nil {
		t.Fatalf("generate rejected a tree with docs/internal: %v", err)
	}
	for _, name := range []string{"internal-go-public-checklist.md", "go-public-checklist.md"} {
		if _, err := os.Stat(filepath.Join(out, name)); err == nil {
			t.Errorf("docs/internal was mirrored as %s", name)
		}
	}
	nav, err := os.ReadFile(filepath.Join(out, "_Sidebar.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nav), "go-public-checklist") {
		t.Errorf("internal record reached the sidebar:\n%s", nav)
	}
}

// TestThisRepoNavigationCoversEveryPage runs the generator over this
// repository rather than a fixture. Without it the cases above only ever
// judge a synthetic tree, and a doc added under docs/ with no nav entry
// would pass every pre-merge check and fail the wiki-sync workflow after
// merge. scripts/ci-go.sh discovers this module, so it runs in the required
// core checks job.
func TestThisRepoNavigationCoversEveryPage(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := docPages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("docPages found no docs in this repo — the check would be vacuous")
	}
	if _, err := navFor(root, pages); err != nil {
		t.Errorf("this repository has %v", err)
	}
}

func collectPages(t *testing.T, root string) map[string]string {
	t.Helper()
	pages, err := docPages(root)
	if err != nil {
		t.Fatal(err)
	}
	return pages
}
