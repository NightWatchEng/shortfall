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
# --- In the PR gate (no build tag) ---

# Tracker publish stall vs tracked-set size
go test -run '^$' -bench BenchmarkTrackerPublishScale -benchmem -benchtime 1s -count 6 -cpu 1 ./emit

# Pre-existing single-goroutine micro-benchmarks
go test -run '^$' -bench 'BenchmarkRecordAccept|BenchmarkRecordSuppressed|BenchmarkAgeBucketFor|BenchmarkTrackerPublish10k' \
    -benchmem -benchtime 1s -count 6 -cpu 1 ./emit
go test -run '^$' -bench 'BenchmarkEncodeVC|BenchmarkDecodeVC|BenchmarkValueContextValidate' \
    -benchmem -benchtime 1s -count 6 -cpu 1 ./biz
go test -run '^$' -bench 'BenchmarkCompute$' -benchmem -benchtime 1x -count 6 ./engine

# --- NOT in the gate: needs -tags benchload. See "What the gate runs" ---

# Core-scaling curves
go test -tags benchload -run '^$' \
    -bench 'BenchmarkRecordParallel$|BenchmarkRecordParallelSuppressed$|BenchmarkTrackerTrackDoneParallel$|BenchmarkParallelSeqFloor$' \
    -benchmem -benchtime 1s -count 6 -cpu 1,2,4,8,18 ./emit

# engine.Compute scaling series (peaks around 2.7 GB resident at the 2M step)
go test -tags benchload -run '^$' -bench BenchmarkComputeScale \
    -benchmem -benchtime 1x -count 6 -timeout 60m ./engine

# Backend pathologies, and the healthy control they are compared against
go test -tags benchload -run '^$' \
    -bench 'BenchmarkRecordHealthyBackend|BenchmarkRecordSlowExporter|BenchmarkRecordFailingExporter' \
    -benchmem -benchtime 2s -count 6 -timeout 60m ./emit
go test -tags benchload -run '^$' -bench BenchmarkTrackerTrackDoneUnderPublish \
    -benchmem -benchtime 2s -count 6 -timeout 60m ./emit
