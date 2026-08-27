# Architecture — diagrams as code

C4 model plus the three money-path sequences, all Mermaid, rendering
natively on github.com. Two kinds of truth here, marked per diagram: the
FROZEN CONTRACT (the v0.1.0 type and interface surface, which exists) and
the TARGET ARCHITECTURE (implementations landing per milestone M3–M7 —
the code refuses loudly until each lands). ADRs 0001–0007 carry the
decisions behind both.

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
enable GitHub Pages here — depending on plan, Pages on a private repo is
either unavailable or PUBLICLY served; there is no private option below
Enterprise, so it stays off. This directory is canonical; wiki sync is a
go-public task.
