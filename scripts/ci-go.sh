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

# The golangci-lint VERSION has one home: this variable. CI's installer
# (scripts/install-golangci-lint.sh) reads it back with `lint-version`
# rather than hardcoding a second copy, so a bump here moves both. The
# env override is the same escape hatch GOVULNCHECK_VERSION has above, and
# it moves both too — the installer reads the overridden value as well.
if [ "$mode" = "lint-version" ]; then
  echo "$GOLANGCI_LINT_VERSION"
  exit 0
fi

# Resolve the linter ONCE, before the module loop. Candidates in order:
# PATH, then $GOBIN, then $GOPATH/bin (`go install` puts it in one of the
# latter two without touching PATH). A candidate is used only if it IS the
# pinned version; a different version is ignored in favour of `go run` at
# the pin, so the analyzer is the same one CI runs no matter what a machine
# happens to have installed.
#
# The `go run` route is the pattern GOVULNCHECK_VERSION already uses: no
# manual install anywhere. It builds the linter on first use and caches it;
# a build failure exits non-zero and fails the check, so a missing or
# unbuildable linter is LOUD, never a silent skip.
lint_bin=""
lint_via=""
if [ "$mode" = "lint" ]; then
  want="${GOLANGCI_LINT_VERSION#v}"
  found=""
  seen=""
  for cand in \
    "$(command -v golangci-lint 2>/dev/null || true)" \
    "$(go env GOBIN 2>/dev/null || true)/golangci-lint" \
    "$(go env GOPATH 2>/dev/null || true)/bin/golangci-lint"
  do
    [ -n "$cand" ] && [ -x "$cand" ] || continue
    # GOBIN and GOPATH/bin are frequently the same directory; report once.
    case "$seen" in *"[$cand]"*) continue ;; esac
    seen="$seen[$cand]"
    got="$("$cand" version --short 2>/dev/null || true)"
    if [ "$got" = "$want" ]; then
      lint_bin="$cand"; lint_via=binary
      echo "ci-go.sh: using golangci-lint $want at $cand" >&2
      break
    fi
    # Plain `[ ... ] && x=y` would be the loop body's last command, and a
    # false test there trips errexit. Keep it an if.
    if [ -n "$got" ]; then found="${found}${found:+, }$cand ($got)"; fi
  done
  if [ -z "$lint_via" ]; then
    lint_via=gorun
    if [ -n "$found" ]; then
      echo "ci-go.sh: ignoring golangci-lint not at the pinned $want: $found" >&2
    else
      echo "ci-go.sh: no golangci-lint $want installed" >&2
    fi
    echo "ci-go.sh: linting via pinned 'go run golangci-lint@${GOLANGCI_LINT_VERSION}' (first run builds it, later runs are cached)" >&2
  fi
fi

# golangci-lint searches every ancestor directory AND $HOME for a
# .golangci.* file, so an unrelated config above someone's clone would
# silently change which linters run — locally green, red on CI's bare
# runner, which is the whole failure this script exists to prevent. Pin the
# config the same way the version is pinned: this repo's file if it has
# one, and --no-config when it does not.
lint_cfg=""
for c in "$root/.golangci.yml" "$root/.golangci.yaml" "$root/.golangci.toml" "$root/.golangci.json"; do
  if [ -f "$c" ]; then lint_cfg="$c"; break; fi
done

# Runs the resolved linter in $1. A function, not a command string: IFS is
# newline-only here, so an unquoted string would never split into argv.
run_lint() {
  if [ -n "$lint_cfg" ]; then set -- "$1" --config "$lint_cfg"; else set -- "$1" --no-config; fi
  d="$1"; shift
  if [ "$lint_via" = binary ]; then
    (cd "$d" && "$lint_bin" run "$@" ./...)
  else
    (cd "$d" && go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" run "$@" ./...)
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
