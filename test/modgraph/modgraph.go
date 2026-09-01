// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package modgraph checks that the repository's own modules agree about
// which version of each other they depend on.
//
// The bug this exists to catch does not go red on its own. Every nested
// go.mod consumed as a library replaces its first-party dependencies with a
// relative path, and a replace makes the require version irrelevant to every
// build that runs here — so a require can name a version that was never
// tagged, and nothing in gofmt, vet, build, test, vuln or lint has any
// reason to object. It stays invisible until an adopter, who sees no replace
// at all, resolves it for real. `cmd/shortfall` shipped requiring two
// sibling modules at v0.0.0 this way.
//
// Be precise about what this adds, because the go tooling already catches
// more than it looks like. Inside the workspace, go rejects a malformed
// go.mod outright and refuses a replace that conflicts with go.work — both
// were tried against this checker and never reached it. Three conditions
// are invisible to go and caught only here, each reproduced red against
// this checker and green against the rest of the suite:
//
//   - a require naming a version that was never tagged (v0.0.0),
//   - the same module required at different versions by different modules,
//     and
//   - a first-party require with no local replace, in a module that is
//     supposed to carry one.
//
// The first two are invisible because a local replace makes the require
// version dead weight for every build that runs here; the third because
// go.work's `use` list supplies the module regardless.
//
// That third check is not universal, and the exception runs the other way.
// `go install pkg@version` refuses any module whose go.mod carries replace
// directives at all — benign ones included — so a module meant to be
// installed that way must carry none. For those (InstallTargets) the
// requirement inverts: a first-party replace is the defect, because it
// breaks `go install` for everyone while go.work keeps every build here
// green. Both directions are checked; neither module is simply exempt.
//
// A checker for that bug class has one failure mode worse than not
// existing: understanding a go.mod less than it thinks it does, and
// reporting green over the gap. So ParseMod records what it could NOT read
// (Unparsed) rather than silently contributing nothing, and the tests fail
// on a first-party line the parser did not understand and on a scan that
// found implausibly few edges. The first cut had neither, and a reviewer
// reproduced the original bug sitting green behind a quoted module path.
//
// The non-local-replace check is belt to that braces: go.work makes a
// first-party replace onto a module path a hard conflict, so go refuses to
// build the test binary before this check can speak. It stays because it
// costs nothing and the day a module leaves the `use` list it is the only
// thing looking.
//
// What this package does NOT check: that the agreed version was ever
// published. Only a real resolution against the module proxy can answer
// that, and it needs the repository public and the tags pushed
// (CONTRIBUTING, "Releases"). The check here is internal consistency —
// necessary, not sufficient.
package modgraph

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ModulePrefix is this repository's module path. A require or replace whose
// path is this, or is under it, is first-party and subject to these checks.
const ModulePrefix = "github.com/NightWatchEng/shortfall"

// InstallTargets names the go.mod files of modules published for
// `go install pkg@version`, keyed by repo-relative path. That command
// rejects any module whose go.mod carries a replace directive — "it must
// not contain directives that would cause it to be interpreted differently
// than if it were the main module" (`go help install`), enforced
// unconditionally, first-party or not — so these modules resolve their
// dependencies through published tags, not relative paths, and the release
// build reaches them the same way an adopter does.
//
// Adding a module here is a release-pipeline decision, not a convenience:
// its dependencies must already be tagged and published when it is built,
// which is why the release fires on `cmd/shortfall/v*` (wave 3) rather than
// the root `v*` tag (wave 1). See CONTRIBUTING, "Releases".
var InstallTargets = map[string]bool{
	"cmd/shortfall/go.mod": true,
}

// Mod is one go.mod's first-party edges.
type Mod struct {
	// File is the repo-relative path of the go.mod (e.g. "cmd/shortfall/go.mod").
	File string
	// Requires maps a first-party module path to the version required.
	Requires map[string]string
	// Replaces maps a first-party module path to its replacement target's
	// PATH — a trailing version on the right-hand side is dropped, since
	// nothing here needs it. The path is kept rather than a bool so callers
	// can tell a local target from a module-path substitution; "replaced" and
	// "resolvable from this checkout" are different claims.
	Replaces map[string]string
	// OtherReplaces holds the left-hand paths of replace directives that are
	// NOT first-party. Every other check here is about first-party edges and
	// has no opinion on them, but `go install pkg@version` refuses a module
	// carrying ANY replace directive, so an InstallTargets module has to be
	// judged on all of them. Recorded rather than counted so the failure can
	// name the offending path.
	OtherReplaces []string
	// Unparsed holds lines that mention ModulePrefix but that this parser
	// could not read as a module, require or replace. A first-party line the
	// checker does not understand is the one thing it must never pass over
	// quietly, so it is surfaced instead of dropped.
	Unparsed []string
}

