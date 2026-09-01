#!/usr/bin/env bash
# Release guard: the last check between a wave-3 tag and published binaries.
#
# cmd/shortfall carries no `replace` directives, which is what makes `go
# install .../cmd/shortfall@version` work, and means its build resolves the
# core and the two query adapters from published tags exactly as an adopter
# does. That buys the install path at the cost of an ordering obligation:
# those tags must already exist, and the module must already name them, or
# the release ships a binary whose library code is not the version on the
# label. Nothing else in the tree can see that — `go.work` supplies the
# modules locally, so every build here stays green either way.
#
# Usage: release-guard.sh <wave-3 tag> [repo root]
# Prints the release version on stdout; every rejection is a `::error::`
# line and a non-zero exit. Lives in its own file rather than inline in the
# workflow so scripts/release-guard-test.sh can drive every branch.
set -euo pipefail

tag="${1:-}"
root="${2:-.}"
prefix=github.com/NightWatchEng/shortfall

die() { echo "::error::$*" >&2; exit 1; }

[ -n "$tag" ] || die "release-guard.sh needs the wave-3 tag as its first argument."

version="${tag#cmd/shortfall/}"
[ "$version" != "$tag" ] ||
  die "tag $tag does not carry the cmd/shortfall/ prefix this job triggers on."

# goreleaser is run with --skip=validate, which downgrades its own non-semver
# tag error to a warning and leaves ctx.Semver zeroed — silently defeating
# `prerelease: auto`. That check is therefore made here instead.
echo "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$' ||
  die "release version $version is not semver, so prerelease detection and the release name would both be wrong."

mod="$root/cmd/shortfall/go.mod"
[ -f "$mod" ] || die "$mod is missing — this guard would verify nothing."

# Parse once, and report a parse failure AS a parse failure: `go mod edit
# -json` writes its diagnosis to stderr and nothing to stdout, so reading its
# output inside a later test would blame whatever that test is about. Same
# for jq: run it where its exit status survives, never inside `[ ... ]`.
parsed=$(go mod edit -json "$mod") ||
  die "$mod could not be parsed by \`go mod edit -json\` (output above)."
[ -n "$parsed" ] ||
  die "\`go mod edit -json $mod\` produced no output, so nothing below would be checked."

replaces=$(printf '%s' "$parsed" | jq -r '.Replace | length') ||
  die "jq failed reading the replace list from $mod — the guard cannot report on what it did not read."
if [ "$replaces" != "0" ]; then
  printf '%s' "$parsed" | jq -r '.Replace[] | "  \(.Old.Path) => \(.New.Path)"' >&2
  die "$mod carries $replaces replace directive(s), which break \`go install\` for every adopter."
fi

requires=$(printf '%s' "$parsed" |
  jq -r --arg p "$prefix" '.Require[]? | select(.Path == $p or (.Path | startswith($p + "/"))) | "\(.Path) \(.Version)"') ||
  die "jq failed reading the require list from $mod — the guard cannot report on what it did not read."
[ -n "$requires" ] ||
  die "no first-party requires found in $mod. The CLI depends on the core and two query adapters, so zero means this guard read the wrong thing."

# The root tag is what goreleaser is handed as the release version, so it is
# asserted directly rather than incidentally — the loop below would only
# reach it while the CLI happens to require the core module by name.
git -C "$root" rev-parse -q --verify "refs/tags/$version" >/dev/null ||
  die "root tag $version does not exist. Push the earlier tag waves before $tag (CONTRIBUTING, Releases)."

# A herestring, not a pipe, so the loop runs in this shell: piped, `die`'s
# `exit` would end only the subshell, and the run would stop on the
# pipeline's status under `set -e` rather than at the line that decided.
while IFS=' ' read -r path got; do
  [ -n "$path" ] || continue
  [ "$got" = "$version" ] ||
    die "$mod requires $path at $got, not $version — the CLI would ship under $version built against $got."

  sub="${path#"$prefix"}"
  sub="${sub#/}"
  dep="${sub:+$sub/}$version"
  git -C "$root" rev-parse -q --verify "refs/tags/$dep" >/dev/null ||
    die "tag $dep does not exist. Push the earlier tag waves before $tag (CONTRIBUTING, Releases)."
  git -C "$root" merge-base --is-ancestor "refs/tags/$dep^{commit}" HEAD ||
    die "tag $dep is not reachable from this commit, so it does not publish the tree being built."

  echo "  ok $path $version (tag $dep)" >&2
done <<< "$requires"

echo "$version"
