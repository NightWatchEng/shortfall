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

## Licensing of contributions

Inbound = outbound: by submitting a PR you agree your contribution is
licensed under [Apache-2.0](LICENSE), the same terms the project ships
under. There is no CLA and no copyright assignment — you keep your
copyright. Certify origin with a DCO sign-off
([developercertificate.org](https://developercertificate.org/)) on each
commit: `git commit -s` adds the `Signed-off-by:` trailer. Sign-off is a
stated convention, not yet machine-checked — nothing in CI or the hooks
verifies the trailer today; reviewers may ask for it.

Every `.go` file carries the two-line header:

```go
// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0
```

New files get it too — `go run ./test/licensehdr -fix` inserts it, and the
test in that module fails the ordinary test step for any tracked file
missing it. `LICENSE` and `NOTICE` themselves never change in a
contribution PR; they are founder-only paths.

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

`docs/` is the source of truth and the wiki is a generated mirror —
`test/wikisync` regenerates the whole page set on every push to main, so
wiki-side edits are overwritten. `docs/internal/` is the exception: it holds
founder- and maintainer-facing records that have no reader on a public docs
site, and the generator skips it. Everything else under `docs/` is mirrored
automatically, which means it can be published with nothing linking to it,
so every page must be reachable one of two ways: a new guide gets an entry
in `test/wikisync`'s curated navigation, and a new ADR gets a row in
`docs/adr/README.md`. `TestThisRepoNavigationCoversEveryPage` runs the
generator over this repository and fails on a page neither route covers,
naming the page and which route it needs. Fenced Go and registry examples in the
guide docs are compiled and validated by `test/docsnippets` in the core CI
job — adding a fence with a new doc-implied identifier extends that
module's stub, and the compile failure names it.

## Architecture diagrams

`docs/architecture/` holds the C4 model (levels 1–3) and the money-path
sequences as Mermaid, rendering natively on github.com and in the wiki.
Diagram updates ride the changing PR per ADR-0008 clause 4. There are no
other diagrams: a picture earns its place by showing structure or a
runtime path that prose cannot, and anything else is a table.

**Colour encodes ownership** — who wrote it, who ships it, who can change
it. It is never decoration, so a node in the wrong class is a false
statement about the code and a reviewer should say so. Copy the classes a
diagram uses and delete the rest:

```
classDef person  fill:#08427b,stroke:#052e56,color:#fff;   /* a human actor */
classDef yours   fill:#6b4c9a,stroke:#4a3369,color:#fff;   /* the reader's own code */
classDef core    fill:#1168bd,stroke:#0b4884,color:#fff;   /* the core Go module */
classDef optin   fill:#2e6fb0,stroke:#0b4884,color:#fff;   /* a nested module you opt into */
classDef ext     fill:#8a8a8a,stroke:#5f5f5f,color:#fff;   /* reached only through an interface */
classDef harness fill:#8a6d1f,stroke:#5c4814,color:#fff,stroke-dasharray:6 4;  /* never ships */
```

The boundary drawn is the **module** boundary, not the company boundary:
your own Prometheus and your own ledger are `ext`, because shortfall
reaches them only through `emit.Exporter` and `query.Querier`. `core` and
`optin` stay two colours because that split is the repo's central promise.
At Level 1 there are no module boundaries to draw, so shortfall is one
`core` box. `harness` carries a dashed stroke as well as a colour, since a
reader mistaking test-only ground truth for production code is the
expensive error.

Shape is a separate axis: stadium `id(["…"])` is a person, rectangle
`id["…"]` a code unit, cylinder `id[("…")]` a datastore, diamond
`id{"…"}` a gate.

Label grammar — bold name as spelled in the repo, italic technology line,
then responsibilities as ` · `-separated fragments:

```
id["<b>engine</b><br/><i>core module</i><br/>four legs · coverage · severity"]
```

Keep arrow labels to a short verb phrase. Every protocol, constraint and
caveat goes in the diagram's **edge table**, which every diagram carries:
one row per edge, or per numbered step, that carries a real constraint —
and one row may cover several that share one. A
`subgraph` or `box` is a claim about everything inside it; `alt`/`opt` is
only for a branch that exists in the code.

Two sequence rules are render correctness, not taste:

- **No semicolons in message or note text.** Sequence text is unquoted and
  `;` terminates the statement, so the rest of the line parses as a new one
  and the fence fails to render. Use an em dash.
- **Box and rect tints are low-alpha `rgba`, never opaque `rgb`.** These
  diagrams render in both of GitHub's themes, and a solid pale fill puts
  pale text on a pale ground in one of them.

Sequences also use `autonumber` and take no markup in labels.

No GitHub Pages on this repo — depending on plan it is unavailable or
publicly served for a private repo; either way it stays off. The wiki is
the published surface instead.

## Design decisions

Irreversible decisions live in `docs/adr/`. Code that contradicts an
accepted ADR is a bug in one of the two; fix the mismatch, never paper over
it. ADR amendments are their own PRs — with one carve-out: a
reconciliation an incoming ADR explicitly mandates may ride that ADR's
PR, with a dated amendment note stating what changed and what the text
said before.
