# Launch message — what to send developers

Maintainer-facing. The wiki generator skips `docs/internal/`, so this page
is not published; it is the source the founder copies from, kept here so the
claims stay checkable against the tree that makes them.

## The rules these drafts follow

ADR-0008 governs how this repository is described as much as what is in it.
A launch message is the highest-traffic doc the project has, and the one
nobody runs a gate over — so the discipline has to be manual and explicit.

**Never claim:**

- production-ready, battle-tested, or any maturity the Status section denies
- adopters, users, or deployments that do not exist
- that the `biz.*` OpenTelemetry semantic conventions are accepted or
  submitted — `docs/internal/semconv.md` says "draft, not submitted"
- a benchmark number without the conditions from `docs/performance.md`

**Always say:** pre-1.0, and that the public interfaces can move between
minor versions (README, "Status").

**Every factual claim traces to a file.** Before sending, re-check:

| Claim | Source |
|---|---|
| the four legs, and the coverage ratio | `README.md`, "What it reports" |
| realized and estimated are never summed | `engine/engine.go` (Evidence doc comment); enforced by the forbidden-sum guard in `engine/report/render_test.go` |
| the adapter list | `git ls-files 'adapters/*/*/go.mod'` |
| v1.0 waits on two external adapters | `README.md`, "Status" |
| no service, datastore, or agent | `README.md`, opening |
| coverage is a reconcile-time number | `engine/engine.go` (Compute sets it unavailable unconditionally); `engine.Coverage`'s only production call site is `cmd/shortfall/reconcile.go` |
| the demo needs no backend | `examples/demo/main.go` — `testkit.QuerierFromResult` serves it in memory |

## Short — Slack, a DM, a reply in a thread

> Every postmortem template has a "revenue impact" field, and it usually gets
> filled by taking a failure count off a dashboard, multiplying by a
> remembered average order value, and putting a `~` in front. Finance can't
> reconcile that, so it gets filed as an anecdote.
>
> I've been building **shortfall** — a Go library + CLI that computes
> incident cost from telemetry your services already emit. No service to run,
> no datastore, no agent.
>
> It's pre-1.0 — v0.x, the public interfaces can still move between minor
> versions — and I'm looking for people to break it: <url>

## Long — LinkedIn, a mailing list, a Show HN body

> **What an incident cost, who it hit, and how sure you are.**
>
> Every postmortem template has a field for customer and revenue impact. It
> usually gets filled by taking a failure count off a dashboard, multiplying
> by a remembered average order value, and prefixing a `~`. Finance has
> nothing to reconcile that against, so it's filed as an anecdote.
>
> A failure count isn't money, for four reasons. A retry isn't a second loss
> — four retried captures are one lost payment, and collapsing them needs the
> entity id, which is gone by the time a request becomes a metric increment.
> Delayed money isn't lost money — which part becomes loss is decided by an
> SLA deadline in a config file, not by the telemetry. The largest loss
> leaves no record at all: checkouts abandoned while the payment page timed
> out never became a request, a log line, or a metric. And sampled traces
> can't count money — at a 10% sample every dollar figure is a 10x
> extrapolation with an invisible error bar.
>
> **shortfall** is a Go library and CLI that takes one incident window and
> reports four legs, each labelled by the kind of evidence behind it:
> realized loss (de-duplicated by entity, net of anything that later
> succeeded), deferred value (in-flight money by age bucket, with your
> registry's SLA deciding what has actually become loss), unrealized loss
> (demand that never arrived, sized against a seasonal baseline — always a
> range, never a point), and customer impact. Deterministic and estimated
> legs are never summed into one figure.
>
> The part I'd most like torn apart is the fifth number: **coverage**. Of the
> money your payment provider's ledger recorded, how much did your telemetry
> actually see? It's computed per (flow, currency) and reported as the
> *worst* slice, not the average — a trust number is a weakest-link number.
> When it reads 0.62 you know what the other four numbers are worth, and
> `shortfall reconcile` tells you which flow and currency you're missing.
>
> There's no service to run, no datastore to install, and no agent to deploy
> — it's a library in your request path and a CLI over your existing backend.
> Adapters today — export: Prometheus, CloudWatch, GCP, OTLP. Query: PromQL,
> SQL, CloudWatch Insights, GCP Logging. Plus Stripe for ledger
> reconciliation, and PagerDuty, incident.io, Rootly, FireHydrant and Slack
> for incidents.
>
> It's **pre-1.0** — v0.x, the public interfaces can still move between minor
> versions, and I'll tag 1.0 only once they've survived two adapters written
> by someone who isn't me.
>
> Two things I'd genuinely like answers to:
>
> 1. Does the four-leg split match how your org actually argues about
>    incident cost — or is there a fifth leg I'm missing?
> 2. If you've wired up one of those backends, where is the adapter wrong?
>
> `go run ./examples/demo` prints a full report against a synthetic checkout
> system — no backend, no signup — so you can see the shape of the answer, and
> argue with it, before pointing anything at real traffic: <url>

## Where it goes first

Three places, not ten.

1. **People who have personally filled in that postmortem field.** Payments
   and SRE, direct messages, no broadcast. This is the only channel likely to
   produce the two external adapters `v1.0.0` waits on.
2. **One written piece leading with the problem**, not the library. The
   README's "The problem" section is the asset; the library is the second
   half of that essay, not the first.
3. **The integration communities** — the ecosystems behind the fourteen
   adapters. "Works with your existing X" is an easier conversation than
   "adopt my library."

**Hold Show HN** until the module graph is verified from outside the
workspace (CONTRIBUTING, "Releases") and `go run ./examples/demo` works from
a clean clone. A first-run failure on launch day is the one mistake that does
not get a second attempt.
