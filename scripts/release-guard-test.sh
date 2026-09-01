#!/usr/bin/env bash
# Regression suite for release-guard.sh, run by the release-guard CI job and
# runnable locally (./scripts/release-guard-test.sh).
#
# Every case builds a throwaway git repo with a cmd/shortfall/go.mod and a
# tag layout, then asserts the guard's verdict. The shapes that must FAIL are
# the point: each one is a way to publish a binary whose library code is not
# the version on its label, and every one of them is invisible to the rest of
# the suite because go.work resolves the modules locally regardless.
set -euo pipefail
guard="$(cd "$(dirname "$0")" && pwd)/release-guard.sh"
fails=0
tmproot=$(mktemp -d)
trap 'rm -rf "$tmproot"' EXIT

# make_repo <dir> <go.mod body> <tag...> — waves 1-2 land on the first
# commit and wave 3 on a second, which is the real release shape: the CLI's
# go.sum can only be tidied once the earlier tags are fetchable.
make_repo() {
  d="$tmproot/$1"; body="$2"; shift 2
  mkdir -p "$d/cmd/shortfall"
  printf '%s' "$body" > "$d/cmd/shortfall/go.mod"
  git -C "$d" init -q 2>/dev/null
  git -C "$d" add -A
  git -C "$d" -c user.email=t@t -c user.name=t commit -q -m base
  for t in "$@"; do git -C "$d" tag "$t"; done
  git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m wave3
  echo "$d"
}

expect() { # expect <pass|fail> <description> <tag> <repo dir>
  want="$1"; desc="$2"; tag="$3"; d="$4"
  if "$guard" "$tag" "$d" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "$got" != "$want" ]; then
    echo "FAIL: $desc (wanted $want, got $got)" >&2
    fails=$((fails + 1))
  fi
}

P=github.com/NightWatchEng/shortfall
good="module $P/cmd/shortfall

go 1.25.0

require (
	$P v0.3.0
	$P/adapters/query/promql v0.3.0
	$P/adapters/query/sql v0.3.0
	modernc.org/sqlite v1.57.0
)
"
tags=(v0.3.0 adapters/query/promql/v0.3.0 adapters/query/sql/v0.3.0)

d=$(make_repo happy "$good" "${tags[@]}")
expect pass "the real wave shape: earlier tags reachable, requires current" cmd/shortfall/v0.3.0 "$d"

# The version is read from the tag, so a tag for a version nothing names must
# not quietly release the version the tree happens to carry.
expect fail "tag names a version the requires do not"       cmd/shortfall/v0.4.0 "$d"
expect fail "tag without the cmd/shortfall/ prefix"          v0.3.0               "$d"
expect fail "no tag argument at all"                         ""                   "$d"

# Quoted module paths are legal go.mod grammar and defeated an earlier
# grep-based guard: every require here is stale, and it must still be caught.
d=$(make_repo quoted "module $P/cmd/shortfall

go 1.25.0

require (
	\"$P\" v0.2.0
	\"$P/adapters/query/promql\" v0.2.0
	\"$P/adapters/query/sql\" v0.2.0
)
" "${tags[@]}")
expect fail "quoted paths hide a wholly stale require set" cmd/shortfall/v0.3.0 "$d"

# One reinstated replace breaks `go install` for every adopter.
d=$(make_repo replaced "$good
replace $P => ../..
" "${tags[@]}")
expect fail "a first-party replace returns" cmd/shortfall/v0.3.0 "$d"

d=$(make_repo thirdparty "$good
replace modernc.org/sqlite => ../vendored/sqlite
" "${tags[@]}")
expect fail "a third-party replace returns" cmd/shortfall/v0.3.0 "$d"

# A guard that reads no requires has verified nothing and must say so.
d=$(make_repo norequires "module $P/cmd/shortfall

go 1.25.0

require modernc.org/sqlite v1.57.0
" "${tags[@]}")
expect fail "no first-party require to check" cmd/shortfall/v0.3.0 "$d"

d=$(make_repo malformed "module $P/cmd/shortfall

require ( oops
" "${tags[@]}")
expect fail "unparseable go.mod" cmd/shortfall/v0.3.0 "$d"

d=$(make_repo missing "$good" "${tags[@]}")
rm -rf "$d/cmd"
expect fail "cmd/shortfall/go.mod absent" cmd/shortfall/v0.3.0 "$d"

# Each wave is checked, not just the root: skipping wave 2 must not reach
# goreleaser as an opaque unknown-revision failure.
d=$(make_repo nowave1 "$good" adapters/query/promql/v0.3.0 adapters/query/sql/v0.3.0)
expect fail "wave 1 skipped (root tag absent)" cmd/shortfall/v0.3.0 "$d"

d=$(make_repo nowave2 "$good" v0.3.0 adapters/query/sql/v0.3.0)
expect fail "wave 2 partly skipped (promql tag absent)" cmd/shortfall/v0.3.0 "$d"

# A tag on an unrelated branch exists but does not publish the tree being
# built, which is the whole point of asking for reachability.
d=$(make_repo unreachable "$good" adapters/query/promql/v0.3.0 adapters/query/sql/v0.3.0)
git -C "$d" checkout -q --detach HEAD~1
git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m sidebranch
git -C "$d" tag v0.3.0
git -C "$d" checkout -q -
expect fail "root tag exists but is not reachable from HEAD" cmd/shortfall/v0.3.0 "$d"

# --skip=validate leaves goreleaser's own semver check as a warning, so a
# non-semver version has to die here or it silently defeats prerelease: auto.
d=$(make_repo nonsemver "module $P/cmd/shortfall

go 1.25.0

require $P v0.3
" "v0.3")
expect fail "version is not semver" cmd/shortfall/v0.3 "$d"

# A prerelease IS semver and must be accepted — goreleaser marks it
# pre-release from exactly this suffix.
pre="module $P/cmd/shortfall

go 1.25.0

require $P v0.3.0-rc1
"
d=$(make_repo prerelease "$pre" v0.3.0-rc1)
expect pass "a semver prerelease is a valid release" cmd/shortfall/v0.3.0-rc1 "$d"

if [ "$fails" -ne 0 ]; then
  echo "release-guard-test: $fails case(s) failed" >&2
  exit 1
fi
echo "release-guard-test: all cases passed"
