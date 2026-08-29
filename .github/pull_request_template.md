<!--
This template asks for the evidence chain a reviewer reads, in the order they
read it. The standards behind it live in CONTRIBUTING.md and docs/adr/ — this
file asks for evidence, it does not restate the rules.

Maintainer note: this repository is private today, so only collaborators see
this template. It is ready ahead of the public flip, not waiting on it.

Delete a comment block once you have filled its section in.
-->

## Tracked item

<!--
The id that authorises this change, e.g. `workspace-abc`. If no tracked item
applies, write `no-bead: <reason>` — the same form the commit header takes
(CONTRIBUTING.md, "Commits"). The id belongs in the commit header and here,
and never in a code comment (docs/adr/0014-go-readability-conventions.md).
-->

## What changed, and why

<!--
What a reviewer needs before reading the diff: the behaviour before, the
behaviour after, and the reason. Name any decision someone could reasonably
disagree with, and say what it costs if it turns out to be wrong.
-->

## Verification

<!--
Paste the output, not a claim that it passed, and paste it from this branch at
its current head rather than from an earlier run.

    ./scripts/ci-go.sh fmt
    ./scripts/ci-go.sh vet
    ./scripts/ci-go.sh build
    ./scripts/ci-go.sh test

Those run across every Go module in the repository, nested adapter modules
included. `.warden/bin/warden verify --scope core` runs the same commands
through the policy in repo.yaml.
-->

```text
paste output here
```

- [ ] A changed hot path (Baggage codec, `emit.Record`, in-flight bucketing,
      `engine.Compute`, baseline fit) re-ran its benchmark, and any regression
      is explained above.
- [ ] Not applicable — no hot path changed.

Merging additionally needs the required checks green (`core checks`,
`gitleaks`, `warden gate`) and a committed pre-PR review attestation. See
[CONTRIBUTING.md](https://github.com/NightWatchEng/shortfall/blob/main/CONTRIBUTING.md)
for both.

## Documentation, in this PR

[ADR-0008](https://github.com/NightWatchEng/shortfall/blob/main/docs/adr/0008-docs-tell-the-truth.md)
clause 4: a PR that changes behaviour, architecture, or a contract
updates the affected documentation **in the same PR**. Stale-doc debt is an
incomplete PR, not a follow-up.

- [ ] The affected README section
- [ ] The affected page under `docs/`
- [ ] The affected diagram under `docs/architecture/`
- [ ] An ADR, where the change makes or amends an irreversible decision
- [ ] Not applicable — this changes no behaviour, architecture, or contract,
      so no documentation is owed.

## Review notes

<!--
Optional, and usually the most useful section: a suggested reading order,
whatever you are least sure of, and anything deliberately left undone with the
tracked item that now carries it.
-->
