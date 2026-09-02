// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// devVersion and devCommit are what the vars below hold when nothing stamped
// them — the sentinel that sends resolveBuild to the build info.
const (
	devVersion = "dev"
	devCommit  = "none"
)

// revisionLen is how much of a VCS revision the version line shows: enough to
// identify a commit, short enough to read.
const revisionLen = 12

// resolveBuild decides what `shortfall version` reports.
//
// A goreleaser build wins outright — it stamped ldVersion/ldCommit through
// -ldflags -X (see .goreleaser.yaml), and those name the published release.
// Everything else falls back to the build info the Go toolchain records, which
// is what lets a binary from `go install pkg@version` name its own version
// instead of reporting "dev". That path is the one the README
// recommends, so it is the one most users' `version` output comes from.
//
// An empty commit means "not known", not "none": the module proxy carries no
// VCS metadata, so an installed binary knows its version and not its revision.
func resolveBuild(ldVersion, ldCommit string, bi *debug.BuildInfo, ok bool) (version, commit string) {
	if ldVersion != devVersion {
		return ldVersion, ldCommit
	}

	if !ok || bi == nil {
		return devVersion, ""
	}

	version = devVersion
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		version = v
	}

	var revision, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if revision == "" {
		return version, ""
	}

	commit = revision
	if len(commit) > revisionLen {
		commit = commit[:revisionLen]
	}

	// A pseudo-version is DERIVED from the revision, so it already carries the
	// hash: appending it again prints the same twelve characters twice.
	if strings.Contains(version, commit) {
		return version, ""
	}

	if modified == "true" {
		commit += "+dirty"
	}

	return version, commit
}

// formatVersionLine renders the version line, omitting a revision we do not
// have rather than printing a placeholder that reads like one.
func formatVersionLine(version, commit string) string {
	if commit == "" {
		return "shortfall " + version
	}

	return fmt.Sprintf("shortfall %s (%s)", version, commit)
}

// versionLine is what the version verb prints, resolved against this binary.
func versionLine() string {
	bi, ok := debug.ReadBuildInfo()

	return formatVersionLine(resolveBuild(version, commit, bi, ok))
}
