// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// buildInfo assembles the shape Go records for a build, so each case can state
// exactly what the runtime would hand us.
func buildInfo(mainVersion, revision, modified string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	bi.Main.Version = mainVersion
	if revision != "" {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.revision", Value: revision})
	}

	if modified != "" {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.modified", Value: modified})
	}

	return bi
}

func TestResolveBuild(t *testing.T) {
	cases := []struct {
		name        string
		ldVersion   string
		ldCommit    string
		bi          *debug.BuildInfo
		ok          bool
		wantVersion string
		wantCommit  string
	}{
		{
			// The release archives: goreleaser stamps v{{.Version}} (see
			// .goreleaser.yaml and TestReleaseStampIsVPrefixed), so this is
			// v-prefixed, and nothing the runtime reports may override it.
			name:      "goreleaser ldflags win outright",
			ldVersion: "v0.3.0", ldCommit: "abc1234",
			bi: buildInfo("v0.9.9", "deadbeefcafe", ""), ok: true,
			wantVersion: "v0.3.0", wantCommit: "abc1234",
		},
		{
			// The README's recommended install path. Go records the module
			// version, so the binary can name itself.
			name:      "go install pkg@version reports the module version",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("v0.3.0", "", ""), ok: true,
			wantVersion: "v0.3.0", wantCommit: "",
		},
		{
			// go build from a checkout: no module version, but a revision.
			name:      "a VCS build reports the revision",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("(devel)", "0123456789abcdef0123", ""), ok: true,
			wantVersion: devVersion, wantCommit: "0123456789ab",
		},
		{
			name:      "a dirty VCS build says so",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("(devel)", "0123456789abcdef0123", "true"), ok: true,
			wantVersion: devVersion, wantCommit: "0123456789ab+dirty",
		},
		{
			name:      "a clean VCS build is not marked dirty",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("(devel)", "0123456789abcdef0123", "false"), ok: true,
			wantVersion: devVersion, wantCommit: "0123456789ab",
		},
		{
			// go run, or a stripped binary: nothing to report, and we must
			// not invent a version.
			name:      "no build info falls back to dev",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: nil, ok: false,
			wantVersion: devVersion, wantCommit: "",
		},
		{
			name:      "an empty main version is not treated as a version",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("", "", ""), ok: true,
			wantVersion: devVersion, wantCommit: "",
		},
		{
			// A workspace/pseudo-version build: Go derives the version FROM
			// the revision, so printing the hash again only doubles it.
			name:      "a pseudo-version that already names the revision drops the duplicate",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("v0.3.1-0.20260902214032-2005246f7818", "2005246f7818abcd", ""), ok: true,
			wantVersion: "v0.3.1-0.20260902214032-2005246f7818", wantCommit: "",
		},
		{
			name:      "a dirty pseudo-version still drops the duplicate revision",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("v0.3.1-0.20260902214032-2005246f7818+dirty", "2005246f7818abcd", "true"), ok: true,
			wantVersion: "v0.3.1-0.20260902214032-2005246f7818+dirty", wantCommit: "",
		},
		{
			name:      "a short revision is not truncated past its length",
			ldVersion: devVersion, ldCommit: devCommit,
			bi: buildInfo("(devel)", "abc123", ""), ok: true,
			wantVersion: devVersion, wantCommit: "abc123",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotVersion, gotCommit := resolveBuild(c.ldVersion, c.ldCommit, c.bi, c.ok)
			if gotVersion != c.wantVersion {
				t.Errorf("version = %q, want %q", gotVersion, c.wantVersion)
			}

			if gotCommit != c.wantCommit {
				t.Errorf("commit = %q, want %q", gotCommit, c.wantCommit)
			}
		})
	}
}

func TestFormatVersionLine(t *testing.T) {
	cases := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{"a stamped release names both", "v0.3.0", "abc1234", "shortfall v0.3.0 (abc1234)"},
		{"an installed build names the version alone", "v0.3.0", "", "shortfall v0.3.0"},
		{"a bare dev build stays honest", devVersion, "", "shortfall dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatVersionLine(c.version, c.commit); got != c.want {
				t.Errorf("formatVersionLine(%q, %q) = %q, want %q", c.version, c.commit, got, c.want)
			}
		})
	}
}

// TestVersionIsNotDevWhenBuiltAsAModule is the regression pin for
// The bug was that `go install pkg@version` produced a binary reporting
// "dev". Any change that stops consulting build info fails here.
func TestVersionIsNotDevWhenBuiltAsAModule(t *testing.T) {
	got, _ := resolveBuild(devVersion, devCommit, buildInfo("v1.2.3", "", ""), true)
	if got == devVersion {
		t.Fatalf("a module build must not report %q — that is the g0o defect", devVersion)
	}
}

// TestReleaseStampIsVPrefixed pins the two provenance paths to one shape, on
// BOTH the release and the dry-run path.
//
// goreleaser's {{.Version}} strips the leading "v" (the published archives are
// shortfall_0.3.0_*), while the build-info fallback reports the module version
// as "v0.3.0" — so a bare {{.Version}} means one build answers "0.3.0" where
// another says "v0.3.0". {{.Tag}} would fix that and break the dry run: the
// snapshot job hands goreleaser the last root tag as GORELEASER_CURRENT_TAG
// and {{.Tag}} ignores snapshot mode, so every snapshot binary would claim to
// BE the last release. Only v{{.Version}} is right on both. Nothing else
// catches any of this.
func TestReleaseStampIsVPrefixed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}

	spec := string(b)
	cases := []struct {
		name    string
		want    bool
		needle  string
		explain string
	}{
		{"the version ldflag prefixes the snapshot-aware version", true, "-X main.version=v{{.Version}}",
			"a release binary must report the v-prefixed version, matching what build info reports"},
		{"the version ldflag does not stamp the v-stripped version", false, "-X main.version={{.Version}}",
			"bare {{.Version}} drops the leading v and desyncs a release binary from an installed one"},
		{"the version ldflag does not stamp the tag", false, "-X main.version={{.Tag}}",
			"{{.Tag}} ignores snapshot mode, so a dry-run binary would claim to be the last release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := strings.Contains(spec, c.needle); got != c.want {
				t.Errorf("contains(%q) = %v, want %v — %s", c.needle, got, c.want, c.explain)
			}
		})
	}
}
