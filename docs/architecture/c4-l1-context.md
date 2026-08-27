# C4 Level 1 — system context

What an incident cost, who it hit, and how sure you are — where that
answer sits among the people and systems that produce and consume it.

```mermaid
C4Context
    title shortfall — system context
    Person(oncall, "On-call engineer", "needs $ impact in minutes, labelled by evidence")
    Person(finance, "Finance", "co-signs the registry; trusts numbers that reconcile")
    System_Boundary(org, "Your payment estate") {
        System(services, "Instrumented services", "api, workers — emit biz.* signals via shortfall")
        System(shortfall, "shortfall", "capture SDK + flow registry + impact engine + CLI")
        SystemDb(ledger, "Ledger / payments DB", "the ground truth Finance believes")
        SystemDb(backends, "Telemetry backends", "Prometheus, Loki, CloudWatch, Splunk, Datadog, warehouse")
    }
    System_Ext(stripe, "Payment providers", "Stripe et al: sync APIs + webhooks")
    System_Ext(incident, "Incident tools", "Slack, incident.io, PagerDuty — consumers, not producers")

    Rel(services, shortfall, "Record() per stage; biz.vc propagates")
    Rel(shortfall, backends, "bounded metrics + outcome events via exporters")
    Rel(shortfall, backends, "sum/count/group-by/range via queriers", "read")
    Rel(stripe, shortfall, "webhooks + wrapped-client responses")
    Rel(shortfall, ledger, "nightly reconciliation → coverage ratio")
    Rel(shortfall, incident, "the four-leg report")
    Rel(oncall, shortfall, "shortfall impact / /impact")
    Rel(finance, shortfall, "reviews the registry once; audits coverage")
```

The two questions that shape everything: **Q1 attribution**
(deterministic — which transactions, whose money) rides the outcome
events; **Q2 counterfactual** (statistical — what never happened) rides
baselines over the metrics. shortfall's job is to answer both and label
which is which.
