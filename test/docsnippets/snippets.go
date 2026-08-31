// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package docsnippets is the deterministic slice of the docs-accuracy
// review lens: every fenced Go block in the guide docs must compile
// against the real modules, and every fenced registry example must pass
// registry.Load. It lives in its own module so scripts/ci-go.sh's
// dynamic module discovery binds it into the core CI job with no gate
// wiring; the judged slice of the lens (claims vs binary) stays with the
// reviewers.
package docsnippets

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// fence is one fenced code block, with enough position to name in a failure.
type fence struct {
	doc  string // repo-relative path
	line int    // 1-based line of the opening fence
	lang string // info string: "go", "yaml", ...
	body string
	ref  bool // reference material (see refMarker), exempt from compiling
	cont bool // continuation (see contMarker): wrapped, unused decls absorbed
}

// The two governance markers. Each must be the whole line directly
// above a fence's opening — deliberately narrow, so a fence cannot
// drift out of governance by accident.
//
// refMarker exempts a fence from the compile check entirely: it is
// reference material — an API-signature listing, not compilable code.
//
// contMarker declares a continuation fence: a wiring-guide step whose
// declarations the prose or a later fence picks up. It always compiles
// wrapped in a func, and only for such fences the compiler's "declared
// and not used" diagnostics are absorbed (see compileGoFences); every
// other diagnostic, and unused declarations in UNMARKED fences, still
// fail loudly.
const (
	refMarker  = "<!-- docsnippets:reference -->"
	contMarker = "<!-- docsnippets:continues -->"
)

// extractFences scans a markdown file for fenced blocks. Indented fences
// (inside list items) are captured with their indentation stripped.
func extractFences(path string) ([]fence, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		fences     []fence
		cur        *fence
		indent     string
		lineNum    int
		prevMarker string
	)
	open := regexp.MustCompile("^([ \t]*)```([a-zA-Z]*)[^`]*$")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if cur == nil {
			if m := open.FindStringSubmatch(line); m != nil {
				cur = &fence{doc: path, line: lineNum, lang: m[2],
					ref: prevMarker == refMarker, cont: prevMarker == contMarker}
				indent = m[1]
			}
			prevMarker = strings.TrimSpace(line)
			continue
		}
		if strings.TrimSpace(line) == "```" {
			fences = append(fences, *cur)
			cur = nil
			prevMarker = "" // a closing fence line is never a marker
			continue
		}
		cur.body += strings.TrimPrefix(line, indent) + "\n"
	}
	if cur != nil {
		// Fail closed: a fence that loses its closing marker must not
		// silently exit governance.
		return nil, fmt.Errorf("%s:%d: fence opened and never closed", cur.doc, cur.line)
	}
	return fences, sc.Err()
}

// fileLevel reports whether a Go fence is a set of top-level declarations
// (compiled as a file) rather than statements (compiled wrapped in a func).
func fileLevel(body string) bool {
	decl := regexp.MustCompile(`^(func|var|type|const|import|package)\b`)
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		return decl.MatchString(l)
	}
	return false
}

// importsFor maps identifiers a fence uses to the import paths that
// provide them. The map is the checker's one maintenance surface: a doc
// that starts using a new package extends it (a miss fails the compile
// loudly, naming the identifier).
var importsFor = map[string]string{
	"biz":        "github.com/NightWatchEng/shortfall/biz",
	"emit":       "github.com/NightWatchEng/shortfall/emit",
	"registry":   "github.com/NightWatchEng/shortfall/registry",
	"engine":     "github.com/NightWatchEng/shortfall/engine",
	"httpmw":     "github.com/NightWatchEng/shortfall/propagate/httpmw",
	"cloudwatch": "github.com/NightWatchEng/shortfall/adapters/export/cloudwatch",
	"gcp":        "github.com/NightWatchEng/shortfall/adapters/export/gcp",
	"otlp":       "github.com/NightWatchEng/shortfall/adapters/export/otlp",
	"promexport": "github.com/NightWatchEng/shortfall/adapters/export/prometheus",
	"cwinsights": "github.com/NightWatchEng/shortfall/adapters/query/cwinsights",
	"gcplogging": "github.com/NightWatchEng/shortfall/adapters/query/gcplogging",
	"promql":     "github.com/NightWatchEng/shortfall/adapters/query/promql",
	"sqlq":       "github.com/NightWatchEng/shortfall/adapters/query/sql",
	"promhttp":   "github.com/prometheus/client_golang/prometheus/promhttp",
	"query":      "github.com/NightWatchEng/shortfall/query",
	"sql":        "database/sql",
	"http":       "net/http",
	"context":    "context",
	"fmt":        "fmt",
	"io":         "io",
	"log":        "log",
	"time":       "time",
}

