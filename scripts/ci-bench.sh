#!/bin/sh
# Benchmark gate: run every benchmark in every module at a given git ref,
# writing results to a file benchstat can compare.
#
#   scripts/ci-bench.sh run <out-file>     # bench the CURRENT tree
#   scripts/ci-bench.sh count              # print number of benchmarks found
#
# Exit codes are a contract the workflow relies on:
#   0 = benchmarks ran and results were written (run) / count printed (count)
#   2 = infra failure: no modules discovered, or `go test -list` itself
#       failed (compile error, download failure) — never a silent skip
#   3 = modules exist but hold zero benchmarks (the honest no-op)
#   other = a benchmark run failed; the failing `go test -bench` rc
#
# Performance is part of this library's contract (it runs inside adopting
# services' hot paths), so the CI job compares PR vs main with benchstat and
# surfaces the delta. The job is ADVISORY until baselines stabilize — but it
# is honest: when no benchmarks exist it says so in its own output instead
# of printing a green nothing.
set -eu
set -f
nl='
'
IFS="$nl"

mode="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
BENCH_TIME="${BENCH_TIME:-1x}"          # CI keeps runs cheap; benchstat needs
BENCH_COUNT="${BENCH_COUNT:-6}"         # >=6 samples for its statistics

modules="$(cd "$root" && git ls-files 'go.mod' '*/go.mod')"

require_modules() {
  m=0
  for _f in $modules; do m=$((m + 1)); done
  if [ "$m" -eq 0 ]; then
    echo "ci-bench.sh: no go.mod found — refusing to pass vacuously" >&2
    exit 2
  fi
}

# Prints the benchmark count for one module dir; a failing `go test -list`
# is an infra failure (exit 2), never counted as zero.
module_bench_count() {
  mdir="$1"
  if ! listing="$(cd "$mdir" && go test -run '^$' -list 'Benchmark.*' ./... 2>&1)"; then
    echo "ci-bench.sh: go test -list failed in $mdir:" >&2
    echo "$listing" >&2
    exit 2
  fi
  printf '%s\n' "$listing" | grep -c '^Benchmark' || true
}

case "$mode" in
  count)
    require_modules
    total=0
    for modfile in $modules; do
      n="$(module_bench_count "$root/$(dirname "$modfile")")"
      total=$((total + n))
    done
    echo "$total"
    ;;
  run)
    require_modules
    out="$2"
    : > "$out"
    found=0
    for modfile in $modules; do
      dir="$root/$(dirname "$modfile")"
      n="$(module_bench_count "$dir")"
      [ "$n" -eq 0 ] && continue
      found=$((found + n))
      echo "==> bench: ${modfile%go.mod} ($n benchmarks)"
      (cd "$dir" && go test -run '^$' -bench . -benchmem \
        -benchtime "$BENCH_TIME" -count "$BENCH_COUNT" ./...) >> "$out"
    done
    if [ "$found" -eq 0 ]; then
      echo "ci-bench.sh: no benchmarks found in any module — nothing to compare" >&2
      exit 3
    fi
    echo "ci-bench.sh: $found benchmarks, results in $out"
    ;;
  *) echo "unknown mode: $mode" >&2; exit 2 ;;
esac
