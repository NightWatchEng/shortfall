#!/bin/sh
# Install the pinned golangci-lint onto a CI runner, verified by sha256.
#
# Why a script and not two inline blocks: the version lives in ci-go.sh
# (`ci-go.sh lint-version`) and the tarball checksum lives here, so each has
# exactly ONE home. Both CI jobs that need the linter call this file, which
# is what stops the two from drifting — the failure this repo already paid
# for once with the step lists themselves (workspace-dfd).
#
# A release tarball checked against a hardcoded sha256 is content-addressed:
# a stronger pin than any tag, and faster on a cold runner than building the
# linter from source (ci.yml records that build as network-fragile). Machines
# WITHOUT the pinned binary do not need this script at all — ci-go.sh falls
# back to `go run` at the same pin.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
v="$("$here/ci-go.sh" lint-version)"
v="${v#v}"

# Checksums for the version ci-go.sh pins. Bump these in the same commit as
# the pin: a mismatch fails here, loudly, instead of linting with a
# different analyzer than the one the pin names.
case "$v" in
  2.13.1) sha256=b17bfbc9d4aaa48be7f4f1ce3240bc3d8200c870c072bacf15c26219e2cfb9cc ;;
  *)
    echo "install-golangci-lint.sh: no checksum on record for golangci-lint $v." >&2
    echo "  ci-go.sh pins $v; add that release's linux-amd64 sha256 here in the" >&2
    echo "  same commit as the pin bump. Refusing to install an unverified binary." >&2
    exit 2
    ;;
esac

# Only the shape both CI jobs run on. Anywhere else, ci-go.sh's `go run`
# fallback already covers the linter, so this script fails loudly rather
# than installing something it cannot verify.
if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
  echo "install-golangci-lint.sh: only linux-amd64 checksums are on record" >&2
  echo "  (this is $(uname -s)/$(uname -m)). Use scripts/ci-go.sh lint, which" >&2
  echo "  runs the same pinned version through 'go run'." >&2
  exit 2
fi

tar="/tmp/golangci-lint-${v}.tar.gz"
dir="golangci-lint-${v}-linux-amd64"

curl -sSfL -o "$tar" \
  "https://github.com/golangci/golangci-lint/releases/download/v${v}/${dir}.tar.gz"
echo "${sha256}  ${tar}" | sha256sum -c -
tar -xzf "$tar" -C /tmp
install -m 0755 "/tmp/${dir}/golangci-lint" /usr/local/bin/golangci-lint

# Assert the installed binary really is the pin. ci-go.sh compares the same
# way and would silently fall back to `go run` on a mismatch; here that must
# be an error, because a mismatch means this script installed the wrong file.
got="$(golangci-lint version --short)"
if [ "$got" != "$v" ]; then
  echo "install-golangci-lint.sh: installed $got but ci-go.sh pins $v" >&2
  exit 2
fi
echo "install-golangci-lint.sh: golangci-lint $got installed (pin $v)"
