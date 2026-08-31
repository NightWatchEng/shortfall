# The money path

Three sequences, one path. A value context is stamped and propagated
(§1), provider events join the same funnel (§2), and a question becomes
a four-leg report (§3). The structural view is at
[C4 L1–L3](README.md); this page is the dynamic one.

---

## 1. Record and propagate across a queue

The direct fix for "correlation_id sometimes isn't there": the value
context is a first-class propagated thing — **one** Baggage member,
re-attached by the consumer with one header copy and no per-team
instrumentation ask.

Five phases, two of which are fences rather than steps: the **ingress
stamp**, where a request that arrived without a value context gets one,
and the **egress fence**, where a request heading somewhere it should not
carry money loses it again.

```mermaid
sequenceDiagram
    autonumber
    actor U as 👤 Caller<br/>upstream service or browser

    box rgba(140,140,140,0.10) Your api service — one process
        participant MW as propagate/httpmw<br/>core module — Middleware in, Transport out
        participant API as api handler<br/>your code — the IngressFunc and the business call
        participant EMA as emit.Std<br/>core module — Record buffers, a ticker flushes
    end

    participant EG as Outbound host<br/>an internal service, or a third-party API
    participant K as capture.q<br/>your broker — Kafka, SQS or AMQP

    box rgba(140,140,140,0.10) Your capture-worker — a different process
        participant W as capture-worker<br/>your code — propagate.Extract, then Record
        participant EMW as emit.Std<br/>core module — its own buffer and its own de-dup set
    end

    participant X as Export adapter → backend<br/>emit.Exporter — prometheus · otlp · cloudwatch · gcp

    rect rgba(140,140,140,0.10)
        Note over U,API: Phase 1 — ingress stamping
        U->>MW: POST /pay — traceparent + baggage
        MW->>MW: recoverMembers(header) — valid members kept,<br/>malformed neighbours dropped and logged
        alt no valid biz.vc on the wire
            MW->>API: IngressFunc(r) — the first hop that recognises the flow
            API-->>MW: biz.ValueContext{flow, entity, hashed customer, amount}
            MW->>MW: Validate + estimate, then biz.WithValueContext(ctx, vc)
        end
        MW->>API: next.ServeHTTP(w, r.WithContext(ctx))
    end
    Note over MW,API: A valid wire biz.vc always wins — the hook stamps only in its<br/>absence. The hook's output faces the same fences as the wire —<br/>PII or a bad shape is rejected loudly and the request still proceeds.

    rect rgba(140,140,140,0.10)
        Note over API,X: Phase 2 — capture at the first hop
        API->>EMA: em.Record(ctx, "auth", biz.ResultSuccess)
        EMA--)X: Flush — ExportMetrics + ExportEvents
    end
    Note over EMA,X: The outcome event is emitted regardless of trace sampling.<br/>Record consults no sampler. A trace id is attached when one is<br/>present and is never load-bearing.

    rect rgba(140,140,140,0.10)
        Note over API,EG: Phase 3 — the egress fence
        API->>MW: outbound call through httpmw.NewTransport(reg, base)
        alt host matches registry.Propagation.AllowHosts
            MW->>EG: baggage rebuilt, biz.vc injected from ctx
        else any other host — deny by default
            MW->>EG: baggage rebuilt, biz.vc deleted
        end
    end
    Note over MW,EG: Strip, not merely "do not add": a biz.vc a global propagator put<br/>there is removed, and the header is rebuilt from recovered members<br/>on a clone, so raw bytes never smuggle one past the fence.

    rect rgba(140,140,140,0.10)
        Note over API,W: Phase 4 — the queue hop
        API->>K: propagate.Inject(kafka.NewCarrier(&headers), vc)
        K->>W: consume — one header, biz.vc
        W->>W: vc, ok, err := propagate.Extract(carrier)
        W->>W: ctx, _ := biz.WithValueContext(ctx, vc)
    end
    Note over K,W: Minutes pass. Trace context may be re-rooted or dropped —<br/>biz.vc survives, because it is a message header the carrier copies<br/>rather than something the tracing pipeline owns.

    rect rgba(140,140,140,0.10)
        Note over W,X: Phase 5 — capture at the failing hop
        W->>EMW: em.Record(ctx, "capture", biz.ResultFailed, emit.WithErr(msg))
        EMW--)X: Flush — ExportMetrics + ExportEvents
    end
    Note over EMA,EMW: Two processes, two buffers, two de-dup sets. In-process de-dup<br/>collapses retries but cannot see across the hop, which is why the<br/>engine's realized leg de-dups by entity a second time.
```

