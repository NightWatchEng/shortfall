---
id: blank-line-after-block
severity: MEDIUM
engine: claude
applies_to: ["**/*.go"]
---
The judged residue of ADR-0014's blank-line clause, and only the residue.

The clause itself — a blank line after the `}` that closes an `if`,
`for`/`range`, `switch`, type switch or `select` when more code follows at
that level, excepting `else`/`else if`, `case`/`default`, the enclosing
`}`, and a block written on one line — is NOT judged here. It is enforced
deterministically by `test/blankline`, which parses every tracked `.go`
file with `go/parser` and fails the core test step naming `file:line`. A
two-line relationship is not expressible as a single-line regex, so this
rule cannot be `engine: declarative`; and re-judging what the checker
already decides would only add a slower, less reliable second opinion.
Do not flag a missing blank line here. The checker has it.

What the checker cannot see is over-application. It requires the blank
line unconditionally, which is right for the common case and wrong for a
tight sequential group — a run of guards that reads as one operation, now
broken into a paragraph each. Flag, in this diff only:

- A run of three or more consecutive same-shaped statements (typically
  `if <assign>; err != nil { return ... }` over successive fields,
  elements or arguments) where the required blank lines turn one logical
  step into a column of one-item paragraphs.

The remedy is never to delete the blank line — the standard is absolute
and the checker will fail the build. It is to restructure so the group is
one statement: a loop over a table of field/decoder pairs, an extracted
helper that returns on first error, or a single combined condition.
Evidence: quote the run and say what it decodes, validates or builds.

Do NOT flag: two adjacent guards, guards that check unrelated things,
a run whose bodies differ in shape, or any blank line the checker
required — that is the standard working, not a finding.

Scope, stated honestly: this rule carries no corpus counts. It is a new
prevention rule shipped alongside the clause, not a promotion from
repeated judged findings, and it is expected to fire rarely. The instance
that justifies shipping it at all is real and in the tree: the six
consecutive `if vc.X, err = <decode>(fields[N]); err != nil` guards in
`DecodeVC` (`biz/baggage.go`) — five calling `unescape`, one calling
`strconv.ParseInt` — where the sweep that applied the clause split one
"read fields 1 through 6" step into six paragraphs. The six bodies are the
same shape — decode, and return a wrapped error naming the field — so the
run does not meet the "bodies differ in shape" exclusion above, even
though the decoders and the messages differ. It is left as it stands:
restructuring the money codec is not a whitespace PR's business, and it
is the worked example of what this rule is for.
