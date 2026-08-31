// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package modgraph

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const repoRoot = "../.."

// firstPartyRequireFloor guards against a scan that examined far less than
// the tree holds. Every check below is a loop over parsed edges, so a parser
// that understood nothing, or a checkout exposing a fraction of the modules,
// would otherwise run zero iterations and report PASS — the difference
// between "these invariants hold" and "nothing was examined", which for a
// checker written against a bug class defined by "nothing went red" is the
// whole point.
//
// The tree holds 23 first-party requires across 24 go.mod files as of this
// commit; the floor is 15. The margin is deliberate — this must not go red
// because an adapter module was legitimately removed — and it is calibrated
// against the failure it is really for: 12 of the 23 arrive via the
// parenthesised require block and 11 via the single-line form, so a parser
// that lost EITHER form drops to 11 or 12 and trips this. Raise it
// deliberately, with a recount, rather than nudging it to keep a red run
// quiet.
const firstPartyRequireFloor = 15

func TestParseModReadsEveryDirectiveFormInThisRepo(t *testing.T) {
	sql := ModulePrefix + "/adapters/query/sql"
	cases := []struct {
		name     string
		content  string
		requires map[string]string
		replaces map[string]string
		unparsed int
	}{
		{
			name: "parenthesised require block",
			content: "module x\n\nrequire (\n\t" + ModulePrefix + " v0.2.0\n\t" +
				sql + " v0.2.0\n\tmodernc.org/sqlite v1.57.0\n)\n",
			requires: map[string]string{ModulePrefix: "v0.2.0", sql: "v0.2.0"},
			replaces: map[string]string{},
		},
		{
			name:     "single-line require",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{},
		},
		{
			name:     "single-line replace",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0\n\nreplace " + ModulePrefix + " => ../..\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{ModulePrefix: "../.."},
		},
		{
			// test/loggolden's real shape.
			name: "parenthesised replace block",
			content: "module x\n\nrequire (\n\t" + ModulePrefix + " v0.2.0\n)\n\n" +
				"replace (\n\t" + ModulePrefix + " => ../..\n\t" + sql + " => ../../adapters/query/sql\n)\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{ModulePrefix: "../..", sql: "../../adapters/query/sql"},
		},
		{
			// What `go mod edit -replace=path@version=target` writes.
			name:     "replace with a version on the left",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0\n\nreplace " + ModulePrefix + " v0.2.0 => ../..\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{ModulePrefix: "../.."},
		},
		{
			// The target's PATH is kept and its version dropped, which is all
			// IsLocalTarget needs. Named for what it actually asserts.
			name:     "replace onto another module keeps the target path, not its version",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0\n\nreplace " + ModulePrefix + " => example.com/fork v1.2.3\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{ModulePrefix: "example.com/fork"},
		},
		{
			// The form that defeated the first cut: quoting made a first-party
			// path read as third-party, and the require vanished from every check.
			name:     "quoted module path",
			content:  "module x\n\nrequire \"" + sql + "\" v0.0.0\n",
			requires: map[string]string{sql: "v0.0.0"},
			replaces: map[string]string{},
		},
		{
			name:     "third-party requires are ignored, not unparsed",
			content:  "module x\n\nrequire (\n\tmodernc.org/sqlite v1.57.0\n\tgolang.org/x/mod v0.38.0\n)\n",
			requires: map[string]string{},
			replaces: map[string]string{},
		},
		{
			name:     "a trailing comment does not become the version",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0 // pinned\n",
			requires: map[string]string{ModulePrefix: "v0.2.0"},
			replaces: map[string]string{},
		},
		{
			name:     "a lookalike path outside this module is not first-party",
			content:  "module x\n\nrequire " + ModulePrefix + "-extras v1.0.0\n",
			requires: map[string]string{},
			replaces: map[string]string{},
		},
		{
			name:     "exclude and retract blocks are read and ignored",
			content:  "module x\n\nexclude (\n\t" + ModulePrefix + " v0.1.0\n)\n\nretract (\n\tv0.0.1\n)\n\ngo 1.25.0\ntoolchain go1.25.0\n",
			requires: map[string]string{},
			replaces: map[string]string{},
		},
		{
			// Both forms of `tool` must agree. The block form was swallowed at
			// top level by the single-line catch-all, so a first-party tool
			// path inside it became an "unreadable" line and failed the run.
			name: "tool block and tool line are both ignored, not unparsed",
			content: "module x\n\ntool (\n\t" + ModulePrefix + "/cmd/shortfall\n\tgolang.org/x/tools/cmd/stringer\n)\n\n" +
				"tool " + ModulePrefix + "/cmd/shortfall\n",
			requires: map[string]string{},
			replaces: map[string]string{},
		},
		{
			name:     "an unreadable first-party line is recorded, never dropped",
			content:  "module x\n\nrequire " + ModulePrefix + " v0.2.0 extra junk here\n",
			requires: map[string]string{},
			replaces: map[string]string{},
			unparsed: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseMod("go.mod", c.content)
			assertMap(t, "requires", got.Requires, c.requires)
			assertMap(t, "replaces", got.Replaces, c.replaces)
			if len(got.Unparsed) != c.unparsed {
				t.Fatalf("unparsed = %d %v, want %d", len(got.Unparsed), got.Unparsed, c.unparsed)
			}
		})
	}
}

func assertMap(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}

	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s[%q] = %q, want %q", label, k, got[k], v)
		}
	}
}