| # | Step | Mechanism / constraint |
|---|---|---|
| 2 | `httpmw` → itself | `recoverMembers` parses the `baggage` header member by member and keeps the valid ones on the `context.Context`. A malformed neighbour is dropped and logged, never fatal, and never hides a valid `biz.vc`. **The decode to a `ValueContext` is lazy** — it happens in `biz.FromContext`, at read time |
| 3 | `httpmw` → api handler | `IngressFunc func(*http.Request) (biz.ValueContext, bool)` is your code, because only your code knows which route is which flow. Called **only** when the wire carried no valid `biz.vc` |
| 4 | api handler → `httpmw` | A hook that cannot know the amount returns `Estimated: true` with a zero amount, and the registry's estimator fills it |
| 5 | `httpmw` → itself | `vc.Validate()` first — the PII guard lives here, **not** in the codec — then `biz.WithValueContext`. Encode fails loudly past **512 UTF-8 bytes** (`*biz.OversizeError`); either way the request proceeds without a stamp rather than 500ing |
| 7 | api handler → `emit` | `Record` reads the `ValueContext` off the `ctx`, never as an argument. It buffers and returns; it never blocks and never returns an error. Two metric points normally (`biz_txn_total` always, `biz_value_total` unless `vc.Estimated`) plus one `biz.Outcome` |
| 8 | `emit` ⇢ export adapter | **Asynchronous.** A background ticker (1 s by default) and `Close` call `Flush`. A drop at any point increments `biz_dropped_events_total{reason}`; the reasons are `invalid`, `overflow` and `export` |
| 9 | api handler → `httpmw` | `NewTransport` is an `http.RoundTripper`, so the fence applies to every outbound call, **including each redirect hop** — a redirect from an allowed host to a disallowed one is fenced at the second hop |
| 10–11 | `httpmw` → outbound host | The allowlist is matched exactly or as `*.domain` against a strictly deeper subdomain; an empty allowlist denies everything. Denied: `delete(members, biz.MemberKey)`. Foreign members still pass through, and if a safe header cannot be rebuilt the `baggage` header is dropped entirely — fail closed |
| 12–13 | api handler → broker → worker | `propagate.Inject` writes exactly one header, key `biz.vc`, the same key the HTTP `baggage` header uses. The encoded form is versioned — a leading `1` and eleven pipe-delimited fields — so a decoder can evolve without breaking an old producer |
| 14 | worker → itself | `propagate.Extract` returns **three** distinguishable outcomes: present and well-formed, absent, and present-but-corrupt. Corrupt is never silently read as absent. The library counts none of them — the error is handed back for your consumer to count |
| 16 | worker → `emit` | The in-process de-dup key is `(flow, entity, stage, result)`. **Result is in the key**, so a later `failed → success` transition on the same entity still emits |

Two things this sequence is drawn to make unmissable. **The egress fence
strips rather than declining to add** — including a member a globally
installed OTel Baggage propagator added — because shipping a transaction
amount and a customer id to a third party is a decision someone makes on
purpose (ADR-0003). And **one member, not eight**: the whole
`ValueContext` is a single versioned `biz.vc`, which is what makes "the
carrier copies one header" true across Kafka, SQS and AMQP alike.

---

## 2. Provider events become outcomes

Same signals, a different capture point. Metadata stamped at creation
makes every webhook arrive pre-attributed, and the wrapped backend sees
the infrastructure failures webhooks never report.

