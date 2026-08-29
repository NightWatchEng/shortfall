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
	"strings"
)

// fence is one fenced code block, with enough position to name in a failure.
type fence struct {
	doc  string // repo-relative path
	line int    // 1-based line of the opening fence
	lang string // info string: "go", "yaml", ...
	body string
}

// extractFences scans a markdown file for fenced blocks. Indented fences
// (inside list items) are captured with their indentation stripped.
func extractFences(path string) ([]fence, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		fences  []fence
		cur     *fence
		indent  string
		lineNum int
	)
	open := regexp.MustCompile("^([ \t]*)```([a-zA-Z]*)[^`]*$")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if cur == nil {
			if m := open.FindStringSubmatch(line); m != nil {
				cur = &fence{doc: path, line: lineNum, lang: m[2]}
				indent = m[1]
			}
			continue
		}
		if strings.TrimSpace(line) == "```" {
			fences = append(fences, *cur)
			cur = nil
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
	"gcplogging": "github.com/NightWatchEng/shortfall/adapters/query/gcplogging",
	"promql":     "github.com/NightWatchEng/shortfall/adapters/query/promql",
	"sqlq":       "github.com/NightWatchEng/shortfall/adapters/query/sql",
	"promhttp":   "github.com/prometheus/client_golang/prometheus/promhttp",
	"sql":        "database/sql",
	"http":       "net/http",
	"context":    "context",
	"fmt":        "fmt",
	"io":         "io",
	"log":        "log",
	"time":       "time",
}

// synthesize renders one fence as a compilable Go file in package
// docsnippet: a generated import block for the identifiers the fence
// uses, then either the fence verbatim (file-level) or the fence wrapped
// in a uniquely named function (statements).
func synthesize(f fence, n int) string {
	var b strings.Builder
	b.WriteString("package docsnippet\n\n")

	used := map[string]bool{}
	ident := regexp.MustCompile(`(?m)(?:^|[^\w.])([a-z]\w*)\.`)
	for _, m := range ident.FindAllStringSubmatch(f.body, -1) {
		if path, ok := importsFor[m[1]]; ok && !used[path] {
			used[path] = true
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

	if fileLevel(f.body) {
		b.WriteString(f.body)
	} else {
		fmt.Fprintf(&b, "func _snippet%d() {\n", n)
		for _, l := range strings.Split(strings.TrimRight(f.body, "\n"), "\n") {
			b.WriteString("\t" + l + "\n")
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
	github.com/NightWatchEng/shortfall/adapters/query/gcplogging v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/promql v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/sql v0.0.0
)

replace github.com/NightWatchEng/shortfall => ROOT

replace github.com/NightWatchEng/shortfall/adapters/export/cloudwatch => ROOT/adapters/export/cloudwatch

replace github.com/NightWatchEng/shortfall/adapters/export/gcp => ROOT/adapters/export/gcp

replace github.com/NightWatchEng/shortfall/adapters/export/otlp => ROOT/adapters/export/otlp

replace github.com/NightWatchEng/shortfall/adapters/export/prometheus => ROOT/adapters/export/prometheus

replace github.com/NightWatchEng/shortfall/adapters/query/gcplogging => ROOT/adapters/query/gcplogging

replace github.com/NightWatchEng/shortfall/adapters/query/promql => ROOT/adapters/query/promql

replace github.com/NightWatchEng/shortfall/adapters/query/sql => ROOT/adapters/query/sql
`

// compileGoFences synthesizes every Go fence of one doc into a temp
// module (with the doc's stub file, if any) and builds it. The returned
// error carries the compiler output, which names fences by file.
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
	for i, f := range fences {
		name := fmt.Sprintf("fence%02d_line%d.go", i, f.line)
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(synthesize(f, i)), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// registryFence reports whether a yaml fence is a complete registry
// example (has the required top-level keys and no elision placeholder).
func registryFence(f fence) bool {
	return f.lang == "yaml" &&
		strings.Contains(f.body, "version:") &&
		strings.Contains(f.body, "flows:") &&
		!strings.Contains(f.body, "...")
}
