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
  until the first one lands. A regression on a hot path should carry a
  stated reason in the PR body — a review convention, not a mechanical
  gate, since the job is advisory; it ratchets toward required as baselines
  stabilize.

## Commits

Conventional commits: `type: summary (tracked-item-id)`, or
`type: summary (no-bead: reason)` when no tracked item applies — the id
lives in the commit header and the PR body, never in code comments. Types:
feat, fix, docs, chore, test, refactor, perf, ci, build, revert — no
scopes. Title cap: 100 bytes (bytes, not characters, so macOS and CI
agree); squash merge appends ` (#N)` on main, which sits outside the cap.

Enforcement: PR titles are linted in CI (`pr title lint`) because squash
merge turns the title into the commit header on main. Install the matching
local hook once per clone:

```sh
git config core.hooksPath .githooks
```

## Releases

Tags drive everything. `v*` tags trigger the `release` workflow: goreleaser
builds `cmd/shortfall` binaries (linux/darwin × amd64/arm64) with checksums
and a changelog, attached to the GitHub release. A tag with a semver
pre-release suffix (`v0.3.0-rc1`) is marked pre-release; a plain `v0.x.y`
publishes as a normal release — v0.x instability is the README's semver
policy, not a release flag. Library consumers just `go get` the tag.
Dry-run rehearsal: run the `release` workflow manually (workflow_dispatch)
— it builds a snapshot and uploads `dist/` as an artifact without tagging
or publishing.

## Tests

Table-driven, per ADR-0007 (founder mandate): named cases, `t.Run`
subtests, one uniform assertion body. Declared exceptions only — fuzz
targets, property loops, golden fences, end-to-end scenario tests,
benchmarks. Where the error contract matters (the registry's actionable
messages), rejection tables assert the error names the offending field —
not merely that an error occurred. The pre-PR review flags departures.

## Architecture diagrams

`docs/architecture/` holds the C4 model and money-path sequences as
Mermaid, rendering natively on github.com. **Definition of Done:** a PR
that changes the architecture (new component, moved boundary, new signal
path) updates the affected diagram in the same PR. No GitHub Pages on
this repo — depending on plan it is unavailable or publicly served for a
private repo; either way it stays off.

## Design decisions

Irreversible decisions live in `docs/adr/`. Code that contradicts an
accepted ADR is a bug in one of the two; fix the mismatch, never paper over
it. ADR amendments are their own PRs.
