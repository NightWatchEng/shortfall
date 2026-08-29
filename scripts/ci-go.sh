#!/bin/sh
# Run a Go check across EVERY module in the repo, discovered dynamically —
# nested adapter modules must never be silently skipped when they land, and
# zero discovered modules is itself a failure (exit 2, infra), never a green
# no-op.
# Usage: scripts/ci-go.sh fmt|vet|build|test|vuln|lint
#        scripts/ci-go.sh lint-version   # print the pinned golangci-lint version
#
# These six modes are the ONE definition of the core checks: repo.yaml's
# verify.core lists all six and the CI "core checks" job runs all six, so
# `warden verify --scope core` and CI agree on the step list as well as on
# the module list. Adding a mode to one caller and not the other is the
# drift that cost a CI round trip on PR #90 (workspace-dfd).
set -eu
set -f                          # no glob expansion of the module list
nl='
'
IFS="$nl"                       # split module list on newlines only

mode="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
# Pinned enforcement tool versions (bump deliberately, never @latest).
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.7.0}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.13.1}"

# The golangci-lint pin lives HERE and nowhere else: CI's installer reads it
# back with `ci-go.sh lint-version` instead of hardcoding a second copy, so
# the local gate and the CI job cannot end up on different analyzers.
if [ "$mode" = "lint-version" ]; then
  echo "$GOLANGCI_LINT_VERSION"
  exit 0
fi

# Resolve the linter ONCE, before the module loop:
#   - a PATH golangci-lint AT the pin -> use it. That is what CI installs
#     (a sha256-verified release tarball, content-addressed), and a dev
#     machine that installed the same version gets the same speed.
#   - anything else — absent, or present at a DIFFERENT version -> the
#     pinned `go run`, exactly the pattern GOVULNCHECK_VERSION already uses.
#     No manual install is needed anywhere, and the pin, never the machine,
#     decides what `lint` means.
# `go run` builds the linter on first use and caches it afterwards; a build
# failure exits non-zero and fails the check, so the linter is never
# silently skipped.
lint_via=""
if [ "$mode" = "lint" ]; then
  want="${GOLANGCI_LINT_VERSION#v}"
  have=""
  if command -v golangci-lint >/dev/null 2>&1; then
    have="$(golangci-lint version --short 2>/dev/null || true)"
  fi
  if [ "$have" = "$want" ]; then
    lint_via=path
    echo "ci-go.sh: using golangci-lint $want from PATH" >&2
  else
    lint_via=gorun
    if [ -n "$have" ]; then
      echo "ci-go.sh: PATH golangci-lint is $have, not the pinned $want — ignoring it" >&2
    else
      echo "ci-go.sh: no golangci-lint on PATH" >&2
    fi
    echo "ci-go.sh: linting via pinned 'go run golangci-lint@${GOLANGCI_LINT_VERSION}' (first run builds it, later runs are cached)" >&2
  fi
fi

# Runs the resolved linter in $1. A function, not a command string: IFS is
# newline-only here, so an unquoted string would never split into argv.
run_lint() {
  if [ "$lint_via" = path ]; then
    (cd "$1" && golangci-lint run ./...)
  else
    (cd "$1" && go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" run ./...)
  fi
}

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
    lint)  run_lint "$dir" || fail=1 ;;
    *) echo "unknown mode: $mode" >&2; exit 2 ;;
  esac
done

if [ "$count" -eq 0 ]; then
  echo "ci-go.sh: no go.mod found — refusing to pass vacuously" >&2
  exit 2
fi
exit "$fail"
