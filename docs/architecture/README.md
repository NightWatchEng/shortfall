# Architecture

The [C4 model](https://c4model.com/), levels 1 to 3, plus the money-path
sequences. All Mermaid, rendering natively on github.com and in the wiki.
The decisions behind them are in the [ADRs](../adr/README.md).

| Level | Diagram | Scope |
|---|---|---|
| 1 | [System context](c4-l1-context.md) | shortfall among the services, backends, providers and people around it |
| 2 | [Containers](c4-l2-containers.md) | the Go modules: your code, the core module, the opt-in nested modules, the never-ships harness |
| 3 | [Components](c4-l3-components.md) | inside `emit` and `engine` — the paths money takes through the code |
| — | [The money path](money-path.md) | the three runtime sequences: record and propagate, provider events, impact query |

Each diagram carries a table naming the protocol or constraint behind
every edge that has one — one row per edge in the C4 levels, one per
numbered step that carries a constraint in the sequences. Colour is
semantic and encodes ownership, so a node in the wrong colour is a
factual defect: violet is your code, dark blue the
core module, mid blue an opt-in nested module, ochre-dashed never ships,
grey external. The full drawing rules are in
[CONTRIBUTING](../../CONTRIBUTING.md#architecture-diagrams).

A PR that changes the architecture updates the affected diagram in the
same PR (ADR-0008 clause 4).

## Repository layout

One git repo, multiple Go modules. The core module has no heavy
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
