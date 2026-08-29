# Diagram stencil

Every diagram in this directory is drawn to the stencil below. It exists so
that "does this diagram match the others?" is a question with an answer:
a reviewer can point at a node and call its colour, its label, or a missing
section a **defect**, the same way they can call a float in `biz/` a defect.

The stencil is adapted from the [C4 model](https://c4model.com)'s conventional
palette. shortfall is a **library**, not a deployed system, so the classes
carry a library's distinctions rather than a deployment's — see
[What the colours mean here](#what-the-colours-mean-here).

**Scope:** every diagram in this directory, and any diagram added to it —
the three C4 diagrams and the three money-path sequences. Sections 1–4
below are the C4 half; [section 5](#5--sequences-the-same-grammar-a-different-renderer)
is the sequence half. A sequence has no `classDef` surface, so it takes the
label grammar, the step table and the key-facts section, and skips the
palette — the differences are enumerated there rather than left to taste.

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

A sequence diagram carries the same section in the same place, keyed on the
`autonumber` step instead of on a node pair — see
[§5.6](#56--the-step-table-replaces-the-edge-table).

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

## 5 · Sequences: the same grammar, a different renderer

A C4 diagram answers *where*; a sequence answers *when*, and *in what order*.
The two families have to read as one system, so a sequence keeps the label
grammar and both required sections. It does not keep the palette, and it
**cannot** keep the markup — the sections below say exactly what carries
over and what is replaced.

### 5.1 · `autonumber`, always

The first line inside the fence, no exceptions:

```
sequenceDiagram
    autonumber
```

The numbers are not decoration: they are the **row keys of the step table**.
Without them a table row cannot be pointed at, and a reviewer checking a
claim against the code has to count arrows to find the one being described.

### 5.2 · No markup in labels — the grammar survives, the tags do not

Mermaid renders sequence participant labels, notes and messages as plain SVG
text. `<b>` and `<i>` come out **as literal angle brackets**, and Markdown
`**bold**` comes out as literal asterisks. Only `<br/>` works. That is a
property of the renderer, verified by rendering rather than assumed, so the
C4 grammar transfers by **line position** instead of by weight:

```
participant EM as emit.Std<br/>core module — Record buffers, Flush exports
```

- **Line 1 — the name**, spelled as the repo spells it. Where a C4 node
  bolds it, a sequence puts it first and alone.
- **Line 2 — the technology / module line.** Where a C4 node italicises it,
  a sequence puts it second.
- **No third line.** A C4 node states its responsibilities because nothing
  else on the page does; a sequence participant's responsibilities are the
  messages on its own lifeline.

Human actors use Mermaid's `actor` keyword — the sequence equivalent of the
stadium shape — and, together with the entry point, carry the leading emoji
glyph. Inner participants do not.

Copying `<b>`/`<i>` in from a C4 label is the one mistake this section
exists to prevent: it renders as visible punctuation, and it looks fine in
the source.

**No semicolons in message or note text.** A C4 label is quoted, so `;` is
just a character there. Sequence text is unquoted, and `;` **terminates the
statement** — the rest of the line is then parsed as a new one and the fence
fails to render. Use an em dash or a full stop.

### 5.3 · `box` marks a boundary, exactly as `subgraph` does

Group participants in a `box` only where a real boundary runs between them —
a process, a module, a trust boundary — and name the boundary in the label.
Same rule as [§3](#3--subgraph-marks-a-real-boundary), same reason: a `box`
is a claim about everything inside it.

**A sequence has no ownership palette, deliberately.** Mermaid gives a
sequence no `classDef` surface, so a participant cannot be coloured by who
owns it; only the `box` around a group can be filled, and the groups here
are processes, not ownership bands. Asserting the palette on a `box` would
make an ownership claim about participants that do not all share one class.
So sequences use **one neutral tint for every band**, and the ownership fact
lives in the technology line and the step table, where it can be stated
precisely.

The tint is always low-alpha `rgba`, never opaque `rgb`:

```
box rgba(140,140,140,0.10) Your api service — one process
```

Opaque fills are banned because GitHub renders these diagrams in both a light
and a dark theme, and a solid pale fill puts pale text on a pale ground in
one of them. A 10% tint bands the diagram in both.

### 5.4 · `rect` is a phase; `Note over` is a guarantee

Two constructs, two jobs, and keeping them apart is what lets a reader skim
a long flow:

- **`rect` — a phase.** A contiguous run of steps belonging to one stage of
  the flow, in the same neutral tint as a `box`. Every `rect` opens with a
  `Note over` naming the phase, so the band says what it is:
  `Note over API,K: Phase 2 — the queue hop`.
- **`Note over` — a guarantee.** A property that holds, worded as a claim a
  reader could go and check, placed over exactly the participants it binds.
  Not a caption for the arrow above it, and not somewhere to park detail that
  belongs in the step table.

The guarantees are why these diagrams exist. Draw the fence where the
guarantee starts and stops holding — an egress allowlist, a de-dup key, an
unavailable marker — instead of leaving it as prose under the fence, where
it is neither placed nor scoped.

### 5.5 · `alt` / `opt` only for a branch that is in the code

A branch drawn is a branch asserted, and the reviewer should be able to find
its `if`. `alt … else` is right when the code really takes one of two paths
(an allowlisted host versus any other host; an events-capable backend versus
a metrics-only one). It is wrong as a way to show two things that both
happen — those are two steps.

### 5.6 · The step table replaces the edge table

Same section, same position — immediately below the fence — keyed on the
`autonumber` step instead of on a node pair:

```markdown
| # | Step | Mechanism / constraint |
|---|---|---|
| 3 | `api` → `emit` | `em.Record(ctx, "auth", biz.ResultSuccess)` — buffered, not exported here |
```

One row per **numbered step that carries a real constraint**. Notes and
phase bands are not numbered and do not get rows; if a note needs defending,
it belongs in the key facts. `## Key facts this diagram encodes` closes the
file unchanged.

---

## 6 · Checklist

A diagram is on-stencil when all of these hold.

**Every diagram:**

- [ ] Labels name the thing as the repo spells it, then what it is built
      from — bold/italic in C4, line 1 / line 2 in a sequence.
- [ ] Human actors and entry points carry a leading emoji glyph; inner
      participants and components do not.
- [ ] Arrow and message labels are short; the detail is in the table.
- [ ] An edge table (C4) or step table (sequence) follows the fence.
- [ ] A `## Key facts this diagram encodes` section closes the file.
- [ ] The fence parses as valid Mermaid and **renders** — checked by
      rendering it, not by reading it.

**C4 diagrams additionally:**

- [ ] Every node is in exactly one semantic class, and the class is true
      at this diagram's level of zoom.
- [ ] The `classDef` block is present, verbatim, minus unused classes.
- [ ] Every `subgraph` is a real boundary and declares a `direction`.

**Sequence diagrams additionally:**

- [ ] `autonumber` is the first line inside the fence.
- [ ] No `<b>`, `<i>` or `**bold**` anywhere in the fence — only `<br/>`.
- [ ] No `;` in any message or note text.
- [ ] Every `box` is a real boundary, named, and tinted with the one
      low-alpha neutral; no opaque `rgb`.
- [ ] Every `rect` is a phase and opens with a `Note over` naming it.
- [ ] Every free `Note over` states a guarantee and spans exactly the
      participants it binds.
- [ ] Every `alt` / `opt` corresponds to a branch that exists in the code.

## Related

- [Architecture index](README.md) — the diagram set and the repository layout
- [ADR-0008 — docs tell the truth](../adr/0008-docs-tell-the-truth.md) — a
  diagram is documentation, and the same honesty rule binds it
