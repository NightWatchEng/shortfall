#!/bin/sh
# Regression suite for commit-lint.sh, run by the pr-title-lint CI job and
# runnable locally (./scripts/commit-lint-test.sh). The multibyte cases pin
# the byte-vs-char fix: length is bytes under LC_ALL=C on every platform, so
# a 100-char header with an em-dash fails identically on macOS and CI.
set -eu
lint="$(dirname "$0")/commit-lint.sh"
fails=0

expect() { # expect <pass|fail> <description> [args...]
  want="$1"; desc="$2"; shift 2
  if "$lint" "$@" >/dev/null 2>&1; then got=pass; else got=fail; fi
  if [ "$got" != "$want" ]; then
    echo "FAIL: $desc (wanted $want, got $got)" >&2
    fails=$((fails + 1))
  fi
}

# 76 ascii filler chars for length-boundary cases.
filler="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

# Convention shapes.
expect pass "typed header with bead id"        "feat: add Money type (workspace-abc.1)"
expect pass "no-bead form"                     "chore: fix typo (no-bead: trivial)"
expect pass "squash suffix tolerated"          "fix: handle nil (workspace-x.2) (#12)"
expect fail "unknown type"                     "feet: add Money type (workspace-abc.1)"
expect fail "missing tracked-item id"          "feat: add Money type"
expect fail "scoped type"                      "feat(emit): add thing (workspace-abc.1)"

# Bundled-PR form: several ids in one parens, single-space separated — the
# ONE multi-id spelling (workspace-53y). Commas and doubled spaces stay
# rejected so the convention has exactly one canonical shape.
expect pass "two ids, space separated"          "fix: close two beads at once (workspace-abc workspace-x.2)"
expect pass "three ids, space separated"        "ci: sweep the scripts (workspace-a1 workspace-b2 workspace-c3)"
expect pass "multi-id with squash suffix"       "fix: close two beads at once (workspace-abc workspace-x.2) (#34)"
expect fail "comma-separated ids"               "fix: close two beads at once (workspace-abc, workspace-x.2)"
expect fail "comma without space"               "fix: close two beads at once (workspace-abc,workspace-x.2)"
expect fail "doubled space between ids"         "fix: close two beads at once (workspace-abc  workspace-x.2)"
expect fail "trailing space inside parens"      "fix: close two beads at once (workspace-abc )"
expect fail "bare second id without prefix"     "fix: close two beads at once (workspace-abc x.2)"

# Byte-length cap: exactly 100 bytes passes, 101 fails.
h100="feat: $filler (no-bead: len888)"  # 6+76+18 = 100 bytes
[ "$(printf '%s' "$h100" | LC_ALL=C wc -c | tr -d ' ')" = 100 ] || { echo "FAIL: h100 fixture is not 100 bytes" >&2; fails=$((fails + 1)); }
expect pass "exactly 100 ascii bytes"          "$h100"
# Shape-valid 101-byte header: fails only via the cap, so an off-by-one cap
# regression cannot hide behind a convention rejection.
h101="feat: a$filler (no-bead: len888)"  # 6+1+76+18 = 101 bytes
[ "$(printf '%s' "$h101" | LC_ALL=C wc -c | tr -d ' ')" = 101 ] || { echo "FAIL: h101 fixture is not 101 bytes" >&2; fails=$((fails + 1)); }
expect fail "101 ascii bytes, valid shape"     "$h101"

# The chars-vs-bytes regression: 100 chars whose em-dash makes 102 bytes must
# fail on every platform (a char-counting shell passed it locally while the
# CI fence rejected it). Octal escapes: POSIX printf — dash has no \xHH.
hmb="$(printf 'feat: %s\342\200\224 (no-bead: len88)' "$filler")" # em-dash: 100 chars, 102 bytes
[ "$(printf '%s' "$hmb" | LC_ALL=C wc -c | tr -d ' ')" = 102 ] || { echo "FAIL: hmb fixture is not 102 bytes" >&2; fails=$((fails + 1)); }
expect fail "multibyte header over 100 bytes"  "$hmb"

# Merge/revert exemption applies only in hook (-f) mode.
tmp="$(mktemp)"
printf 'Merge branch main\n' > "$tmp"
expect pass "merge commit exempt in -f mode"   -f "$tmp"
expect fail "merge title linted in arg mode"   "Merge branch main"
rm -f "$tmp"

if [ "$fails" -gt 0 ]; then
  echo "commit-lint-test: $fails case(s) failed" >&2
  exit 1
fi
echo "commit-lint-test: all cases pass"