`adapters/payment/stripe` imports **`biz` only** — not `emit`, not
`engine`, not `registry`. It never calls `Record` and never holds an
`Exporter`. It produces `biz.Outcome` values and hands them to callbacks
**your** code registers; your code is what calls `emit`. That seam is why
a non-Stripe user never compiles stripe-go.

```mermaid
sequenceDiagram
    autonumber
    box rgba(140,140,140,0.10) Your service — one process
        participant S as 🧩 your service<br/>your code — creates intents, owns both sinks
        participant EM as emit.Std<br/>core module — Record buffers, a ticker flushes
    end

    participant X as Export adapter → backend<br/>emit.Exporter — prometheus · otlp · cloudwatch · gcp

    box rgba(140,140,140,0.10) adapters/payment/stripe — its own Go module, imports biz only
        participant WB as stripe.Backend decorator<br/>WrapBackend — installed with stripe.SetBackend
        participant WH as webhook receiver<br/>stripe.Handler + VerifyAndMap
        participant RC as ledger reconciler<br/>stripe.Reconcile + ListPageFunc
    end

    participant ST as Stripe<br/>api.stripe.com — sync API, webhooks, list API

    rect rgba(140,140,140,0.10)
        Note over S,ST: Phase 1 — creation stamps the attribution
        S->>S: stripe.WithStripeMetadata(params, vc)
        S->>WB: paymentintent.New(params) — through the installed backend
        WB->>ST: POST /v1/payment_intents
        ST-->>WB: response, or 4xx, 5xx, 429, timeout
    end
    Note over S,WB: Exactly three metadata keys ride the intent — biz_flow, biz_entity,<br/>biz_customer. Amount and currency deliberately do not, because the<br/>event payload already carries the authoritative figure.

    rect rgba(140,140,140,0.10)
        Note over S,ST: Phase 2 — the sync truth a webhook never delivers
        WB-->>S: onCall(ProviderCall{op, outcome, status, latency, err})
        S->>EM: em.RecordProviderCall("stripe", p.Op, p.Outcome)
        alt infrastructure failure on an auth op — timeout, 5xx or 429
            WB-->>S: onAuth(biz.Outcome{Stage auth, Result failed, Source stripe:client})
            S->>EM: em.Record(ctx, o.Stage, o.Result, emit.WithSource(o.Source), emit.WithAt(o.At))
        end
    end
    Note over WB,ST: A 402 decline is Stripe answering, so it is classified a successful<br/>provider call and no synthetic auth failure is invented. The gates are<br/>narrow — failure, an auth op, PaymentIntent params, and a biz_flow key.

    rect rgba(140,140,140,0.10)
        Note over S,ST: Phase 3 — webhooks arrive pre-attributed
        ST--)WH: payment_intent.succeeded · payment_intent.payment_failed ·<br/>payment_intent.processing · payment_intent.requires_action ·<br/>charge.failed · charge.dispute.created ·<br/>invoice.paid · invoice.payment_failed
        WH->>WH: VerifyAndMap — 1 MiB body cap, then HMAC and timestamp
        WH-->>S: sink(biz.Outcome{At: event.Created, Source: stripe:webhook})
        S->>EM: em.Record(ctx, o.Stage, o.Result, emit.WithSource(o.Source), emit.WithAt(o.At))
        EM--)X: Flush — ExportMetrics + ExportEvents
    end
    Note over WH,ST: Provider event time, not receipt time. Delivery may be hours late<br/>during an outage, and a late event must not move money into the<br/>wrong window. An unmapped but verified event is a 200 and no outcome.

    rect rgba(140,140,140,0.10)
        Note over S,ST: Phase 4 — the reconciler polls, it does not listen
        RC->>ST: ListPageFunc — payment intents created since T, paged
        ST-->>RC: pages of PaymentIntent
        RC-->>S: stripe.Ledger{Rows []biz.LedgerRow, Scanned, Skipped}
    end
    Note over S,RC: These rows are the ledger side of the coverage ratio —<br/>shortfall reconcile passes them to engine.Coverage, and they<br/>never enter the realized leg.

    Note over S,EM: Both capture points can fire for one PaymentIntent. The in-process<br/>de-dup key carries stage and result, so a stripe:client auth failure<br/>and a stripe:webhook capture success both emit — and the engine's<br/>realized leg then drops that entity as recovered.
```

