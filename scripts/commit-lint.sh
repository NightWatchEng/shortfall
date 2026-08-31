#!/bin/sh
# Lint one commit-message (or PR-title) header against the repo convention:
#   type: summary (tracked-item-id)   or   type: summary (no-bead: reason)
# A bundled PR lists every id it closes in the one parens, single-space
# separated: (workspace-a workspace-b) — the only multi-id spelling.
# Types: feat fix docs chore test refactor perf ci build revert — no scopes;
# the convention is documented scope-free in CONTRIBUTING.md and the
# changelog filters rely on unscoped prefixes.
#
# Usage: scripts/commit-lint.sh "<header>"   or   scripts/commit-lint.sh -f <msg-file>
#
# Byte-length note: length is measured in BYTES under LC_ALL=C on every
# platform, so macOS and CI agree (a char-vs-byte skew between local hook
# and CI fence has bitten this workspace before). The cap applies to the
# TITLE; squash merge appends " (#N)" on main, which the regex tolerates.
set -eu
LC_ALL=C
export LC_ALL
nl='
'

file_mode=0
if [ "${1:-}" = "-f" ]; then
  file_mode=1
  header="$(head -n1 "$2")"
else
  header="${1:?usage: commit-lint.sh \"<header>\" | -f <file>}"
  case "$header" in
    *"$nl"*)
      echo "commit-lint: header must be a single line" >&2
      exit 1
      ;;
  esac
fi

# GitHub-generated merge/revert commits are exempt — but ONLY in hook (-f)
# mode: a squash merge never turns a PR title into a "Merge ..." header, so
# the CI fence must lint every title with no escape hatch.
if [ "$file_mode" = 1 ]; then
  case "$header" in
    "Merge "*|"Revert \""*) exit 0 ;;
  esac
fi

len="$(printf '%s' "$header" | wc -c | tr -d ' ')"
if [ "$len" -gt 100 ]; then
  echo "commit-lint: header is ${len} bytes (max 100): $header" >&2
  exit 1
fi

if ! printf '%s' "$header" | grep -Eq \
  '^(feat|fix|docs|chore|test|refactor|perf|ci|build|revert): .+ \((workspace-[a-z0-9.]+( workspace-[a-z0-9.]+)*|no-bead: [^)]+)\)( \(#[0-9]+\))?$'; then
  cat >&2 <<EOF
commit-lint: header does not match the convention:
  type: summary (tracked-item-id)      e.g.  feat: add Money type (workspace-abc.1)
  type: summary (id-1 id-2)            e.g.  fix: close both beads (workspace-a workspace-b)
  type: summary (no-bead: reason)      e.g.  chore: fix typo (no-bead: trivial)
A bundled PR lists every id it closes, single-space separated, in one parens.
Types (no scopes): feat fix docs chore test refactor perf ci build revert
Got: $header
EOF
  exit 1
fi

# A bundle lists DISTINCT ids — ERE has no backreference, so the duplicate
# check is a shell pass over the matched parens group. sed prints the group
# only for the id form; the no-bead form yields nothing and skips this.
ids="$(printf '%s' "$header" | sed -n -E 's/^.* \((workspace-[a-z0-9.]+( workspace-[a-z0-9.]+)*)\)( \(#[0-9]+\))?$/\1/p')"
if [ -n "$ids" ]; then
  dup="$(printf '%s' "$ids" | tr ' ' "$nl" | sort | uniq -d)"
  if [ -n "$dup" ]; then
    echo "commit-lint: duplicate tracked-item id in header: $dup" >&2
    exit 1
  fi
fi
exit 0