func TestIsLocalTarget(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{name: "parent path", target: "../..", want: true},
		{name: "dot-slash path", target: "./vendor/x", want: true},
		{name: "absolute path", target: "/tmp/x", want: true},
		{name: "bare dot", target: ".", want: true},
		{name: "module path is not local", target: "example.com/fork", want: false},
		{name: "first-party module path is not local", target: ModulePrefix, want: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsLocalTarget(c.target); got != c.want {
				t.Fatalf("IsLocalTarget(%q) = %v, want %v", c.target, got, c.want)
			}
		})
	}
}

// loadMods reads every tracked go.mod and refuses to hand back a set that
// would make the checks below vacuous: no files, no first-party edges, or
// any first-party line the parser could not read.
func loadMods(t *testing.T) []Mod {
	t.Helper()
	files, err := TrackedModFiles(repoRoot)
	if err != nil {
		t.Fatalf("listing go.mod files: %v", err)
	}

	mods := make([]Mod, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}

		m := ParseMod(f, string(b))
		if len(m.Unparsed) > 0 {
			t.Fatalf("%s: %d first-party line(s) this parser could not read: %s. It would "+
				"otherwise drop out of every check below while the suite stayed green — the "+
				"one failure mode this package must not have", f, len(m.Unparsed),
				strings.Join(m.Unparsed, " | "))
		}

		mods = append(mods, m)
	}

	total := 0
	for _, m := range mods {
		total += len(m.Requires)
	}

	if total < firstPartyRequireFloor {
		t.Fatalf("found %d first-party require(s) across %d go.mod file(s), want at least %d. "+
			"Every check below loops over these, so a short scan does not weaken them — it "+
			"empties them, and they pass having examined nothing", total, len(files), firstPartyRequireFloor)
	}

	return mods
}

func TestEveryFirstPartyRequireNamesOneVersion(t *testing.T) {
	byPath := Versions(loadMods(t))
	for _, path := range sortedKeys(byPath) {
		t.Run(path, func(t *testing.T) {
			versions := byPath[path]
			if len(versions) == 1 {
				return
			}

			var detail []string
			for _, v := range sortedKeys(versions) {
				detail = append(detail, v+" ("+strings.Join(versions[v], ", ")+")")
			}

			// Not a build failure for an adopter — minimal version selection
			// picks the highest and resolves. It is a smell: skew means one of
			// these was updated and the others were forgotten, and the next
			// release tags a version some module never asked for.
			t.Fatalf("%s is required at %d different versions: %s. A local replace hides the "+
				"skew from every build that runs here, so it persists silently until a release "+
				"tags one version and the stragglers name something else",
				path, len(versions), strings.Join(detail, "; "))
		})
	}
}

func TestNoFirstPartyRequireNamesAPlaceholderVersion(t *testing.T) {
	for _, m := range loadMods(t) {
		for _, path := range sortedKeys(m.Requires) {
			t.Run(m.File+" "+path, func(t *testing.T) {
				v := m.Requires[path]
				if v == "v0.0.0" {
					t.Fatalf("%s requires %s at %s, a version this repository has never "+
						"tagged. It resolves here only because a replace stands in for it; "+
						"`go get` of that module path fails for everyone else", m.File, path, v)
				}

				if !strings.HasPrefix(v, "v") {
					t.Fatalf("%s requires %s at %q, which is not a semver tag", m.File, path, v)
				}
			})
		}
	}
}

// TestEveryFirstPartyRequireIsReplacedLocally is a deliberate ratchet on the
// CURRENT posture, not a permanent law of the repository. While no
// first-party version is published, a require without a local replace cannot
// resolve outside the workspace — goreleaser builds with GOWORK=off. Once the
// tag waves in CONTRIBUTING have run, dropping a replace becomes the correct
// move rather than a defect (it is the tracked path to a working `go
// install`), and this test is expected to be revisited in that change rather
// than to keep enforcing a posture that has ended.
func TestEveryFirstPartyRequireIsReplacedLocally(t *testing.T) {
	for _, m := range loadMods(t) {
		for _, path := range sortedKeys(m.Requires) {
			t.Run(m.File+" "+path, func(t *testing.T) {
				target, ok := m.Replaces[path]
				if !ok {
					t.Fatalf("%s requires %s but does not replace it. Until that version is "+
						"published, a build of this module outside the workspace cannot "+
						"resolve it — and goreleaser builds with GOWORK=off", m.File, path)
				}

				if !IsLocalTarget(target) {
					t.Fatalf("%s replaces %s with %q, which is another module path rather "+
						"than a local one. That still has to resolve from the proxy, so it "+
						"does not make the require version irrelevant the way a path does",
						m.File, path, target)
				}
			})
		}
	}
}

// TestTrackedModFilesExplainsAGitFailure covers the branch that exists so a
// broken scan can be diagnosed: git reports its reason on stderr, and %w on
// an *exec.ExitError renders only "exit status 128". Without this the one
// place this package can fail for infrastructure reasons is its least
// explainable.
func TestTrackedModFilesExplainsAGitFailure(t *testing.T) {
	cases := []struct {
		name string
		root string
		want string
	}{
		{name: "not a git work tree", root: t.TempDir(), want: "not a git repository"},
		{name: "root does not exist", root: filepath.Join(t.TempDir(), "absent"), want: "No such file or directory"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := TrackedModFiles(c.root)
			if err == nil {
				t.Fatal("want an error, got nil — a scan that cannot run must not look like an empty repository")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to carry git's own diagnosis (%q). Only the "+
					"exit status survives if ExitError.Stderr is dropped", err, c.want)
			}
		})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}
