# Contributing

## The short version

Every change arrives as a PR from a same-repo branch, passes the required
checks (`core checks`, `gitleaks`, `warden gate`), and carries a committed
pre-PR review attestation (`.warden/memory/attest/`). Merge only on green —
no exceptions, including for maintainers (enforce_admins is on).

## Commands

- `./scripts/ci-go.sh fmt|vet|build|test|vuln|lint` — the core checks, run
  across **every** Go module in the repo (nested adapter modules included;
  zero modules found is a failure, not a pass).
- `.warden/bin/warden verify --scope core` — the attested verify, same
  commands via the policy in `repo.yaml`.
- `.warden/bin/warden explain` — orientation: components, risk tiers,
  rules, protected paths.

## Benchmarks

Performance is part of this library's contract — it runs inside adopting
services' request paths. Hot-path packages (Baggage codec, `emit.Record`,
in-flight bucketing, engine `Compute`, baseline fit) carry Go benchmarks.

- `./scripts/ci-bench.sh run out.txt` runs every benchmark in every module
  (`BENCH_TIME`/`BENCH_COUNT` env to tune; CI uses 1x/6 for cheap
  statistics, local precision runs want the Go defaults).
- CI's advisory `benchmarks` job compares PR vs main with `benchstat` and
  writes the delta to the job summary. It reports "0 benchmarks" honestly
  until the first one lands. A regression on a hot path needs a stated
  reason in the PR body; the job ratchets toward required as baselines
  stabilize.

## Commits

Conventional commits: `type: summary (tracked-item-id)` — the id lives in
the commit header and the PR body, never in code comments.

## Design decisions

Irreversible decisions live in `docs/adr/`. Code that contradicts an
accepted ADR is a bug in one of the two; fix the mismatch, never paper over
it. ADR amendments are their own PRs.