// TrackedModFiles asks git for the tracked go.mod files under root, so an
// untracked scratch module in a working tree cannot fail the build.
func TrackedModFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "go.mod", "*/go.mod").Output()
	if err != nil {
		// Output() puts git's own diagnosis in ExitError.Stderr, and %w on an
		// ExitError renders only "exit status 128" — which cannot distinguish
		// "git is missing" from "not a work tree" from "bad pathspec".
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}

		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}

	return files, nil
}

// IsFirstParty reports whether a module path belongs to this repository.
// The boundary is a path separator, so an unrelated "…/shortfall-extras" is
// correctly excluded.
func IsFirstParty(path string) bool {
	return path == ModulePrefix || strings.HasPrefix(path, ModulePrefix+"/")
}

// IsLocalTarget reports whether a replace target is a filesystem path rather
// than another module path. Only a local target makes the require version
// irrelevant to a build from this checkout, which is the property the
// replace checks actually depend on.
func IsLocalTarget(target string) bool {
	return strings.HasPrefix(target, "./") ||
		strings.HasPrefix(target, "../") ||
		strings.HasPrefix(target, "/") ||
		target == "." || target == ".."
}

// unquote strips the optional quoting go.mod's grammar allows around a
// module path. Without this a quoted first-party path reads as third-party
// and vanishes from every check.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '`' && s[len(s)-1] == '`') {
		return s[1 : len(s)-1]
	}

	return s
}

// ParseMod extracts the first-party requires and replaces from one go.mod's
// contents, and records any first-party line it could not read.
//
// Both `require` and `replace` have a single-line and a parenthesised-block
// form and the go tooling writes both — the block form of replace is what
// test/loggolden uses. `replace` additionally permits a version on the left
// (`path v1.0.0 => target`), which is what `go mod edit -replace=path@v` writes.
func ParseMod(file, content string) Mod {
	m := Mod{File: file, Requires: map[string]string{}, Replaces: map[string]string{}}
	block := ""

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}

		if line == "" {
			continue
		}

		switch {
		case line == "require (":
			block = "require"
			continue
		case line == "replace (":
			block = "replace"
			continue
		case line == "exclude (" || line == "retract (" ||
			line == "tool (" || line == "godebug (":
			block = "ignored"
			continue
		case block != "" && line == ")":
			block = ""
			continue
		}

		fields := strings.Fields(line)
		for i := range fields {
			fields[i] = unquote(fields[i])
		}

		if parseLine(&m, block, fields) {
			continue
		}

		// Not understood. Only first-party lines matter: anything else is a
		// third-party require or a directive these checks have no opinion on.
		if strings.Contains(line, ModulePrefix) && block != "ignored" {
			m.Unparsed = append(m.Unparsed, line)
		}
	}

	return m
}

// parseLine records one directive and reports whether it was understood.
// A line naming a third-party module is "understood" — read and correctly
// found irrelevant — which is what keeps Unparsed to genuine gaps.
func parseLine(m *Mod, block string, fields []string) bool {
	arrow := -1
	for i, f := range fields {
		if f == "=>" {
			arrow = i
			break
		}
	}

	switch {
	case len(fields) >= 2 && fields[0] == "module":
		return true

	// Inside a require block: `<path> <version>`, no arrow.
	case block == "require" && arrow < 0 && len(fields) == 2:
		if IsFirstParty(fields[0]) {
			m.Requires[fields[0]] = fields[1]
		}

		return true

	// `require <path> <version>`.
	case fields[0] == "require" && arrow < 0 && len(fields) == 3:
		if IsFirstParty(fields[1]) {
			m.Requires[fields[1]] = fields[2]
		}

		return true

	// Inside a replace block: `<path> [<version>] => <target> [<version>]`.
	case block == "replace" && (arrow == 1 || arrow == 2) && len(fields) > arrow+1:
		if IsFirstParty(fields[0]) {
			m.Replaces[fields[0]] = fields[arrow+1]
		} else {
			m.OtherReplaces = append(m.OtherReplaces, fields[0])
		}

		return true

	// `replace <path> [<version>] => <target> [<version>]`.
	case fields[0] == "replace" && (arrow == 2 || arrow == 3) && len(fields) > arrow+1:
		if IsFirstParty(fields[1]) {
			m.Replaces[fields[1]] = fields[arrow+1]
		} else {
			m.OtherReplaces = append(m.OtherReplaces, fields[1])
		}

		return true

	// Directives these checks have no opinion on, and their block bodies.
	case block == "ignored",
		fields[0] == "go", fields[0] == "toolchain", fields[0] == "godebug",
		fields[0] == "tool", fields[0] == "exclude", fields[0] == "retract":
		return true
	}

	return false
}

// Versions collects every distinct version any module requires of a given
// first-party path, with the go.mod files that ask for each.
func Versions(mods []Mod) map[string]map[string][]string {
	byPath := map[string]map[string][]string{}
	for _, m := range mods {
		for path, version := range m.Requires {
			if byPath[path] == nil {
				byPath[path] = map[string][]string{}
			}

			byPath[path][version] = append(byPath[path][version], m.File)
		}
	}

	return byPath
}