```

`-cpu N` sets `GOMAXPROCS` to N, and `b.RunParallel` spawns N goroutines to
match — so one flag moves cores and concurrency together, and `benchstat`
folds the `-N` name suffixes into a single comparison. No benchmark here
hand-rolls the goroutines it measures (the tracker-starvation benchmark
runs one publisher goroutine alongside, which is the contention it exists
to measure), and no assertion in any of them depends on one goroutine
outrunning another.

## `emit.Record` — does it scale with cores?

**Barely.** Throughput tops out near 1M accepted outcomes per second at
eight cores — 2.2× what one core does — and falls back past that. Every
`Record` call takes the emitter's single mutex to be admitted and de-duped,
and an *accepted* one takes it a second time to append its metric points,
so past a handful of concurrent callers the goroutines are queueing rather
than working.

### Accepted outcomes (`BenchmarkRecordParallel`)

Every call mints a distinct de-dup key, so every call takes the full accept
path: validate, build the outcome, take the lock, buffer it, then build the
two ADR-0004 label maps.

| `-cpu` | ns/op | spread | throughput | speedup vs 1 core | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 2258 | ±16% | 443k/s | 1.00× | 3644 | 17 |
| 2 | 1462 | ±8% | 684k/s | 1.54× | 2964 | 17 |
| 4 | 1062 | ±11% | 942k/s | 2.13× | 2914 | 17 |
| 8 | 1044 | ±10% | 958k/s | 2.16× | 2852 | 17 |
| 18 | 1351 | ±18% | 740k/s | 1.67× | 2828 | 17 |

Eighteen cores buy 1.67× the throughput of one. The curve peaks at eight
and turns over after it: adding cores past the plateau costs throughput.

Three caveats that belong with this table rather than in the limits
section, because they change how it reads:

- At `-cpu 1` the background flusher shares the single processor with the
  caller, so that row includes the amortised cost of exporting batches.
  From `-cpu 2` upward the flusher runs elsewhere. Read row 1 as *total CPU
  cost per outcome* and the later rows as *latency seen by the caller*.
- This benchmark takes one shared atomic increment per call to mint a
  distinct de-dup key. `BenchmarkParallelSeqFloor` measures exactly that
  loop and nothing else: 1.83 ns at `-cpu 1`, 13.0–13.9 ns from `-cpu 2`
  upward. Subtract it — it is about 1% of the numbers above, which is why
  it was left in rather than engineered away. The other parallel
  benchmarks on this page count in goroutine-local variables and carry no
  such floor.
- The ±10–18% spreads are real, not sampling sloppiness. Lock hand-off
  under contention is genuinely variable, and `ns/op` is a mean — see
  [Limits](#limits) on tail latency. Between two full runs of this sweep
  the `-cpu 18` figure moved from 1180 to 1351 ns (14%) with no code
  change, so treat any single absolute value here as ±15% and rely on the
  shape, which reproduced exactly across both runs.

### Suppressed retries (`BenchmarkRecordParallelSuppressed`)

Every goroutine re-records one already-seen key: take the lock, hit the
two-generation de-dup set, return. This is the emitter's lock cost with
none of the work.

| `-cpu` | ns/op | spread | throughput | speedup vs 1 core | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 719 | ±5% | 1.39M/s | 1.00× | 288 | 3 |
| 2 | 434 | ±2% | 2.30M/s | 1.65× | 288 | 3 |
| 4 | 259 | ±25% | 3.86M/s | 2.77× | 289 | 3 |
| 8 | 229 | ±22% | 4.37M/s | 3.14× | 292 | 3 |
| 18 | 254 | ±9% | 3.94M/s | 2.83× | 304 | 3 |

The suppressed path scales better than the accept path (3.1× against 2.2×)
because it holds the lock for less time and allocates far less — but it
turns over at the same place, past eight cores, which is where the lock
rather than the work becomes the limit.

It is also not free. A de-duplicated retry still costs ~720 ns and three
allocations: the `ValueContext` is decoded out of the request context and
the de-dup key string is built before the emitter has any way to know the
call is a duplicate. Retry storms are not cheap on this path.

### `BenchmarkRecordAccept`, corrected

The single-goroutine `BenchmarkRecordAccept` used to report 3 allocs/op for
a path that allocates **17**. It rotated over a pool of 49152 de-dup keys
while the emitter's two-generation set retains up to 131072, so after the
first pass every call found its key already remembered: under 4% of a
`-benchtime 1s` run actually took the accept path, and the published figure
was the integer mean of ~96% suppressed calls at 3 allocations and ~4%
accepted calls at 17.

It now forgets the de-dup set between passes, so every call is accepted, and
it **asserts conservation** — `delivered == b.N`, the same check
`BenchmarkRecordParallel` carries. That is an assertion rather than a
comment because the benchmark drifted into measuring the wrong path
precisely because nothing checked which path it took.

## `emit.InFlightTracker` — the cost of contention

### `Track` + `Done` from every consumer at once

`BenchmarkTrackerTrackDoneParallel`: each goroutine tracks a message onto
its own private id, then completes it. One shared tracker, one shared
mutex, no publishing.

| `-cpu` | ns/op per pair | spread | throughput | vs 1 core | allocs/op |
|---:|---:|---:|---:|---:|---:|
| 1 | 85.7 | ±1% | 11.7M/s | 1.00× | 0 |
| 2 | 103 | ±2% | 9.70M/s | 0.83× | 0 |
| 4 | 85.8 | ±2% | 11.7M/s | 1.00× | 0 |
| 8 | 135 | ±2% | 7.44M/s | 0.64× | 0 |
| 18 | 124 | ±3% | 8.09M/s | 0.69× | 0 |

The in-flight set is sharded by message-id hash across 32 cache-line-padded
mutexes, so consumers contend only when their ids land on the same shard.
The shape that used to sit here was **negative scaling** — a single
`sync.Mutex` in front of one map put eighteen consumers at 19% of one
consumer's throughput (287 ns/pair at `-cpu 18` against 55.9 at `-cpu 1`).
Now the curve is a band: 86–135 ns/pair everywhere on the sweep, worst-case
throughput 7.4M pairs/s instead of 3.5M.

The sharding is not free at one core: a single-threaded caller pays the id
hash, the exponent-pin lookup, and two atomics on the item count, which
moved the uncontended pair from 55.9 ns to 85.7. That trade is taken
deliberately — the tracker's deployment shape is many queue consumers, and
the contended edge of the band is what an incident-sized backlog drains
through. A same-id pathological stream lands on one shard and degrades to
the old single-mutex behavior, no worse.

The absolute numbers remain far above any payments queue this library is
aimed at; the sweep is sizing information. Publish still stalls everything
briefly — the next two tables.

### `Publish` — how long the mutex is held

`BenchmarkTrackerPublishScale`: `Publish` takes every shard lock at once
and holds all of them while it aggregates — value and count must come from
one consistent snapshot (ADR-0012) — so its duration is still the
worst-case stall a consumer goroutine takes on `Track` or `Done` when a
publish lands.

| tracked items | ns/op | spread | wall | per item | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 32,630 | ±2% | 32.6 µs | 32.6 ns | 368 | 6 |
| 10,000 | 312,300 | ±1% | 312 µs | 31.2 ns | 400 | 8 |
| 100,000 | 2,907,000 | ±1% | 2.91 ms | 29.1 ns | 407 | 8 |

Linear, at roughly 31 ns per tracked item, and it allocates almost
nothing; locking 32 mutexes instead of one is noise at these depths.
Multiply by your publish interval to get the duty cycle: a 100k-deep
backlog published every second stalls the tracker for 2.9 ms in every
1000 ms — 0.3% of the time. That is small. It is not small if you publish
every 10 ms.

The sink for this series is a counting emitter, so these are the tracker's
own snapshot costs with none of the `Std` emitter's buffering folded in.
The older `BenchmarkTrackerPublish10k` measures the pair together and lands
at 317 µs — the emitter adds no measurable time at this size, only
memory (11.5 kB and 78 allocations against 400 B and 8).

### `Track`/`Done` against a publisher that never stops

`BenchmarkTrackerTrackDoneUnderPublish` (tagged `benchload`) is the upper
bound on interference: a publisher goroutine spinning on `Publish` over a
10,000-item set, while consumers try to `Track` and `Done`.

| configuration (`-cpu 18`) | ns/op per `Track`+`Done` | spread | vs no publisher |
|---|---:|---:|---:|
| no publisher | 124 | ±3% | 1× |
| publisher spinning over 10,000 items | 5,000–5,800 | ±6% | **~44×** |

A consumer's `Track` goes from ~124 ns to roughly 5.5 µs when it has to
queue behind a publisher that is always inside its all-shard snapshot —
down from the ~30 µs the single-mutex tracker paid here, because a
consumer now waits only for the snapshot passes that overlap its shard
acquisition rather than for every contender ahead of it in one queue.
The completed-publish count still swings run to run, which is why the
figure is a range and the benchmark stays out of the regression gate.
Take the order of magnitude, not the digits.

This is not a production cadence and must not be read as one — a real
deployment publishes on an interval measured in seconds, where the duty
cycle above applies and the interference is a fraction of a percent. It is
here because it bounds the worst case, and because it says which knob
matters: the publish **interval** relative to the `Publish` duration for
your backlog depth, not the number of consumers.

## `engine.Compute` — what shape is it?

`Compute` assembles the four legs for an incident window. The gate
benchmark measures 50k and 200k events; the series below straddles those
across about 2.3 orders of magnitude, from 10k to 2M, so a reader pricing a
window they have not measured can interpolate instead of guessing.

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

**Memory is the binding constraint, not time — but size on residency, not
on this column.** The 4.35 GB at 2M is `B/op`: bytes handed to the allocator
over the whole `Compute` call, which is about 1.0 GB/s of churn for the
collector to chase. It is not how much the process holds at once, and the
two are not related by any fixed ratio — lifetime and `GOGC` decide that.
Measured on the reference host, the 2M step peaks at **2.7 GB resident**
against a live heap that tops out near **1.06 GB** (`GODEBUG=gctrace=1`,
heap goal ~2.1 GB). So size a host running `shortfall impact` over a large
window on peak RSS: 4 GB clears 2M events with room to spare. Read `B/op`
as allocator pressure, not as a memory requirement.

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
three run `Record` on the accept path through the **same harness** — a
16384-entry event buffer (`backendBuffer`) and a 1 ms background flusher —
and the only thing that varies is what the exporter does with the batch.
The healthy row is its own benchmark, `BenchmarkRecordHealthyBackend`,
which exists solely to hold that harness constant; comparing against the
core-scaling table instead would confound the backend with a 64× larger
buffer, and the drop figures below are as much a property of the buffer
bound as of the backend.

| backend | ns/op | outcomes lost | counted as | flushes completed | allocs/op |
|---|---:|---:|---|---:|---:|
| healthy (`BenchmarkRecordHealthyBackend`) | 1204 | 0% | — | 2248 | 17 |
| 25 ms per event batch (`BenchmarkRecordSlowExporter`) | 786 | 48.7% | `reason="overflow"` | 95 | 10 |
| refuses every batch (`BenchmarkRecordFailingExporter`) | 1130 | 99.97% | `reason="export"` | 2347 | 17 |

Read the second row carefully: against a backend that has gone 25 ms slow,
`Record` gets **35% faster** than against a healthy one. That is not good
news. The flusher completed 95 flushes instead of 2248 because it was stuck
in the exporter; the buffer filled; and the overflow path returns before it
builds either label map — ten allocations instead of seventeen — so the
emitter gets cheaper exactly as it starts losing data. A latency dashboard
would show this incident as an improvement.

The counter is the signal, not the timing. Alert on
`biz_dropped_events_total`; a `Record` latency graph will not tell you your
telemetry is on fire.

The failing-backend row is the gentler failure: batches are refused
immediately, so the buffer keeps draining (2347 flushes, more than the
healthy run), the caller keeps paying close to full price, and essentially
every outcome is lost to `reason="export"` rather than to overflow. Both are
counted; neither is silent.

**Scale the loss figure, do not quote it.** 48.7% is what a 16384-entry
buffer loses to a 25 ms backend at roughly 800k accepted outcomes/sec. A
larger `WithBufferSize` buys proportionally more time before the first
drop and nothing after it — the buffer sets how long an outage you can
absorb, not whether you absorb it.

## Sustained load — what happens after the benchmark ends

Benchmarks answer "how fast is one call". `TestSustainedLoadReconciles`
(`testkit/load_test.go`) answers what a run that keeps going does:
seeded checkout-ledger transactions replay through a real `emit.Std`
(50 ms background flush) plus an `InFlightTracker` (25 ms publish loop)
at a configurable offered rate, and at the end the sink must reconcile
exactly — every issued outcome event received once, value sums equal,
`biz_dropped_events_total` zero on the wire, nothing left in flight.
The run's second half is asserted flat: median heap, goroutine count,
and the metric series count must not grow with transaction count
(cardinality protection is a library guarantee, and this is where it is
tested rather than asserted). `TestSustainedLoadCatchesSeededLeak`
keeps those assertions honest by seeding a pinned goroutine-and-buffer
leak and requiring the harness to flag it.

Two configurations, so nobody reads a seconds-long result as a soak:

| configuration | rate | duration | workers | where it runs |
|---|---:|---:|---:|---|
| smoke (default) | 1,200 tx/s | 3 s | 8 | every `go test ./testkit`, so every PR gate |
| soak | `SHORTFALL_LOAD_RATE` | `SHORTFALL_LOAD_SECONDS` | `SHORTFALL_LOAD_WORKERS` | release checks, run by hand |

A soak invocation looks like:

```sh
SHORTFALL_LOAD_RATE=2000 SHORTFALL_LOAD_SECONDS=14400 \
  go test ./testkit -run TestSustainedLoadReconciles -timeout 5h -v
