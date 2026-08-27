#!/bin/sh
# Run a Go check across EVERY module in the repo, discovered dynamically —
# nested adapter modules must never be silently skipped when they land, and
# zero discovered modules is itself a failure (exit 2, infra), never a green
# no-op.
# Usage: scripts/ci-go.sh fmt|vet|build|test|vuln|lint
set -eu
set -f                          # no glob expansion of the module list
nl='
'
IFS="$nl"                       # split module list on newlines only

mode="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
# Pinned enforcement tool versions (bump deliberately, never @latest).
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.7.0}"

# Only files literally named go.mod: 'go.mod' at the root, '*/go.mod' at any
# depth (git pathspec * crosses directories; the /go.mod suffix anchors the
# basename, so a stray 'djangogo.mod' never matches).
modules="$(cd "$root" && git ls-files 'go.mod' '*/go.mod')"

count=0
fail=0
for modfile in $modules; do
  count=$((count + 1))
  dir="$root/$(dirname "$modfile")"
  echo "==> $mode: ${modfile%go.mod}"
  case "$mode" in
    fmt)
      # gofmt -l prints unformatted files on stdout and parse errors on
      # stderr with a nonzero exit — both must fail the check.
      if ! out="$(gofmt -l "$dir" 2>&1)"; then
        echo "gofmt error:"; echo "$out"; fail=1
      elif [ -n "$out" ]; then
        echo "gofmt needed:"; echo "$out"; fail=1
      fi
      ;;
    vet)   (cd "$dir" && go vet ./...) || fail=1 ;;
    build) (cd "$dir" && go build ./...) || fail=1 ;;
    test)  (cd "$dir" && go test ./...) || fail=1 ;;
    vuln)  (cd "$dir" && go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...) || fail=1 ;;
    lint)  (cd "$dir" && golangci-lint run ./...) || fail=1 ;;
    *) echo "unknown mode: $mode" >&2; exit 2 ;;
  esac
done

if [ "$count" -eq 0 ]; then
  echo "ci-go.sh: no go.mod found — refusing to pass vacuously" >&2
  exit 2
fi
exit "$fail"