| # | Step | Mechanism / constraint |
|---|---|---|
| 1 | your service → itself | `WithStripeMetadata(p MetadataSetter, vc biz.ValueContext)` is a void mutator, not a wrapper. It takes anything with `AddMetadata(key, value string)`, so `*stripe.Params` and every params type embedding it qualifies |
| 2 | your service → backend decorator | Your call site is **unchanged stripe-go**. `WrapBackend` returns a `stripe.Backend` that embeds the inner one and overrides `Call`; install it once with `stripe.SetBackend`. It creates nothing, reads no response body, and is invisible at the call site |
| 4 | Stripe → backend decorator | `providerOutcome` classifies status 0 (transport error or timeout), any 5xx, and 429 as `failed`. **Every other 4xx is `success`** — Stripe answered, and a declined card is a business result, not an outage |
| 5 | backend decorator → your service | `WithProviderMetric` is a callback, not a counter: the adapter observes, `emit` counts. One line wires it, and the call lands on `biz_provider_calls_total{provider, op, outcome}` inside the buffer, the label fence and the drop counter |
| 6 | backend decorator → your service | `WithAuthOutcome`, behind four gates: the call failed, the op is `payment_intents.create` or `.confirm`, the params are `*stripe.PaymentIntentParams`, and `biz_flow` metadata is present. The `ValueContext` is rebuilt from the **request params**, not the response, and `At` is the call-start time |
| 8 | Stripe → webhook receiver | Eight mapped event types, and the mapping is the adapter's whole opinion: succeeded/payment_failed/processing → `capture`, requires_action → `auth` deferred, `charge.failed` → `capture` failed, dispute → `dispute` failed, the two `invoice.*` → `settle`. A verified event outside the map is ignored with a 200 |
| 9 | webhook receiver → itself | Body capped at 1 MiB **before** verification, then `ConstructEventWithOptions` with `IgnoreAPIVersionMismatch`. The HMAC and timestamp checks still run — only the SDK's `api_version` pin is skipped. A read or signature failure is a 400, which is what makes Stripe retry |
| 10 | webhook receiver → your service | The outcome's `At` is `event.Created` and its `Source` is `stripe:webhook`. There is **no idempotency on `event.ID`** here — redelivery is handled downstream, by entity |
| 13–14 | reconciler → Stripe | `Reconcile(ctx, fetch PageFunc, since)` pages through a caller-supplied `PageFunc`; the loop terminates on `!HasMore`, an empty page, or a cursor that stops advancing. Terminal statuses only are classified, and the amount basis is the intent's `amount` — never `amount_received` (ADR-0010) — so a partial capture does not silently reduce the ledger side |

Two capture points, two sources, and the double-count is caught
downstream: a create that times out emits `auth`/`failed` with
`Source: stripe:client`; if Stripe processed it anyway, the webhook later
emits `capture`/`success`. Both emit, and the realized leg then finds a
success for that entity and drops it. Net realized loss: zero, which is
correct. On a metrics-only backend there is no entity to de-duplicate by,
which is exactly what that leg's `upper bound` caveat admits to.

A Stripe outage does **not** show up here as webhook silence. It shows up
in your own entry-stage counter, which the counterfactual leg compares
against its baseline. What this adapter contributes is positive evidence:
a non-zero `biz_provider_calls_total{outcome="failed"}` in the window
makes the engine append an upstream-attribution hint.

---

## 3. An impact question becomes a four-leg report

Query-time, not ingest-time. The engine asks any backend only four verbs
— sum, count, group-by and time range — plus event order, so nothing
vendor-specific leaks past the adapter.

Two things the picture is drawn to make unmissable: **the legs run one at
a time and degrade one at a time**, and **coverage is never queried here
at all**.

