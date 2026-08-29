# Architecture — diagrams as code

C4 model plus the three money-path sequences, all Mermaid, rendering
natively on github.com. ADRs 0001–0017 carry the decisions behind them
([docs/adr](../adr/README.md)).

**Definition of Done rule:** a PR that changes the architecture updates
the affected diagram in the same PR (see CONTRIBUTING).

**Drawing rule:** [the stencil](STYLE.md) governs every diagram in this
directory — a semantic palette, a label grammar, and the edge table and
key-facts sections each diagram carries. The palette encodes ownership,
so a node in the wrong colour is a factual defect and a reviewer should
say so. Read it before adding or redrawing a diagram. The three C4
diagrams are drawn to it; the sequences have not been redrawn yet.

| Diagram | What it shows |
|---|---|
| [Diagram stencil](STYLE.md) | the palette, the label grammar, and the sections every diagram here carries |
| [C4 L1 — system context](c4-l1-context.md) | shortfall between the instrumented services, the backends, the providers, and the people |
| [C4 L2 — containers](c4-l2-containers.md) | your code, the core module, the opt-in nested modules, and the never-ships harness — which side of the vendor line each sits on |
| [C4 L3 — capture & engine components](c4-l3-components.md) | inside emit and engine: the paths money takes through the code |
| [Sequence — record & propagate](seq-record-propagate.md) | api → queue → worker with `biz.vc`, the fix for "correlation_id sometimes isn't there" |
| [Sequence — Stripe webhook](seq-stripe-webhook.md) | provider events becoming outcomes, hours-late deliveries included |
| [Sequence — impact query](seq-impact-query.md) | `shortfall impact` from question to four-leg report |

## Repository layout

One git repo, multiple Go modules: the core module has no heavy
dependencies; every adapter under `adapters/` is a nested module, so
depending on the Prometheus exporter never pulls a payments SDK into
your build.

```
biz/          value types: Money, ValueContext, Outcome
registry/     the YAML flow registry: schema, loader, validation
emit/         stage transitions -> bounded metrics + outcome events
propagate/    HTTP middleware and queue header carriers for ValueContext
engine/       the four legs, baseline, report renderers
query/        the query AST and Querier boundary
cmd/shortfall CLI: validate, impact, reconcile
adapters/     export, query, payment, incident — each its own module
examples/     synthetic checkout app used as the ground-truth harness
testkit/      scenario runner and exporter conformance suite
docs/adr/     one ADR per design decision
```

Viewing: GitHub renders Mermaid in the private repo natively. Do NOT
enable GitHub Pages here — depending on plan, Pages on a private repo is
either unavailable or PUBLICLY served; there is no private option below
Enterprise, so it stays off. This directory is canonical; wiki sync is a
go-public task.
