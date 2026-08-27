# Architecture — diagrams as code

C4 model plus the three money-path sequences, all Mermaid, rendering
natively on github.com. The diagrams document the surface frozen at
v0.1.0; ADRs 0001–0007 carry the decisions behind it.

**Definition of Done rule:** a PR that changes the architecture updates
the affected diagram in the same PR (see CONTRIBUTING).

| Diagram | What it shows |
|---|---|
| [C4 L1 — system context](c4-l1-context.md) | shortfall between the instrumented services, the backends, the providers, and the people |
| [C4 L2 — containers](c4-l2-containers.md) | the four layers plus CLI and testkit, and which side of the vendor line each sits on |
| [C4 L3 — capture & engine components](c4-l3-components.md) | inside emit and engine: the paths money takes through the code |
| [Sequence — record & propagate](seq-record-propagate.md) | api → queue → worker with `biz.vc`, the fix for "correlation_id sometimes isn't there" |
| [Sequence — Stripe webhook](seq-stripe-webhook.md) | provider events becoming outcomes, hours-late deliveries included |
| [Sequence — impact query](seq-impact-query.md) | `shortfall impact` from question to four-leg report |

Viewing: GitHub renders Mermaid in the private repo natively. Do NOT
enable GitHub Pages here — on this plan Pages serves publicly even from a
private repo. Wiki sync happens at go-public (the wiki product is not
available on private personal repos on this plan; this directory is
canonical either way).