// stripPackageClause removes a fence's own `package X` line. A quickstart
// shows a complete, copy-pasteable program, and the synthesized file
// supplies its own package clause — two would not compile, which would
// push exactly the fence a reader is most likely to run out of governance.
func stripPackageClause(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		if strings.HasPrefix(t, "package ") {
			return strings.Join(lines[i+1:], "\n")
		}
		break
	}
	return body
}

// splitLeadingImports separates a fence's own leading import block from
// the rest of its body. Docs that teach wiring open a fence with the
// interesting imports and continue with statements ("mixed" fences);
// compiled verbatim those statements would sit at file level. The specs
// come back verbatim (aliases kept), rest is the body without the block.
// A fence with no leading import block returns (nil, body).
func splitLeadingImports(body string) (specs []string, rest string) {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "//") {
			i++
			continue
		}
		break
	}
	if i >= len(lines) {
		return nil, body
	}
	switch t := strings.TrimSpace(lines[i]); {
	case t == "import (":
		for j := i + 1; j < len(lines); j++ {
			s := strings.TrimSpace(lines[j])
			if s == ")" {
				return specs, strings.Join(lines[j+1:], "\n")
			}
			if s != "" {
				specs = append(specs, s)
			}
		}
		return nil, body // unclosed block: leave it to the compiler to name
	case strings.HasPrefix(t, "import "):
		return []string{strings.TrimPrefix(t, "import ")}, strings.Join(lines[i+1:], "\n")
	}
	return nil, body
}

// importSpecPath extracts the quoted path from an import spec line
// (`"net/http"`, `prom "example.com/x"`).
func importSpecPath(spec string) string {
	if i := strings.IndexByte(spec, '"'); i >= 0 {
		if j := strings.IndexByte(spec[i+1:], '"'); j >= 0 {
			return spec[i+1 : i+1+j]
		}
	}
	return ""
}

