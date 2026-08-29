# Performance

shortfall runs inside a payments request path. `emit.Record` is called on
every stage transition your services make, and `emit.InFlightTracker` sits
on the queue-consumer hot path, so their cost is part of the library's
contract rather than an implementation detail.

This page is what that costs, measured. It answers four questions an
adopter has to answer before putting the library in front of production
traffic:

- Does `Record` get faster when you give it more cores?
- What does the in-flight tracker cost when every consumer goroutine hits
  it at once?
- What shape does `engine.Compute` have as an incident window grows?
- What happens to the caller when the telemetry backend goes slow or
  starts refusing writes?

Every number below is reproducible with one of the commands in the block
under [How these numbers were produced](#how-these-numbers-were-produced).
Where a number does not apply, [Limits](#limits) says so — read that
section before sizing anything.

## How these numbers were produced

| | |
|---|---|
| Machine | Apple M5 Pro, 18 cores, 48 GB, macOS 26.6.2 |
| Go | go1.27.0 darwin/arm64 |
| Default `GOMAXPROCS` | 18 |
| State | idle laptop, on AC power, no other load |
| Samples | `-count 6`, `-benchtime 1s` for micro-benchmarks, `-benchtime 1x` for the `engine.Compute` series |
| Reported | the **median** of the six samples; the spread column is (max − min) / median |

Absolute numbers are host-specific and will not reproduce on your
hardware. The **shapes** — where a curve flattens, where it turns over,
whether growth is linear — are the part that transfers, and they are what
the tables are arranged to show.

Reproduce the whole page, from the repository root:

```sh
# Core-scaling curves (in the gate)
go test -run '^$' \
    -bench 'BenchmarkRecordParallel$|BenchmarkRecordParallelSuppressed$|BenchmarkTrackerTrackDoneParallel$|BenchmarkParallelSeqFloor$' \
    -benchmem -benchtime 1s -count 6 -cpu 1,2,4,8,18 ./emit

# Tracker publish stall vs tracked-set size (in the gate)
go test -run '^$' -bench BenchmarkTrackerPublishScale -benchmem -benchtime 1s -count 6 -cpu 1 ./emit

# Single-goroutine micro-benchmarks (in the gate)
go test -run '^$' -bench 'BenchmarkRecordAccept|BenchmarkRecordSuppressed|BenchmarkAgeBucketFor|BenchmarkTrackerPublish10k' \
    -benchmem -benchtime 1s -count 6 -cpu 1 ./emit
go test -run '^$' -bench 'BenchmarkEncodeVC|BenchmarkDecodeVC|BenchmarkValueContextValidate' \
    -benchmem -benchtime 1s -count 6 -cpu 1 ./biz
go test -run '^$' -bench 'BenchmarkCompute$' -benchmem -benchtime 1x -count 6 ./engine

# Load benchmarks — NOT in the gate, see "What the gate runs" below
go test -tags benchload -run '^$' -bench BenchmarkComputeScale \
    -benchmem -benchtime 1x -count 6 -timeout 60m ./engine
go test -tags benchload -run '^$' \
    -bench 'BenchmarkRecordSlowExporter|BenchmarkRecordFailingExporter|BenchmarkTrackerTrackDoneUnderPublish' \
    -benchmem -benchtime 2s -count 6 -timeout 60m ./emit
```

`-cpu N` sets `GOMAXPROCS` to N, and `b.RunParallel` spawns N goroutines to
match — so one flag moves cores and concurrency together, and `benchstat`
folds the `-N` name suffixes into a single comparison. No benchmark here
spawns goroutines by hand, and no assertion in any of them depends on one
goroutine outrunning another.

## `emit.Record` — does it scale with cores?

**Barely.** Throughput tops out near 1M accepted outcomes per second at
eight cores — 2.2× what one core does — and falls back past that. Every
`Record` call takes the emitter's single mutex twice, once for admission
and de-dup and again to append the metric points, so past a handful of
concurrent callers the goroutines are queueing rather than working.

### Accepted outcomes (`BenchmarkRecordParallel`)

Every call mints a distinct de-dup key, so every call takes the full accept
path: validate, build the outcome, take the lock, buffer it, then build the
two ADR-0004 label maps.

| `-cpu` | ns/op | spread | throughput | speedup vs 1 core | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 2187 | ±11% | 457k/s | 1.00× | 3664 | 17 |
| 2 | 1488 | ±5% | 672k/s | 1.47× | 2960 | 17 |
| 4 | 1160 | ±8% | 862k/s | 1.89× | 2923 | 17 |
| 8 | 1002 | ±15% | 998k/s | 2.18× | 2868 | 17 |
| 18 | 1180 | ±3% | 847k/s | 1.85× | 2789 | 17 |

Eighteen cores buy 1.85× the throughput of one. The curve peaks at eight
and turns over after it: adding cores past the plateau costs throughput.

Three caveats that belong with this table rather than in the limits
section, because they change how it reads:

- At `-cpu 1` the background flusher shares the single processor with the
  caller, so that row includes the amortised cost of exporting batches.
  From `-cpu 2` upward the flusher runs elsewhere. Read row 1 as *total CPU
  cost per outcome* and the later rows as *latency seen by the caller*.
- This benchmark takes one shared atomic increment per call to mint a
  distinct de-dup key. `BenchmarkParallelSeqFloor` measures exactly that
  loop and nothing else: 1.75 ns at `-cpu 1`, 13–15 ns from `-cpu 2`
  upward. Subtract it — it is 1% of the numbers above, which is why it was
  left in rather than engineered away. The other parallel benchmarks on
  this page count in goroutine-local variables and carry no such floor.
- The ±15% spread at `-cpu 8` is real, not sampling sloppiness. Lock
  hand-off under contention is genuinely variable, and `ns/op` is a mean —
  see [Limits](#limits) on tail latency.

### Suppressed retries (`BenchmarkRecordParallelSuppressed`)

Every goroutine re-records one already-seen key: take the lock, hit the
two-generation de-dup set, return. This is the emitter's lock cost with
none of the work.

| `-cpu` | ns/op | spread | throughput | speedup vs 1 core | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 725 | ±3% | 1.38M/s | 1.00× | 288 | 3 |
| 2 | 420 | ±3% | 2.38M/s | 1.73× | 288 | 3 |
| 4 | 231 | ±4% | 4.33M/s | 3.14× | 289 | 3 |
| 8 | 184 | ±19% | 5.44M/s | 3.94× | 292 | 3 |
| 18 | 234 | ±13% | 4.27M/s | 3.10× | 305 | 3 |

The suppressed path scales better than the accept path (3.9× against 2.2×)
because it holds the lock for less time and allocates far less — but it
turns over at the same place, past eight cores, which is where the lock
rather than the work becomes the limit.

It is also not free. A de-duplicated retry still costs ~725 ns and three
allocations: the `ValueContext` is decoded out of the request context and
the de-dup key string is built before the emitter has any way to know the
call is a duplicate. Retry storms are not cheap on this path.

### A note on `BenchmarkRecordAccept`

The older single-goroutine `BenchmarkRecordAccept` reports 3 allocs/op. The
accept path allocates **17**. That benchmark rotates over a fixed pool of
4096 contexts × 3 stages × 4 results = 49152 de-dup keys, and the emitter's
two-generation set retains up to 131072 recent keys. The pool is smaller
than the retention window, so once it has been walked once, every
subsequent call finds its key already remembered: only the first 49152
iterations of a run take the accept path. At `-benchtime 1s` that run is
about 1.4M iterations, so under 4% of it measures acceptance. The
reported `3 allocs/op` is the integer mean of ~96% suppressed calls at 3
allocations and ~4% accepted calls at 17.

It is still a stable, useful baseline for the regression gate — it just
measures something narrower than its name says. The honest accept figures
are the `BenchmarkRecordParallel` table above. Fixing the older benchmark
is tracked separately; it is not done here, because correcting it would
move a gate baseline in the same PR that introduces the numbers proving it
needs correcting.

## `emit.InFlightTracker` — the cost of contention

### `Track` + `Done` from every consumer at once

`BenchmarkTrackerTrackDoneParallel`: each goroutine tracks a message onto
its own private id, then completes it. One shared tracker, one shared
mutex, no publishing.

| `-cpu` | ns/op per pair | spread | throughput | vs 1 core | allocs/op |
|---:|---:|---:|---:|---:|---:|
| 1 | 56.5 | ±1% | 17.7M/s | 1.00× | 0 |
| 2 | 124 | ±5% | 8.10M/s | 0.46× | 0 |
| 4 | 220 | ±2% | 4.55M/s | 0.26× | 0 |
| 8 | 228 | ±2% | 4.38M/s | 0.25× | 0 |
| 18 | 295 | ±5% | 3.39M/s | 0.19× | 0 |

**This is the most important shape on the page, and it is negative
scaling.** The tracker does not merely fail to speed up with more
consumers; it gets slower in absolute terms. Eighteen concurrent consumers
get 19% of the throughput one consumer gets, and the loss is already 2.2×
at two consumers. `InFlightTracker` is a Go map behind a single
`sync.Mutex` with a critical section of a few tens of nanoseconds — short
enough that under contention the cost is dominated by lock hand-off and
cache-line ping-pong rather than by the work, and more contenders means
more of both.

The absolute numbers are still large: 3.4M paired `Track`/`Done` calls per
second is far above any payments queue this library is aimed at. So this is
a shape to know before you size, not a fire to put out. It starts to matter
when the tracked set is also being published — the next two tables.

### `Publish` — how long the mutex is held

`BenchmarkTrackerPublishScale`: `Publish` snapshots the entire tracked set
under the tracker's mutex before emitting anything, so its duration is also
the worst-case stall a consumer goroutine takes on `Track` or `Done` when a
publish lands.

| tracked items | ns/op | spread | wall | per item | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 35,190 | ±11% | 35.2 µs | 35.2 ns | 368 | 6 |
| 10,000 | 354,500 | ±9% | 355 µs | 35.5 ns | 400 | 8 |
| 100,000 | 3,278,000 | ±5% | 3.28 ms | 32.8 ns | 401 | 8 |

Linear, at roughly 34 ns per tracked item, and it allocates almost nothing.
Multiply by your publish interval to get the duty cycle: a 100k-deep
backlog published every second holds the tracker's mutex for 3.3 ms in
every 1000 ms — 0.3% of the time. That is small. It is not small if you
publish every 10 ms.

The sink for this series is a counting emitter, so these are the tracker's
own snapshot costs with none of the `Std` emitter's buffering folded in.
The older `BenchmarkTrackerPublish10k` measures the pair together and lands
at the same 354 µs — the emitter adds no measurable time at this size, only
memory (11.8 kB and 78 allocations against 400 B and 8).

### `Track`/`Done` against a publisher that never stops

`BenchmarkTrackerTrackDoneUnderPublish` (tagged `benchload`) is the upper
bound on interference: a publisher goroutine spinning on `Publish` over a
10,000-item set, while consumers try to `Track` and `Done`.

| configuration (`-cpu 18`) | ns/op per `Track`+`Done` | spread | vs no publisher |
|---|---:|---:|---:|
| no publisher | 295 | ±5% | 1× |
| publisher spinning over 10,000 items | 31,110 | ±13% | **105×** |

A consumer's `Track` goes from 295 ns to 31 µs when it has to queue behind
a publisher that is always in its critical section. The publish count
itself varied ±34% run to run, which is why this benchmark is kept out of
the regression gate.

This is not a production cadence and must not be read as one — a real
deployment publishes on an interval measured in seconds, where the duty
cycle above applies and the interference is a fraction of a percent. It is
here because it bounds the worst case, and because it says which knob
matters: the publish **interval** relative to the `Publish` duration for
your backlog depth, not the number of consumers.

## `engine.Compute` — what shape is it?

`Compute` assembles the four legs for an incident window. The gate
benchmark measures 50k and 200k events; the series below spans two orders
of magnitude either side of that, so a reader pricing a window they have
not measured can interpolate instead of guessing.

| events | wall clock | per event | B/op | allocs/op | source |
|---:|---:|---:|---:|---:|---|
| 10,000 | 35.9 ms | 3.59 µs | 32.0 MB | 473k | `ComputeScale` (tagged) |
| 50,000 | 198 ms | 3.96 µs | 165 MB | 2.35M | `Compute` (gate) |
| 100,000 | 415 ms | 4.15 µs | 357 MB | 5.55M | `ComputeScale` (tagged) |
| 200,000 | 727 ms | 3.63 µs | 651 MB | 10.0M | `Compute` (gate) |
| 1,000,000 | 2.22 s | 2.22 µs | 2.29 GB | 28.8M | `ComputeScale` (tagged) |
| 2,000,000 | 4.32 s | 2.16 µs | 4.35 GB | 52.2M | `ComputeScale` (tagged) |

**Two million events is 4.3 seconds, not 43.** Doubling 1M to 2M costs
1.95× the time and 1.90× the memory — linear, with no cliff anywhere in the
range. Per-event cost sits between 2.2 µs and 4.2 µs across two orders of
magnitude, and is if anything *lower* at the top of the range than the
bottom.

That last part is unexplained and this series does not isolate it. One
candidate is the generator: it draws customer ids from a pool of 50,000, so
the customers leg's grouping work stops growing once the event count passes
that, while everything else keeps scaling. Treat the flat-to-falling
per-event line as an artefact of this dataset rather than a property of
`Compute`, and size on the linear reading — the safe one.

**Memory is the binding constraint, not time.** 2M events allocates 4.35 GB
in a single `Compute` call — more than the process is likely to have on a
modest instance, and 2.2 GB/s of allocation churn for the collector to
chase. If you are sizing a host to run `shortfall impact` over a large
window, size it on this column.

## Backend pathologies — does back-pressure reach the caller?

**No.** `emit.Std.Flush` releases the emitter's mutex before it calls the
exporter, so a backend that has gone slow blocks the flusher, never the
caller. What a wedged backend costs you is **data**, not latency: the
bounded event buffer stops draining, fills, and `Record` sheds load —
dropping whole observations and counting them on
`biz_dropped_events_total{reason="overflow"}` (ADR-0002).

That property is pinned by a test, not inferred from a benchmark.
`TestSlowBackendReachesRecordAsCountedDropsNotAsBlocking` blocks an
exporter inside its export, waits on a channel until the flusher is
provably in there, then fills the buffer to a known depth and checks the
overflow count is exactly right. Nothing in it sleeps and nothing compares
durations: if `Record` ever did inherit the backend's latency, the test
deadlocks rather than flaking.

Both halves of it were checked against the defect they exist to catch, in
plain and `-race` builds alike. Making the buffer large enough not to
overflow drops the assertion to `overflow = 0, want 64` in both modes;
holding the emitter's mutex across the export — the shape "back-pressure
reaches `Record`" actually takes — hangs the test until the timeout. A
concurrency assertion whose discrimination has not been demonstrated under
both build modes is not evidence, because the race detector changes which
goroutine wins.

The benchmarks put numbers on the trade. All three rows are `-cpu 18`, all
three are `Record` on the accept path, and the only difference is what the
exporter does with the batch.

| backend | ns/op | outcomes lost | counted as | allocs/op |
|---|---:|---:|---|---:|
| healthy (`BenchmarkRecordParallel`) | 1256 | 0% | — | 17 |
| 25 ms per batch (`BenchmarkRecordSlowExporter`) | 563 | 81.4% | `reason="overflow"` | 5 |
| refuses every batch (`BenchmarkRecordFailingExporter`) | 1070 | 99.99% | `reason="export"` | 17 |

Read the second row carefully: against a backend that has gone 25 ms slow,
`Record` is **more than twice as fast** as against a healthy one. That is
not good news. The overflow path returns before it builds either label map
— five allocations instead of seventeen — so the emitter gets cheaper
exactly as it starts losing data. A latency dashboard would show this
incident as an improvement.

The counter is the signal, not the timing. Alert on
`biz_dropped_events_total`; a `Record` latency graph will not tell you your
telemetry is on fire.

The failing-backend row is the gentler failure: batches are refused
immediately, so the buffer keeps draining, the caller keeps paying full
price, and essentially every outcome is lost to `reason="export"` rather
than to overflow. Both are counted; neither is silent.

## What the gate runs, and what it does not

CI's `benchmarks` job is **advisory** — it is not in the required-check
set. It runs `scripts/ci-bench.sh run` on the PR head and again on `main`,
compares the two with `benchstat`, and writes the delta into the job
summary. `ci-bench.sh` discovers benchmarks with `go test -list` across
every module, and runs them at `BENCH_TIME=1x`, `BENCH_COUNT=6`.

`ci-bench.sh count` reports 17 benchmarks on this branch, up from 12: the
five gate-resident benchmarks added for this page cost the job about 0.5 s
of its ~18 s, measured at the CI settings.

Every benchmark quoted on this page is in that comparison **except** these
four, which are behind the `benchload` build tag and are therefore invisible
to `go test -list`:

| Benchmark | Why it is out of the gate |
|---|---|
| `BenchmarkComputeScale` | Several GB of live heap per sample at the top of the series — an out-of-memory kill on a hosted runner, not a slow job |
| `BenchmarkRecordSlowExporter` | Spends its time waiting on a 25 ms-per-batch backend; the variance would manufacture regressions on PRs that changed nothing |
| `BenchmarkRecordFailingExporter` | Same |
| `BenchmarkTrackerTrackDoneUnderPublish` | Measures lock starvation, which is inherently high-variance |

Running them needs `-tags benchload`, as printed in the reproduce block
above. This is a deliberate exclusion recorded here so the gate does not
have to lie about its coverage — but it has a cost, listed in the limits
below.

## Limits

What follows is what these numbers do **not** say. Read it before you size
anything on them.

**One machine.** Apple silicon, macOS, `go1.27.0`, an idle laptop on AC
power. There are no Linux numbers here, no x86 numbers, and nothing
measured inside a container. A pod with a fractional CPU quota behaves
differently from a machine with 18 free cores in ways these curves cannot
predict — CFS throttling in particular interacts badly with the lock
contention the tables above document.

**No tail latency.** Go benchmarks report a **mean** (`ns/op` is total time
divided by iterations). Every figure here is a mean. A mutex under
contention has a long tail, and the ±15–19% run-to-run spread at `-cpu 8`
is a hint of it. If your SLO is a p99, nothing on this page bounds it.

**Discard exporters.** Every emitter benchmark writes to an in-process sink
that counts and forgets. There is no serialization, no TLS, no network, and
no real OTLP, Prometheus, or CloudWatch client in the measured path. Real
exporter cost is additive to everything here and is not characterised.
`adapters/query/promql` carries its own benchmarks for the read side; the
write-side adapters carry none.

**Seconds, not hours.** These are benchmark windows of a few seconds. There
is no steady-state figure, no memory behaviour over a long run, no
observation of what the de-dup set's generation rotation does to the heap
after an hour. A sustained-load harness over `examples/checkout` is tracked
as separate work and is not done.

**`engine.Compute` is measured through an in-memory querier.** The
`memq.Querier` serves events and metric points from Go slices. A real
backend adds network round-trips and its own query planning, which in
practice dominate the times in that table. Read the series as the engine's
own cost, not as time-to-report.

**One dataset shape.** The `Compute` series uses one synthetic incident:
one flow, three currencies, 50k distinct customers, a third of events
succeeding. Cardinality, currency count, and customer count all affect the
grouping work, and none of them are swept.

**Nothing outside `emit` and `engine`.** `propagate/` (Baggage and queue
carriers), the exporters and queriers under `adapters/`, and the
`shortfall` CLI have no concurrency numbers. The `biz` codec figures below
are single-goroutine only.

**The gate runs tests without `-race`.** `scripts/ci-go.sh test` is a plain
`go test ./...`, so the concurrency test described above is exercised
without the detector in CI. It was run under `-race` by hand, including its
discrimination check, but nothing enforces that on every PR.

**The tagged benchmarks are not compiled by CI.** Nothing in the core
verify scope or the benchmark job builds a `benchload`-tagged file, so a
refactor of `engine.Compute`, `memq`, or the emitter can break them and
nobody will learn until someone reruns the commands above. That is the
price of keeping them out of the gate, and it is a real one.

**Nothing here was optimised.** ADR-0015 orders correctness, then
readability, then the benchstat comparison, and says the benchmark lands
before the optimisation. The contention this page documents is reported,
not fixed; no production code changed to produce these numbers.

## Single-goroutine reference figures

For completeness, the pre-existing micro-benchmarks at `-cpu 1`. These are
the costs of the small pieces, not of the paths that contain them.

| Path | ns/op | spread | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `emit.Record`, de-duplicated retry (`BenchmarkRecordSuppressed`) | 515 | ±6% | 288 | 3 |
| `emit.Record`, mixed accept/suppress (`BenchmarkRecordAccept`) | 716 | ±4% | 381 | 3 |
| `emit.AgeBucketFor` | 0.24 | ±11% | 0 | 0 |
| `emit.InFlightTracker.Publish`, 10k items (`BenchmarkTrackerPublish10k`) | 354,100 | ±4% | 11,780 | 78 |
| `biz` ValueContext encode (`BenchmarkEncodeVC`) | 133 | ±3% | 112 | 3 |
| `biz` ValueContext decode (`BenchmarkDecodeVC`) | 195 | ±16% | 176 | 1 |
| `biz.ValueContext.Validate` | 335 | ±6% | 0 | 0 |

`BenchmarkRecordAccept` is labelled "mixed accept/suppress" here rather
than by its name, for the reason given [above](#a-note-on-benchmarkrecordaccept).
