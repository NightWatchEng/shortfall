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
		"adapters/export/README.md":                "not docs",
		"testkit/vectors/outcome-event.json":       "{}",
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

func TestNavRefusesAnUncuratedPage(t *testing.T) {
	root := writeRepo(t)
	// A new doc that no section lists would be mirrored but unreachable —
	// the generator must name it rather than publish an orphan page.
	orphan := filepath.Join(root, "docs", "brand-new-guide.md")
	if err := os.WriteFile(orphan, []byte("unlisted"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := generate(root, t.TempDir())
	if err == nil {
		t.Fatal("generate accepted a page missing from the curated navigation")
	}
	if !strings.Contains(err.Error(), "brand-new-guide") {
		t.Errorf("error does not name the uncurated page: %v", err)
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