// synthesize renders one fence as a compilable Go file in package
// docsnippet: the fence's own leading import block (if any) merged with
// a generated one for the identifiers the fence uses, then either the
// remainder verbatim (file-level) or wrapped in a uniquely named
// function (statements). discard names identifiers the wrapped form
// must blank-assign: a tutorial fence legitimately declares things the
// prose, not the fence, goes on to use, and only the compiler can say
// which (see compileGoFences).
func synthesize(f fence, n int, discard []string) string {
	var b strings.Builder
	b.WriteString("package docsnippet\n\n")

	ownSpecs, body := splitLeadingImports(stripPackageClause(f.body))
	f.body = body

	used := map[string]bool{}
	for _, s := range ownSpecs {
		if p := importSpecPath(s); p != "" {
			used[p] = true // the fence's own spec wins; don't auto-add it again
		}
	}
	if len(ownSpecs) > 0 {
		b.WriteString("import (\n")
		for _, s := range ownSpecs {
			b.WriteString("\t" + s + "\n")
		}
		b.WriteString(")\n\n")
	}

	// Scan with line comments stripped: a comment naming a package
	// (`// emit.New(*registry.Registry, ...)`) must not import it.
	lineComment := regexp.MustCompile(`(?m)//.*$`)
	scanned := lineComment.ReplaceAllString(f.body, "")
	ident := regexp.MustCompile(`(?m)(?:^|[^\w.])([a-z]\w*)\.`)
	for _, m := range ident.FindAllStringSubmatch(scanned, -1) {
		if path, ok := importsFor[m[1]]; ok && !used[path] {
			used[path] = true
		}
	}
	for _, s := range ownSpecs {
		if p := importSpecPath(s); p != "" {
			delete(used, p) // already emitted above; keep it out of the generated block
		}
	}
	if len(used) > 0 {
		b.WriteString("import (\n")
		paths := make([]string, 0, len(used))
		for p := range used {
			paths = append(paths, p)
		}
		// deterministic order keeps failures reproducible
		for i := 0; i < len(paths); i++ {
			for j := i + 1; j < len(paths); j++ {
				if paths[j] < paths[i] {
					paths[i], paths[j] = paths[j], paths[i]
				}
			}
		}
		for _, p := range paths {
			alias := ""
			switch p {
			case "github.com/NightWatchEng/shortfall/adapters/export/prometheus":
				alias = "promexport "
			case "github.com/NightWatchEng/shortfall/adapters/query/sql":
				alias = "sqlq "
			}
			fmt.Fprintf(&b, "\t%s%q\n", alias, p)
		}
		b.WriteString(")\n\n")
	}

	if fileLevel(f.body) && !f.cont {
		b.WriteString(f.body)
	} else {
		fmt.Fprintf(&b, "func _snippet%d() {\n", n)
		for _, l := range strings.Split(strings.TrimRight(f.body, "\n"), "\n") {
			b.WriteString("\t" + l + "\n")
		}
		for _, id := range discard {
			fmt.Fprintf(&b, "\t_ = %s // declared for the reader; the prose picks it up\n", id)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// tempModTemplate is the synthesized module every doc's fences compile
// in. ROOT is replaced with the absolute repo root at run time.
const tempModTemplate = `module docsnippet

go 1.25.0

require (
	github.com/NightWatchEng/shortfall v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/cloudwatch v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/gcp v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/otlp v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/prometheus v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/cwinsights v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/gcplogging v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/promql v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/sql v0.0.0
)

replace github.com/NightWatchEng/shortfall => ROOT

replace github.com/NightWatchEng/shortfall/adapters/export/cloudwatch => ROOT/adapters/export/cloudwatch

replace github.com/NightWatchEng/shortfall/adapters/export/gcp => ROOT/adapters/export/gcp

replace github.com/NightWatchEng/shortfall/adapters/export/otlp => ROOT/adapters/export/otlp

replace github.com/NightWatchEng/shortfall/adapters/export/prometheus => ROOT/adapters/export/prometheus

replace github.com/NightWatchEng/shortfall/adapters/query/cwinsights => ROOT/adapters/query/cwinsights

replace github.com/NightWatchEng/shortfall/adapters/query/gcplogging => ROOT/adapters/query/gcplogging

replace github.com/NightWatchEng/shortfall/adapters/query/promql => ROOT/adapters/query/promql

replace github.com/NightWatchEng/shortfall/adapters/query/sql => ROOT/adapters/query/sql
`

// unusedIdent matches the compiler's declared-and-not-used diagnostic
// for a synthesized fence file, capturing the fence index and the name.
var unusedIdent = regexp.MustCompile(`(?m)^\./fence(\d+)_line\d+\.go:\d+:\d+: declared and not used: (\w+)$`)

// compileGoFences synthesizes every Go fence of one doc into a temp
// module (with the doc's stub file, if any) and builds it. The returned
// error carries the compiler output, which names fences by file.
//
// One diagnostic class is absorbed rather than reported, and only for
// fences carrying contMarker: declared and not used. A wiring guide's
// fence declares `reg, err := registry.Load(...)` and the NEXT fence
// (or the prose) uses reg; inside the synthesized func that is a
// compile error, but in the reader's program it is not. The compiler
// itself names the identifiers, they are blank-assigned, and the build
// reruns — so an unused variable never masks any other diagnostic, an
// UNMARKED fence's unused declarations still fail (the broken.md
// fixture pins that), and everything else fails loudly everywhere.
func compileGoFences(root, tmp string, fences []fence, stubs string) error {
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte(strings.ReplaceAll(tempModTemplate, "ROOT", root)), 0o644); err != nil {
		return err
	}
	if stubs != "" {
		if err := os.WriteFile(filepath.Join(tmp, "stubs.go"), []byte(stubs), 0o644); err != nil {
			return err
		}
	}
	discards := make([][]string, len(fences))
	var lastErr error
	// One retry per fence with unused idents would do; 4 rounds bounds
	// even a pathological cascade without looping forever.
	for attempt := 0; attempt < 4; attempt++ {
		for i, f := range fences {
			name := fmt.Sprintf("fence%02d_line%d.go", i, f.line)
			if err := os.WriteFile(filepath.Join(tmp, name), []byte(synthesize(f, i, discards[i])), 0o644); err != nil {
				return err
			}
		}
		lastErr = nil
		for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
			cmd := exec.Command("go", args...)
			cmd.Dir = tmp
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
			if out, err := cmd.CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, out)
				break
			}
		}
		if lastErr == nil {
			return nil
		}
		grew := false
		for _, m := range unusedIdent.FindAllStringSubmatch(lastErr.Error(), -1) {
			i, err := strconv.Atoi(m[1])
			if err == nil && i < len(discards) && fences[i].cont && !slicesContains(discards[i], m[2]) {
				discards[i] = append(discards[i], m[2])
				grew = true
			}
		}
		if !grew {
			return lastErr
		}
	}
	return lastErr
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// registryFence reports whether a yaml fence is a complete registry
// example (has the required top-level keys and no elision placeholder).
func registryFence(f fence) bool {
	return f.lang == "yaml" &&
		strings.Contains(f.body, "version:") &&
		strings.Contains(f.body, "flows:") &&
		!strings.Contains(f.body, "...")
}