```

The smoke configuration on the reference machine reconciles ~3,600
transactions at 1,197/s achieved against 1,200/s offered, with
Record-call-site latency p50 ≈ 2.7 µs and p99 ≈ 11 µs. A smoke run
catches reconciliation breaks and gross leaks, not slow ones; the soak
exists for the slow ones.

## What the gate runs, and what it does not

CI's `benchmarks` job is **advisory** — it is not in the required-check
set. It runs `scripts/ci-bench.sh run` on the PR head and again on `main`,
compares the two with `benchstat`, and writes the delta into the job
summary. `ci-bench.sh` discovers benchmarks with `go test -list` across
every module, and runs them at `BENCH_TIME=1x`, `BENCH_COUNT=6`.

`ci-bench.sh count` reports 13 benchmarks on this branch, up from 12.
**Exactly one** of the benchmarks written for this page is in the gate:
`BenchmarkTrackerPublishScale`. Every other one is behind the `benchload`
build tag and therefore invisible to `go test -list`.

That is a deliberate and, for the concurrency benchmarks, a
counter-intuitive choice, so here is the evidence for it.

`BENCH_TIME=1x` means `b.N` is 1. `b.RunParallel` responds by spawning
`GOMAXPROCS` goroutines to perform a single iteration between them, so what
the gate would compare PR-against-`main` is goroutine wake-up, not `Record`.
Running this package twice at the CI settings over an **identical tree** and
handing both files to `benchstat`:

| Benchmark | B/op spread, run vs identical run | allocs/op observed | allocs/op the path actually costs |
|---|---:|---:|---:|
| `BenchmarkRecordParallel` | ±12% / ±7% | 68–85 | 17 |
| `BenchmarkParallelSeqFloor` | ±352% / ±454% | 42 ±40% | 0 |
| `BenchmarkRecordParallelSuppressed` | ±74% / ±95% | 49–66 | 3 |
| `BenchmarkTrackerTrackDoneParallel` | ±0% / ±239% | 42 ±33% | 0 |
| `BenchmarkTrackerPublishScale` | ±0% / ±0%, every sample equal | 7 / 9 / 9 | 7 / 9 / 9 |

On one such identical-tree pair `benchstat` reported `ParallelSeqFloor` as a
**32% regression at p=0.041** — a significant finding against a diff that
did not exist. A benchmark that manufactures regressions is worse than no
benchmark: it trains reviewers to ignore the job. So the four
`RunParallel` benchmarks are tagged out, and their curves are read from the
explicit `-cpu` sweep in the reproduce block, where `b.N` is large enough
for `RunParallel` to measure the work instead of the spawn.

`TrackerPublishScale` is a serial loop, was bit-identical across the same
pair of runs, and stays in the gate. It costs the job about 0.2 s.

The full exclusion list and the reason for each:

| Benchmark | Why it is out of the gate |
|---|---|
| `BenchmarkRecordParallel` | `RunParallel` at `b.N=1` measures goroutine spawn; demonstrated false positives (above) |
| `BenchmarkRecordParallelSuppressed` | Same |
| `BenchmarkTrackerTrackDoneParallel` | Same |
| `BenchmarkParallelSeqFloor` | Same — and it is a harness-calibration row, not a product path |
| `BenchmarkComputeScale` | Wall clock: the four-size series takes ~42 s at `-count 6`, and the gate runs it twice (PR head and main) |
| `BenchmarkRecordHealthyBackend` | `RunParallel`, plus it exists only as the control for the two rows below |
| `BenchmarkRecordSlowExporter` | Spends its time waiting on a 25 ms-per-batch backend |
| `BenchmarkRecordFailingExporter` | Same |
| `BenchmarkTrackerTrackDoneUnderPublish` | Measures lock starvation, which is inherently high-variance (publish count varied ±34%) |

Honesty note about the pre-existing set: `b.N=1` is hard on several of the
benchmarks that were already there. `BenchmarkRecordSuppressed` shows
±328% on `sec/op` and ±4926% on `B/op` across the same identical-tree pair.
That is not introduced here and not fixed here — but a reader should not
take the gate's stability for granted on the strength of this page.

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
contention has a long tail, and the ±10–25% within-run spreads on the
contended rows are a hint of it. If your SLO is a p99, nothing on this page
bounds it.

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

**The tagged benchmarks are compiled but not run.** `ci-go.sh vet`
type-checks every module with `-tags "benchload integration"`, so a refactor
of `engine.Compute`, `memq`, or the emitter that breaks a tagged file fails
the gate. What CI does not do is *run* them — that is still the price of
keeping them out of the gate, and a change that makes one slower, or wrong
in a way that still compiles, is invisible until someone reruns the commands
above. `go build -tags` would not have bought even the compile: every tagged
file here is a `_test.go`, and `go build` only compiles non-test files.

**One thing here was optimised, after its benchmark landed.** ADR-0015
orders correctness, then readability, then the benchstat comparison, and
says the benchmark lands before the optimisation. The tracker's negative
scaling was first reported on this page unfixed; the sharded tracker then
followed with the before/after sweep in its PR. Everything else on the
page is reported, not tuned.

## Single-goroutine reference figures

For completeness, the pre-existing micro-benchmarks at `-cpu 1`. These are
the costs of the small pieces, not of the paths that contain them.

| Path | ns/op | spread | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `emit.Record`, de-duplicated retry (`BenchmarkRecordSuppressed`) † | 453 | ±1% | 288 | 3 |
| `emit.Record`, accepted (`BenchmarkRecordAccept`) † | 1,525 | ±2% | 3,265 | 17 |
| `emit.AgeBucketFor` | 0.24 | ±11% | 0 | 0 |
| `emit.InFlightTracker.Publish`, 10k items (`BenchmarkTrackerPublish10k`) | 316,700 | ±1% | 11,485 | 78 |
| `biz` ValueContext encode (`BenchmarkEncodeVC`) | 133 | ±3% | 112 | 3 |
| `biz` ValueContext decode (`BenchmarkDecodeVC`) | 195 | ±16% | 176 | 1 |
| `biz.ValueContext.Validate` | 335 | ±6% | 0 | 0 |

† These two rows were re-measured together on 2026-08-30, when
`BenchmarkRecordAccept` was corrected (see
[above](#benchmarkrecordaccept-corrected)). **They are not comparable to the
rest of this table**: the re-measurement ran under concurrent load, not the
idle laptop the methodology specifies, and not on the machine the other rows
came from. `BenchmarkRecordSuppressed` is re-stated from the same run as a
calibration point — unchanged code, previously published at 515 ns, measured
here at 453.

Read two things from the pair and nothing else. **The ratio**: an accepted
call costs about 3.4× a de-duplicated one, which held at 3.3× on a quieter
run, so it is the figure that survives the host. **The allocation columns**:
17 against 3, and 3,265 B against 288, deterministic and identical across
runs. The absolute nanoseconds are this host on this afternoon.

They do not contradict the README's "2.3 µs on one core", which comes from
`BenchmarkRecordParallel` at `-cpu 1`. That benchmark runs a live background
flusher, so each call carries its amortised share of the export;
`BenchmarkRecordAccept` uses `WithFlushInterval(0)` and drains with the timer
stopped, so it prices `Record` itself. Two questions, two numbers: what one
call costs the caller, and what one call costs the process.
