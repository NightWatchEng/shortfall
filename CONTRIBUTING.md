# Contributing

## The short version

Every change arrives as a PR from a same-repo branch, passes the required
checks (`core checks`, `gitleaks`, `warden gate`), and carries a committed
pre-PR review attestation (`.warden/memory/attest/`). Merge only on green —
no exceptions, including for maintainers (enforce_admins is on).

## Who can contribute

Contributing requires write access today. The repository is private, and
the required `warden gate` check authenticates with repository credentials
that a PR from a fork never receives — a fork PR cannot pass the required
checks, so fork PRs are not accepted. That is a pre-flip posture, not a
philosophy: it gets revisited before the repository goes public, in the
same pass that updates the issue templates already pre-positioned for the
flip.

## Commands

- `./scripts/ci-go.sh fmt|vet|build|test|vuln|lint` — the core checks, run
  across **every** Go module in the repo (nested adapter modules included;
  zero modules found is a failure, not a pass). `vuln` and `lint` need no
  setup: govulncheck and golangci-lint are pinned in the script and run
  through `go run` when the pinned binary is not already installed (the
  first such run builds the tool, later ones are cached).
- `.warden/bin/warden verify --scope core` — the attested verify, same six
  commands via the policy in `repo.yaml`, and the same six checks the
  required `core checks` job runs. Running it before opening a PR is the
  cheapest way to find out what that job will say about your code; the job
  additionally installs the pinned linter, a step with no local
  counterpart, so a green verify is not a promise the job goes green.
- `.warden/bin/warden explain` — orientation: components, risk tiers,
  rules, protected paths.

## Benchmarks

Performance is part of this library's contract — it runs inside adopting
services' request paths. Hot-path packages (Baggage codec, `emit.Record`,
in-flight bucketing, engine `Compute`, baseline fit) carry Go benchmarks.

- `./scripts/ci-bench.sh run out.txt` runs every untagged benchmark in
  every module (`BENCH_TIME`/`BENCH_COUNT` env to tune; CI uses 1x/6 for
  cheap statistics, local precision runs want the Go defaults).
- Benchmarks behind the `benchload` build tag are deliberately outside that
  set — either too large for a hosted runner or too variable to compare
  with `benchstat`. `go test -list` never sees them, so they do not RUN in
  CI. They are compiled, though: `ci-go.sh vet` type-checks with
  `-tags "benchload integration"`, so a refactor that breaks a tagged file
  fails the gate rather than waiting for someone to run it by hand. Run them
  with `-tags benchload`; the commands and the reason for each exclusion are
  in [docs/performance.md](docs/performance.md).
- CI's advisory `benchmarks` job compares PR vs main with `benchstat` and
  writes the delta to the job summary. It reports "0 benchmarks" honestly
  until the first one lands. A regression on a hot path should carry a
  stated reason in the PR body — a review convention, not a mechanical
  gate, since the job is advisory; it ratchets toward required as baselines
  stabilize.

## Commits

Conventional commits: `type: summary (tracked-item-id)`, or
`type: summary (no-bead: reason)` when no tracked item applies — the id
lives in the commit header and the PR body, never in code comments. A PR
that closes several tracked items lists every id in the one parens,
single-space separated: `fix: close both (workspace-a workspace-b)` — that
is the only multi-id spelling; commas are rejected. Types:
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

## Code style

gofmt and go vet are the mechanical baseline (CI-gated). What they leave
open, ADR-0014 decides: method chains break after the dot; multi-line
calls and literals go one item per line with a trailing comma; long
conditions break after the operator; exported identifiers carry terse
godoc contracts; inline comments state only what the code cannot show,
cite ADRs instead of restating them, and never carry tracked-item ids,
change history, or ALL-CAPS emphasis. Reviewers block on departures by
citing the ADR.

## Tests

Table-driven, per ADR-0007 (founder mandate): named cases, `t.Run`
subtests, one uniform assertion body. Declared exceptions only — fuzz
targets, property loops, golden fences, end-to-end scenario tests,
benchmarks. Where the error contract matters (the registry's actionable
messages), rejection tables assert the error names the offending field —
not merely that an error occurred. The pre-PR review flags departures.

## Documentation accuracy

ADR-0008 (founder mandate): docs tell the truth. Honest tense (planned
machinery names its landing milestone or is marked TARGET design),
enforced-means-enforced, every reference resolves, and one standard never
has two disagreeing wordings.
**Definition of Done:** a PR that changes behavior, architecture, or a
contract updates the affected README section, usage doc, AND diagram in
the same PR. The docs-accuracy review rule flags departures.

## Architecture diagrams

`docs/architecture/` holds the C4 model and money-path sequences as
Mermaid, rendering natively on github.com. Diagram updates ride the
changing PR per ADR-0008 clause 4. No GitHub Pages on
this repo — depending on plan it is unavailable or publicly served for a
private repo; either way it stays off.

## Design decisions

Irreversible decisions live in `docs/adr/`. Code that contradicts an
accepted ADR is a bug in one of the two; fix the mismatch, never paper over
it. ADR amendments are their own PRs — with one carve-out: a
reconciliation an incoming ADR explicitly mandates may ride that ADR's
PR, with a dated amendment note stating what changed and what the text
said before.
