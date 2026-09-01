# Contributing

## The short version

Every change arrives as a PR from a same-repo branch, passes the required
checks (`core checks`, `gitleaks`, `warden gate`), and carries a committed
pre-PR review attestation (`.warden/memory/attest/`). Merge only on green —
no exceptions, including for maintainers (enforce_admins is on).

## Who can contribute

Anyone. Outside contributions arrive by a **branch relay** rather than by a
fork pull request, for two mechanical reasons that outlive the repository
being private:

1. The required `warden gate` check resolves the AgentOps platform over SSH
   against a private repository, using a deploy key held as a repository
   secret. GitHub sends no repository secret to a workflow triggered by a
   pull request from a fork — unconditionally, by design, and not something
   a permissions block or an approval setting changes. The gate is
   fail-closed, so on a fork PR it reports that it did not run.
2. Separately, the required pre-PR review attestation
   (`.warden/memory/attest/`) is produced by `warden attest`, which needs
   that same private-platform access. A contributor could not generate one
   on their own machine either, so this is not a CI problem that a CI change
   would fix.

**The relay.** Open a pull request from your fork anyway, or file an issue
with a link to your branch. A maintainer pushes your commits to a branch in
this repository and opens the PR from there:

```sh
git remote add contrib https://github.com/<you>/shortfall.git
git fetch contrib <your-branch>
git push origin FETCH_HEAD:refs/heads/contrib/<you>-<topic>
```

`refs/heads/` is not optional: `FETCH_HEAD` is not itself under
`refs/heads/`, so git cannot infer the destination namespace and refuses the
push outright.

Your commits keep their authorship, and the PR carries a `Co-authored-by:`
trailer. The maintainer runs and commits the pre-PR review — which is what
the attestation asserts happened, and it did. Everything else is identical
to any other PR, including the six local checks below, which need no special
access and which you should run before asking for the relay.

This costs a maintainer a few minutes per contribution. It is the honest
shape while the gate depends on a private platform, and it is preferred over
the alternative: a required check that can go green without having run is
worse than one that is occasionally inconvenient.

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

Tags drive everything. The `cmd/shortfall/v*` tag — the LAST of the three
waves below, not the root `v*` tag — triggers the `release` workflow:
goreleaser builds `cmd/shortfall` binaries (linux/darwin × amd64/arm64) with
checksums and a changelog, attached to the GitHub release for the root tag.
It fires last because the CLI carries no `replace` directives and so builds
against the published core and adapter modules, which have to exist first.
A tag with a semver pre-release suffix (`v0.3.0-rc1`) is marked pre-release;
a plain `v0.x.y` publishes as a normal release — v0.x instability is the
README's semver policy, not a release flag. Library consumers just `go get`
the tag.
Dry-run rehearsal: run the `release` workflow manually (workflow_dispatch)
— it builds a snapshot and uploads `dist/` as an artifact without tagging
or publishing.

### Tagging is a fan-out, not one tag

`go get` on `github.com/NightWatchEng/shortfall` is satisfied by the root
`v*` tag. Every **nested** module — the fourteen under `adapters/` and
`cmd/shortfall` — is a separate module in Go's eyes, and Go resolves each
one only through a tag carrying its own directory prefix. A root `v0.2.0`
publishes nothing under `adapters/`.

So a release is three ordered waves, and the order is load-bearing: each
wave's tags must exist on the remote before the next wave's `go.mod` can
resolve them.

**Two commits, not one, and the order below is the whole reason.** Every
first-party require in the tree names one version (`test/modgraph` fails the
core test step otherwise), so a release starts by bumping all of them — the
fourteen adapters, the three `test/` modules that import first-party code,
and `cmd/shortfall` — in a merged commit. For every module but
`cmd/shortfall` that is the end of it: a local `replace` means the version is
never resolved and no `go.sum` entry exists to update.

`cmd/shortfall` is the exception, and it cannot be finished in that commit.
It has no replace, so its `go.sum` needs the module hashes for the new
version — and those cannot be computed before that version is tagged and
fetchable. `go mod tidy` at this point fails with `unknown revision`, and
there is no flag that invents the hash. So the bump commit lands with
`go.mod` naming the new version and `go.sum` still on the old one, which
`go.work` hides from every local and CI build. **It is waves 1-2 that make
the tidy possible, and the tidied `go.sum` is its own commit, tagged as wave
3.**

