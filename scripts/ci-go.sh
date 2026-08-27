#!/bin/sh
# Run a Go check across EVERY module in the repo, discovered dynamically —
# nested adapter modules must never be silently skipped when they land.
# Usage: scripts/ci-go.sh fmt|vet|build|test|vuln
set -eu

mode="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"

fail=0
for modfile in $(cd "$root" && git ls-files '*go.mod' 'go.mod'); do
  dir="$root/$(dirname "$modfile")"
  echo "==> $mode: ${modfile%go.mod}"
  case "$mode" in
    fmt)
      bad="$(gofmt -l "$dir" | grep -v '^$' || true)"
      if [ -n "$bad" ]; then echo "gofmt needed:"; echo "$bad"; fail=1; fi
      ;;
    vet)   (cd "$dir" && go vet ./...) || fail=1 ;;
    build) (cd "$dir" && go build ./...) || fail=1 ;;
    test)  (cd "$dir" && go test ./...) || fail=1 ;;
    vuln)  (cd "$dir" && go run golang.org/x/vuln/cmd/govulncheck@latest ./...) || fail=1 ;;
    *) echo "unknown mode: $mode" >&2; exit 2 ;;
  esac
done
exit "$fail"