```mermaid
sequenceDiagram
    autonumber
    actor O as 🚨 On-call engineer<br/>needs the $ figure in minutes

    box rgba(140,140,140,0.10) cmd/shortfall — its own Go module
        participant CLI as ⌨️ shortfall impact<br/>cmd/shortfall — flags in, rendered report out
    end

    box rgba(140,140,140,0.10) shortfall core module — zero heavy deps
        participant R as registry.Registry<br/>core module — an in-memory value, loaded once
        participant E as engine.Compute<br/>core module — five legs, strictly one at a time
    end

    participant Q as combined query.Querier<br/>cmd/shortfall — promql for metrics, sql for events

    participant B as Your backends<br/>Prometheus + your event store

    rect rgba(140,140,140,0.10)
        Note over O,Q: Phase 1 — the question
        O->>CLI: shortfall impact --registry r.yaml --from … --to …<br/>--scope stage=capture --prometheus … --sql …
        CLI->>R: registry.Load(r.yaml)
        CLI->>Q: buildQuerier — combined{metrics: promql, events: sql}
        CLI->>E: Compute(ctx, reg, q, Request{Window, Scope, Flows})
    end
    Note over CLI,Q: The CLI owns both. The engine is handed a loaded registry value<br/>and a Querier — it opens no file and dials no backend of its own.

    rect rgba(140,140,140,0.10)
        Note over E,B: Phase 2 — realized, the deterministic leg
        E->>Q: Capabilities()
        alt Caps.Events
            E->>Q: QueryEvents(outcome=success, group by currency · entity)
            Q->>B: translated at the adapter — PromQL or SQL
            B-->>Q: rows
            E->>Q: QueryEvents(outcome=failed, group by currency · entity,<br/>Agg = EventAggMaxPerGroup)
        else Caps.Metrics only
            E->>Q: QueryMetric(sum biz_value_total{outcome=failed} by currency)
            E->>Q: QueryMetric(sum biz_txn_total{outcome=failed})
        end
    end
    Note over E,Q: ADR-0009 — the failed sweep takes the maximum single amount per<br/>(currency, entity), never a mean, and an entity that also has a<br/>success in the window is dropped entirely as recovered.<br/>The metrics-only branch cannot do either, and says so in a caveat.

    rect rgba(140,140,140,0.10)
        Note over E,Q: Phase 3 — deferred, money still in flight
        E->>Q: QueryMetric(biz_inflight_value by flow · stage · age_bucket · currency)
        E->>Q: QueryMetric(biz_inflight_count by flow · stage · age_bucket · currency)
    end
    Note over E: Two gauges, read as levels rather than summed. The SLA deadline<br/>and on-breach rule come from the registry — a breach becomes<br/>projected-lost inside this leg and never moves into realized.

    rect rgba(140,140,140,0.10)
        Note over E,Q: Phase 4 — customers, who was hit
        E->>Q: QueryEvents(outcome=failed, group by currency · customer · segment)
    end
    Note over E: Distinct count and top-N are ranked in-process from these groups.<br/>This leg is gross and recovery-agnostic on purpose — it is a<br/>who-to-call list, and summing it as company loss double-counts.

    rect rgba(140,140,140,0.10)
        Note over E,Q: Phase 5 — unrealized, the estimate
        E->>R: Stages[0], baseline lookback, estimator, recovery fraction, value stage
        E->>Q: QueryMetric(sum biz_txn_total at the entry stage, step 1h — lookback weeks)
        E->>Q: QueryMetric(sum biz_txn_total at the entry stage, step 1h — observed window)
        E->>Q: QueryEvents + 2 × QueryMetric — average order value at the value stage
        E->>Q: QueryMetric(sum biz_provider_calls_total{outcome=failed})
    end
    Note over E: The hour-of-week median and MAD are computed in-process from those<br/>hourly buckets. The backend serves counts, never a baseline.<br/>The result has no single-number form — only Low, Mid and High per currency.

    rect rgba(140,140,140,0.10)
        Note over O,E: Phase 6 — assembly, and the leg nobody queried
        E->>E: Coverage = unavailable("coverage needs a provider ledger —<br/>run shortfall reconcile for the trust number")
        E->>R: reg.Severity ladder → SuggestSeverity(realized + deferred per minute)
        E-->>CLI: Report{Realized, Deferred, Unrealized, Customers,<br/>Coverage, Severity, GeneratedAt, RegistryVersion, LibraryVersion}
        CLI-->>O: RenderText | RenderMarkdown | RenderJSON
    end
    Note over E,CLI: A leg the backend cannot ground marks itself — Unavailable bool plus<br/>a caveat or note on the money legs, NotAvailableReason on customers,<br/>a reason string on coverage (ADR-0017). Never a fabricated zero, and<br/>realized is never summed with the estimate.
```

