# C4 Level 1 — system context

What an incident cost, who it hit, and how sure you are — where that
answer sits among the people and systems that produce and consume it.

```mermaid
flowchart TB
    ONCALL(["On-call engineer<br/>needs $ impact in minutes,<br/>labelled by evidence"])
    FINANCE(["Finance<br/>co-signs the registry once ·<br/>trusts numbers that reconcile"])

    subgraph ORG["Your payment estate"]
        SERVICES["Instrumented services<br/>api · workers —<br/>emit biz.* signals via shortfall"]
        SHORTFALL["shortfall<br/>capture SDK · flow registry ·<br/>impact engine · CLI"]
        LEDGER[("Ledger / payments DB<br/>the ground truth Finance believes")]
        BACKENDS[("Telemetry backends<br/>Prometheus · Loki · CloudWatch ·<br/>Splunk · Datadog · warehouse")]
    end

    STRIPE["Payment providers<br/>Stripe et al — sync APIs + webhooks"]
    INCIDENT["Incident tools<br/>Slack · incident.io · PagerDuty —<br/>consumers, not producers"]

    SERVICES -->|"Record() per stage ·<br/>biz.vc propagates"| SHORTFALL
    SHORTFALL -->|"bounded metrics +<br/>outcome events (exporters)"| BACKENDS
    BACKENDS -.->|"sum · count · group-by · range<br/>(queriers, read)"| SHORTFALL
    STRIPE -->|"webhooks +<br/>wrapped-client responses"| SHORTFALL
    SHORTFALL -->|"nightly reconciliation →<br/>coverage ratio"| LEDGER
    SHORTFALL -->|"the four-leg report"| INCIDENT
    ONCALL -->|"shortfall impact · /impact"| SHORTFALL
    FINANCE -->|"reviews the registry ·<br/>audits coverage"| SHORTFALL
```

The two questions that shape everything: **Q1 attribution**
(deterministic — which transactions, whose money) rides the outcome
events; **Q2 counterfactual** (statistical — what never happened) rides
baselines over the metrics. shortfall's job is to answer both and label
which is which.
