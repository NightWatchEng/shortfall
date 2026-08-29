# Diagram stencil

Every diagram in this directory is drawn to the stencil below. It exists so
that "does this diagram match the others?" is a question with an answer:
a reviewer can point at a node and call its colour, its label, or a missing
section a **defect**, the same way they can call a float in `biz/` a defect.

The stencil is adapted from the [C4 model](https://c4model.com)'s conventional
palette. shortfall is a **library**, not a deployed system, so the classes
carry a library's distinctions rather than a deployment's — see
[What the colours mean here](#what-the-colours-mean-here).

**Scope:** every diagram in this directory, and any diagram added to it.
The three C4 diagrams are drawn to it today. The money-path sequences
(`seq-*.md`) are governed by it but have not been redrawn yet; a sequence
diagram has no `classDef` surface, so when they are, they take the label
grammar, the edge table, and the key-facts section, and skip the palette.

---

## 1 · The palette is semantic

**Colour encodes ownership: who wrote it, who ships it, who can change it.**
It is never decoration, and it never encodes importance, freshness, or
where a node happens to sit on the canvas.

That is the whole point of writing it down. A node drawn in the wrong class
is not a cosmetic nit — it is a **false statement about the code**, and a
reviewer should say so. Drawing `cmd/shortfall` in `core` **at Level 2**,
for instance, would assert a module boundary that does not exist: the CLI is
its own Go module, and it pulls a SQLite driver the core module has never
depended on. (At Level 1 the single shortfall box is the system in scope and
legitimately covers the CLI — see the `core` row below.)

| Class | Fill | Stroke | Means |
|---|---|---|---|
| `person` | `#08427b` | `#052e56` | A **human actor**. Someone who asks shortfall a question or answers for its output. |
| `yours` | `#6b4c9a` | `#4a3369` | **The reader's own code.** You write it, you own it, shortfall only offers it an interface. |
| `core` | `#1168bd` | `#0b4884` | **shortfall's own shipped code.** At Level 1 that is the whole system in scope, CLI included — there are no module boundaries to draw at that zoom. From Level 2 down it narrows to the **core Go module** (`github.com/NightWatchEng/shortfall`), imported directly, zero heavy dependencies, with `optin` carrying the nested modules. |
| `optin` | `#2e6fb0` | `#0b4884` | **A separate nested Go module shipped from this repo** — every `adapters/*` module and the `cmd/shortfall` CLI. You opt in, and its dependencies stay out of your build until you do. Used from Level 2 down, where module boundaries are the subject. |
| `ext` | `#8a8a8a` | `#5f5f5f` | **External to the shortfall module.** Reached only through an interface; shortfall does not control its behaviour or its uptime. |
| `harness` | `#8a6d1f` | `#5c4814`, `stroke-dasharray:6 4` | **Never ships.** Test-only ground truth: `examples/checkout`, `testkit`, `test/*`. |

All six use `color:#fff`.

Copy this block verbatim into any diagram that needs it, and delete the
classes the diagram does not use:

```
classDef person  fill:#08427b,stroke:#052e56,color:#fff;
classDef yours   fill:#6b4c9a,stroke:#4a3369,color:#fff;
classDef core    fill:#1168bd,stroke:#0b4884,color:#fff;
classDef optin   fill:#2e6fb0,stroke:#0b4884,color:#fff;
classDef ext     fill:#8a8a8a,stroke:#5f5f5f,color:#fff;
classDef harness fill:#8a6d1f,stroke:#5c4814,color:#fff,stroke-dasharray:6 4;
```

### What the colours mean here

The C4 palette was designed for a deployed system, where "external" means
*another company's server*. Transplanted unchanged, it would say almost
nothing about a library: shortfall has no servers, no database, and no
deployment of its own.

So the boundary the palette draws is **the shortfall module boundary, not
the company boundary**, and the ownership ladder runs:

```
person → yours → core → optin → ext          (+ harness, off to one side)
   ▲                                  ▲
   asks the question                  shortfall cannot see inside this
```

Two consequences worth stating outright, because they surprise people:

- **Your Prometheus and your ledger are `ext`.** They are your infrastructure
  and sit inside your company, but shortfall reaches them only through
  `emit.Exporter` and `query.Querier` and cannot see past that interface.
  For a library, that is exactly what "external" has to mean.
- **`core` and `optin` are two colours, not one**, because the split is the
  central promise of the repo: a Prometheus user never compiles stripe-go.
  Collapsing them into one colour would erase the claim the architecture is
  built to make. The class is defined by the **module boundary**, not by the
  word "adapter" — which is why `cmd/shortfall`, its own module pulling a
  SQLite driver, is `optin` and not `core` from Level 2 down.
- **The `core`/`optin` split is a Level 2 distinction, deliberately.** Level
  1 asks who uses shortfall and what it touches; module boundaries are not
  yet the subject, so shortfall is one `core` box. Splitting it there would
  answer a question the diagram has not asked.

`harness` is the one class where a reader mistaking it for production code
would be an expensive error, so it carries a **dashed stroke as well as a
colour** — the distinction survives a greyscale printout and a red-green
colour vision deficiency.

### Shape is a separate axis

Colour says *who owns it*. Shape says *what kind of thing it is*. They are
independent: a datastore can be `ext` or `yours`, and both are cylinders.

| Shape | Syntax | Kind |
|---|---|---|
| Stadium | `id(["…"])` | A person |
| Rectangle | `id["…"]` | A code unit — package, module, service, component |
| Cylinder | `id[("…")]` | A datastore or a backend that holds data |
| Diamond | `id{"…"}` | A decision or gate in a flow |

---

## 2 · Label grammar

A node label answers three questions without the reader dropping into prose:
**what is it**, **what is it built from**, **what does it do**.

```
id["<b>Name</b><br/><i>technology / module</i><br/>responsibilities"]
```

- **Line 1 — bold name.** The thing as it is spelled in the repo:
  `<b>engine</b>`, `<b>adapters/payment/stripe</b>`. Not a prose paraphrase.
- **Line 2 — italic technology line.** The module, package path, protocol,
  interface, or dependency that makes it what it is. Omit only when the node
  genuinely has none (a human).
- **Line 3+ — plain responsibilities.** What it is *for*, in fragments
  separated by ` · `. Not a sentence, no trailing period.

Use `<br/>` for line breaks and wrap the whole label in quotes so Mermaid
does not choke on punctuation.

**Emoji glyphs** go on every human actor and on the **entry points** — the
actor, the runnable, the call that starts the flow, the node a reader's eye
should land on first. Not on inner components, where a glyph per box is
noise rather than an anchor. One glyph, leading, then a space:
`["⚙️ <b>engine</b><br/>…"]`.

**Keep the arrow labels short.** An edge label is a verb phrase of a few
words — `stage transitions`, `webhooks`. Every constraint, protocol, and
caveat that will not fit belongs in the edge table, not on the arrow.

---

## 3 · `subgraph` marks a real boundary

A `subgraph` is a claim that something is true of everything inside it — a
module boundary, a trust boundary, an ownership boundary. Grouping nodes
because they look tidy together is a misuse: it invites the reader to infer
a boundary that does not exist.

Every `subgraph` declares an explicit `direction`, so the layout is the
author's decision and not the renderer's:

```
subgraph core["shortfall core module — zero heavy deps"]
    direction TB
    ...
end
```

---

## 4 · Required sections

Every diagram file carries these, in this order. A file missing either one
is incomplete, and that is a reviewable finding.

### The edge table

Immediately below the fence. One row per edge that carries a real
constraint, so the arrows stay short and the constraints stay somewhere a
reviewer can check them against the code.

```markdown
| Edge | Protocol / notes |
|---|---|
| your service → `emit` | In-process Go call — `Record()` per stage transition |
| `engine` → query adapter | `query.Querier` interface — the only questions the engine may ask |
```

Name the mechanism: the interface, the header, the wire format, the
enforcement. "Sends data" is not a row worth writing.

### Key facts this diagram encodes

Closing section, `##`-level, headed exactly:

```markdown
## Key facts this diagram encodes
```

Bulleted, each bullet opening with a **bolded claim** and then the evidence
for it. This is where the diagram stops depicting and starts **arguing**:
the reader should be able to read only this section and come away with the
things the picture was drawn to prove. A bullet that merely restates a box
("the engine computes the report") is not a key fact.

---

## 5 · Checklist

A diagram is on-stencil when all of these hold:

- [ ] Every node is in exactly one semantic class, and the class is true
      at this diagram's level of zoom.
- [ ] The `classDef` block is present, verbatim, minus unused classes.
- [ ] Labels follow bold name / italic technology / plain responsibilities.
- [ ] Human actors and entry-point nodes carry a leading emoji glyph.
- [ ] Every `subgraph` is a real boundary and declares a `direction`.
- [ ] Arrow labels are short; the detail is in the edge table.
- [ ] An edge table follows the fence.
- [ ] A `## Key facts this diagram encodes` section closes the file.
- [ ] The fence still parses as valid Mermaid and renders on github.com.

## Related

- [Architecture index](README.md) — the diagram set and the repository layout
- [ADR-0008 — docs tell the truth](../adr/0008-docs-tell-the-truth.md) — a
  diagram is documentation, and the same honesty rule binds it