| # | Step | Mechanism / constraint |
|---|---|---|
| 1 | on-call → CLI | `--registry`, `--from` and `--to` are required (RFC3339); `--scope k=v` and `--flow` repeat. With neither `--prometheus` nor `--sql` the command exits 2 before reaching the engine — there is no default backend |
| 3 | CLI → querier | With both flags the CLI builds a `combined` `Querier` over the two nested modules. `Capabilities()` takes **each field from the backend that owns it** — not an intersection: PromQL reports `Events: false` and SQL reports `Metrics: false`, so ANDing them would leave every leg ungrounded |
| 5 | engine → querier | `Capabilities()` is asked **before every leg**. A leg whose capability is absent is skipped and marked, so the unsupported verb is never issued |
| 6 | engine → querier | The recovery sweep: successes in the window, grouped `(currency, entity)`. It reads no money — it exists only to build the set of entities to exclude |
| 7 | querier → backend | The adapter translates. **Every query in this diagram takes this hop**; it is drawn once because the constraint is the same each time |
| 9 | engine → querier | The failure sweep, grouped `(currency, entity)` with `Agg: EventAggMaxPerGroup` — the largest single failed attempt, a real observed figure (ADR-0009) |
| 11 | engine → querier | The metrics-only fallback for `Leg.Count` carries the caveat `metrics-only: upper bound, not de-duped by entity` — metrics have no entity label, by design (ADR-0004) |
| 12–13 | engine → querier | `biz_inflight_value` and `biz_inflight_count`, no aggregation set: gauges read at the window, not summed over it. A missing count gauge beside a non-empty value gauge raises the ADR-0012 caveat rather than inventing a count |
| 14 | engine → querier | Failed events grouped `(currency, customer, segment)` — **no order-by, no limit, no distinct-count verb**. `EventAggDistinctCount` and `OrderSumDesc` exist in the AST and in the adapters, and the engine uses neither |
| 18 | engine → querier | Average order value at the flow's declared value stage (ADR-0016), in a fixed order of preference: successful events, then `biz_value_total ÷ biz_txn_total`, then the registry's estimator. A failed events query is disclosed as a note rather than silently downgraded |
| 20 | engine → itself | **No query.** The trust number needs a provider ledger an impact request does not carry, so `Compute` writes an unavailable reason and `shortfall reconcile` calls `engine.Coverage` separately |
| 21 | engine → registry | `SuggestSeverity` walks the registry's ladder against realized-plus-deferred per minute, **per currency** with no cross-currency sum, taking the most severe level any one currency triggers (ADR-0013). Nothing clearing the lowest threshold returns `""`, never a fabricated severity |
| 23 | CLI → on-call | `--format text` (default), `markdown` or `json`. The three money legs always carry their evidence tag, grounded or not; coverage carries its `[trust]` tag **only when grounded**, which in an impact report it never is. An ungrounded money leg prints `none` with its caveat beside it |

**Realized de-duplicates by entity and excludes recovery.** A transaction
that failed and then succeeded contributes nothing; an entity redelivered
five times contributes its largest single failed attempt once. The
exclusion is set membership over the window, not a timestamp comparison —
a success anywhere in the window clears the entity.

**An impact report never carries a real coverage number.** Rendering that
absence as 100% would invert the meaning of the one line whose job is to
say how much you can trust the rest.
