#!/bin/sh
# Lint one commit-message (or PR-title) header against the repo convention:
#   type: summary (tracked-item-id)   or   type: summary (no-bead: reason)
# Types: feat fix docs chore test refactor perf ci build revert
#
# Usage: scripts/commit-lint.sh "<header>"   or   scripts/commit-lint.sh -f <msg-file>
#
# Byte-length note: length is measured in BYTES under LC_ALL=C on every
# platform, so macOS and CI agree (a char-vs-byte skew between local hook
# and CI fence has bitten this workspace before).
set -eu
LC_ALL=C
export LC_ALL

if [ "${1:-}" = "-f" ]; then
  header="$(head -n1 "$2")"
else
  header="${1:?usage: commit-lint.sh \"<header>\" | -f <file>}"
fi

# Merge/squash artifacts GitHub generates are exempt.
case "$header" in
  "Merge "*|"Revert \""*) exit 0 ;;
esac

len="$(printf '%s' "$header" | wc -c | tr -d ' ')"
if [ "$len" -gt 100 ]; then
  echo "commit-lint: header is ${len} bytes (max 100): $header" >&2
  exit 1
fi

if ! printf '%s' "$header" | grep -Eq \
  '^(feat|fix|docs|chore|test|refactor|perf|ci|build|revert)(\([a-z0-9/-]+\))?: .+ \((workspace-[a-z0-9.]+|no-bead: [^)]+)\)( \(#[0-9]+\))?$'; then
  cat >&2 <<EOF
commit-lint: header does not match the convention:
  type: summary (tracked-item-id)      e.g.  feat: add Money type (workspace-abc.1)
  type: summary (no-bead: reason)      e.g.  chore: fix typo (no-bead: trivial)
Types: feat fix docs chore test refactor perf ci build revert
Got: $header
EOF
  exit 1
fi
exit 0