```sh
# 1. the core module
git tag v0.2.0 && git push origin v0.2.0

# 2. every shipped nested module, on its own path prefix
for m in adapters/export/cloudwatch adapters/export/gcp \
         adapters/export/otlp adapters/export/prometheus \
         adapters/incident/firehydrant adapters/incident/incidentio \
         adapters/incident/pagerduty adapters/incident/rootly \
         adapters/incident/slack adapters/payment/stripe \
         adapters/query/cwinsights adapters/query/gcplogging \
         adapters/query/promql adapters/query/sql; do
  git tag "$m/v0.2.0"
done
git push origin --tags

# 3. the CLI. Its versions are only NOW resolvable, so tidy first — this
#    writes the go.sum entries for the tags pushed in waves 1-2 — commit
#    that, and tag the commit that carries it.
(cd cmd/shortfall && GOWORK=off go mod tidy)
git commit -am "chore: cmd/shortfall go.sum for v0.2.0" && git push
git tag cmd/shortfall/v0.2.0 && git push origin cmd/shortfall/v0.2.0
```

Wave 3 is what triggers the `release` workflow: goreleaser builds
`cmd/shortfall` with `GOWORK=off` against the tags from waves 1-2 and
publishes the archives onto the root tag's release. Before it builds, the job
re-derives the version from the tag and refuses to continue unless
`cmd/shortfall/go.mod` carries no `replace`, every first-party require names
that version, and each one's tag exists and is reachable from the commit
being built. Skip a wave and it says which.

**Then ask pkg.go.dev to index each path.** It does not reliably notice a
new version on its own — after the v0.2.0 waves the site still served
v0.1.0 fourteen hours later, and `@v0.2.0` returned 404 for every path but
one (workspace-isc). The proxy was correct throughout; only the display was
behind. One POST per module path fixes it:

```sh
for m in "" /cmd/shortfall /adapters/export/cloudwatch /adapters/export/gcp \
         /adapters/export/otlp /adapters/export/prometheus \
         /adapters/incident/firehydrant /adapters/incident/incidentio \
         /adapters/incident/pagerduty /adapters/incident/rootly \
         /adapters/incident/slack /adapters/payment/stripe \
         /adapters/query/cwinsights /adapters/query/gcplogging \
         /adapters/query/promql /adapters/query/sql; do
  curl -fsS -X POST "https://pkg.go.dev/fetch/github.com/NightWatchEng/shortfall${m}@v0.2.0" \
    >/dev/null && echo "indexed ${m:-/}" || echo "FAILED ${m:-/}"
done
```

`-f` and the echo are the point: without them a 4xx is not a curl error and
the output is discarded, so a step whose entire job is to fix a silent
staleness failure would itself fail silently — and the operator, just told
the site can lag, would blame the lag.

**Then bump the quickstart's download URL.** `docs/quickstart.md` step 2
names a release asset by version (`shortfall_<version>_darwin_arm64.tar.gz`),
because goreleaser puts the version in the filename and GitHub's
`releases/latest/download/` alias cannot absorb that. Nothing checks it: the
old URL keeps returning 200, and `test/docsnippets` reads `go` and `yaml`
fences, never `sh`. Miss this and the next new reader downloads the previous
release.

Modules under `test/` are internal harnesses that nothing imports from
outside this repository; they are deliberately never tagged.

**The `replace` directives are not a substitute.** Every nested module
consumed as a library replaces its first-party dependencies with a relative
path to the repository root: the fourteen under `adapters/<kind>/<name>/`
with `../../..`, and `test/docsnippets`, `test/loggolden` and
`test/promgolden`, one level shallower, with `../..`. Two of them replace
sibling adapters too: `test/loggolden` two (cloudwatch, cwinsights),
`test/promgolden` one (promql). (The remaining `test/` modules —
`blankline`, `licensehdr`, `modgraph`, `symbolcheck`, `wikisync` — are
standalone checkers importing nothing first-party, so they carry neither.)

`cmd/shortfall` is the one exception, and it runs the other way: it carries
no replace at all, because `go install` refuses a module that has one. It
requires the core, promql and sql at their published versions and resolves
them from the proxy like any adopter.

`test/modgraph` is what keeps this paragraph from being the only thing
holding the invariant: it fails the core test step when first-party requires
disagree about a version, name one that was never tagged, lack the local
replace that makes them resolvable here, or — for `cmd/shortfall` — carry
one. A `replace` in a
*dependency's* `go.mod` is ignored by whoever consumes it, so those lines
make the require versions invisible to us while leaving them fully
load-bearing for an adopter. That is exactly how `cmd/shortfall` shipped
requiring two sibling modules at `v0.0.0` — a version that has never
existed — without anything going red.

**Verify from outside the workspace, or you have verified nothing.**
`./scripts/ci-go.sh test` runs under `go.work`, where every replace applies,
so it cannot see a resolution failure. `test/modgraph` catches the version
*correspondence* — that every first-party require names one consistent
version — but it reads the tree, not the proxy, so it cannot tell you whether
that version was ever published.

Only a real resolution can, and it needs the tag waves above pushed. Note what
this checks and what it does not: module resolution finds the newest
*published tag* for each path, never this working tree — so run it after
tagging, to confirm what you published, not before, to preview what you are
about to.

