#!/bin/sh
# Benchmark gate: run every benchmark in every module at a given git ref,
# writing results to a file benchstat can compare.
#
#   scripts/ci-bench.sh run <out-file>     # bench the CURRENT tree
#   scripts/ci-bench.sh count              # print number of benchmarks found
#
# Performance is part of this library's contract (it runs inside adopting
# services' hot paths), so the CI job compares PR vs main with benchstat and
# surfaces the delta. The job is ADVISORY until baselines stabilize — but it
# is honest: when no benchmarks exist it says so in its own output instead
# of printing a green nothing, and the count mode lets the job annotate
# exactly how many benchmarks were compared.
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

count_benchmarks() {
  total=0
  for modfile in $modules; do
    dir="$root/$(dirname "$modfile")"
    n="$(cd "$dir" && go test -list 'Benchmark.*' ./... 2>/dev/null | grep -c '^Benchmark' || true)"
    total=$((total + n))
  done
  echo "$total"
}

case "$mode" in
  count)
    count_benchmarks
    ;;
  run)
    out="$2"
    : > "$out"
    found=0
    for modfile in $modules; do
      dir="$root/$(dirname "$modfile")"
      n="$(cd "$dir" && go test -list 'Benchmark.*' ./... 2>/dev/null | grep -c '^Benchmark' || true)"
      [ "$n" -eq 0 ] && continue
      found=$((found + n))
      echo "==> bench: ${modfile%go.mod} ($n benchmarks)"
      (cd "$dir" && go test -run '^$' -bench . -benchmem \
        -benchtime "$BENCH_TIME" -count "$BENCH_COUNT" ./...) >> "$out"
    done
    if [ "$found" -eq 0 ]; then
      echo "ci-bench.sh: no benchmarks found in any module — nothing to compare" >&2
      # Truthful no-op: the CALLER decides whether that is acceptable.
      # Exit 3 distinguishes "none found" from "ran and wrote results" (0)
      # and from infra failure (2).
      exit 3
    fi
    echo "ci-bench.sh: $found benchmarks, results in $out"
    ;;
  *) echo "unknown mode: $mode" >&2; exit 2 ;;
esac