Do not probe a version before you tag it. `proxy.golang.org` caches negative
lookups, so a `go get` of an untagged version leaves it serving `unknown
revision` for some minutes after the tag lands — which reads exactly like a
broken release and is not one. If a fresh tag 404s, check `GOPROXY=direct`
before you conclude anything.

```sh
cd "$(mktemp -d)" && go mod init scratch
cat > main.go <<'EOF'
package main

import (
	"fmt"

	"github.com/NightWatchEng/shortfall/biz"
	_ "github.com/NightWatchEng/shortfall/adapters/export/prometheus"
)

func main() { fmt.Println(biz.Money{Amount: 1, Currency: "USD", Exponent: 2}) }
EOF
GOWORK=off go mod tidy
GOWORK=off go build ./...
```

The import is the point — `go build ./...` in a module with no `.go` files
matches no packages and exits 0, which would make this block look like a
compile check while compiling nothing.

The CLI has its own leg, and from v0.3.0 on it is the one an adopter reaches
for first:

```sh
cd "$(mktemp -d)"
GOWORK=off GOFLAGS= go install github.com/NightWatchEng/shortfall/cmd/shortfall@v0.3.0
shortfall --help    # from GOBIN; the archive is no longer the only way in
```

Run it from an empty directory with the flags cleared. Inside the workspace,
or with a stray `GOFLAGS`, you are testing something else — a `replace` that
would break this for every adopter resolves perfectly well from here.

**`go install` of the CLI is on that list from v0.3.0 on**, and the reason
it took a decision is worth keeping. `go install pkg@version` refuses any
module whose `go.mod` carries `replace` directives — "it must not contain
directives that would cause it to be interpreted differently than if it were
the main module" (`go help install`), enforced unconditionally, benign
replaces included. `cmd/shortfall/go.mod` carried three, and while nothing
was published they were also the only thing resolving the release build:
goreleaser builds that module with `GOWORK=off` and no `GOPRIVATE`, so
without them the build could not reach the core and sibling-adapter modules
at all.

Publishing the tag waves ended that. `cmd/shortfall` now requires the core
and the two query adapters at their published versions and replaces
nothing, so from v0.3.0 on `go install
github.com/NightWatchEng/shortfall/cmd/shortfall@latest` will work and the
release build resolves the graph the same way an adopter does
(workspace-47f). It does not work yet: `@latest` resolves to the newest
published `cmd/shortfall/v*` tag, which is v0.2.0, and that go.mod carries
the replaces and always will — a published `go.mod` is immutable. The
working install path starts at v0.3.0.

Two things follow, and both are enforced rather than remembered:

- **`cmd/shortfall` must never regain a `replace`.** One reinstated line
  breaks the install path for every adopter while `go.work` keeps every
  build here green. `test/modgraph` fails on it
  (`TestInstallTargetsCarryNoFirstPartyReplace`); the inverse check still
  holds for every other module, which is replaced locally as before.
- **The release fires on the wave-3 tag**, not the root `v*` tag, because
  the CLI now builds against published modules and they must exist first.

The CLI also still ships as **release binaries** — linux/darwin ×
amd64/arm64 archives with checksums — and from a clone it runs as `go run
./cmd/shortfall`.

## Code style

gofmt and go vet are the mechanical baseline (CI-gated). What they leave
open, ADR-0014 decides: method chains break after the dot; multi-line
calls and literals go one item per line with a trailing comma; long
conditions break after the operator; a blank line follows the `}` that
closes an `if`, `for`, `switch`, type switch or `select` whenever more code
follows at that level — the exceptions being `else`/`else if`,
`case`/`default`, the enclosing `}`, and a block written on one line;
exported identifiers carry terse godoc contracts; inline comments state
only what the code cannot show, cite ADRs instead of restating them, and
never carry tracked-item ids, change history, or ALL-CAPS emphasis.
Reviewers block on departures by citing the ADR. The blank-line clause is
enforced deterministically rather than by eye: `test/blankline` fails the
core test step on any tracked `.go` file that breaks it, and
`go run ./test/blankline -fix` inserts the missing lines.

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
module's stub, and the compile failure names it. Prose is checked too:
`test/symbolcheck` resolves every backticked `pkg.Symbol` in the guide
docs against the real packages, so a renamed or deleted symbol fails the
build naming the doc and line. An identifier that is not this
repository's — standard library, a vendor SDK, a provider payload field —
goes in that module's `allowSelectors` with its reason.

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

No GitHub Pages on this repo: the wiki is the published surface, and two
generated sites competing to be the docs is how one of them goes stale
without anyone noticing. It stayed off while the repo was private for a
different reason (plan-dependent availability), and it stays off now for
this one.

## Design decisions

Irreversible decisions live in `docs/adr/`. Code that contradicts an
accepted ADR is a bug in one of the two; fix the mismatch, never paper over
it. ADR amendments are their own PRs — with one carve-out: a
reconciliation an incoming ADR explicitly mandates may ride that ADR's
PR, with a dated amendment note stating what changed and what the text
said before.
