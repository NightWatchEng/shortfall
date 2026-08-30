package biz

// The serialized names of an Outcome's fields — the outcome event's wire
// contract, in one place.
//
// These existed only as string literals repeated across every exporter and
// every querier that reads one back. Nothing tied the copies together, so
// drift was not a mistake anyone made, it was the default: ADR-0002's
// canonical JSON, docs/semconv.md and the shipped exporters ended up
// spelling the same five facts three different ways. Naming them once is
// what makes the contract a thing the compiler can check rather than a
// thing three documents describe.
//
// The spelling is the one the exporters already ship, so adopting these
// constants moved no bytes on any wire (ADR-0002, amended). They are
// exported because an out-of-tree adapter needs the same names to be
// conformant, and testkit/vectors/outcome-event.json pins them for a port
// that cannot import Go.
//
// Changing a value here is a wire-format break for every consumer of every
// exporter. It is an ADR amendment, not an edit.
const (
	// EventKey and EventOutcome mark the record as this library's, so a
	// shared sink can tell an outcome event from everything else in it.
	EventKey     = "event"
	EventOutcome = "biz.outcome"

	AttrFlow    = "biz.flow"
	AttrStage   = "biz.stage"
	AttrOutcome = "biz.outcome"

	// AttrEntityID is the de-dup key. AttrCustomerID is the hashed handle
	// (h:...); neither may ever ride a metric label (ADR-0004).
	AttrEntityID   = "biz.entity.id"
	AttrCustomerID = "biz.customer.id"
	AttrSegment    = "biz.segment"

	// The money facts. Amount is integer minor units and Exponent is what
	// makes it unambiguous, so the two are meaningless apart (ADR-0001).
	AttrAmountMinor = "biz.amount_minor"
	AttrCurrency    = "biz.currency"
	AttrExponent    = "biz.exponent"
	AttrValueKind   = "biz.value.kind"
	AttrAmountEst   = "biz.amount.est"

	// AttrSLADeadline is optional: present only when the ValueContext
	// carries a deadline. Every transport that can express it emits it —
	// ADR-0002 requires the transports to agree on the fields a given
	// Outcome produces, and until workspace-cnz only OTLP did.
	AttrSLADeadline = "biz.sla.deadline"

	// Diagnostic fields, all optional. TraceID is carried natively by
	// transports that have a trace concept (OTLP puts it on the log
	// record's span context) and as this attribute by those that do not.
	AttrSource  = "source"
	AttrError   = "error"
	AttrTraceID = "trace.id"
)

// OutcomeEventAttrs lists every attribute name an outcome event can carry,
// in the order the vector records them. A conformance test walks this to
// prove an exporter emits the whole set and nothing outside it, which is
// the check ADR-0002 claimed existed before workspace-cnz added it.
var OutcomeEventAttrs = []string{
	AttrFlow, AttrStage, AttrOutcome,
	AttrEntityID, AttrCustomerID, AttrSegment,
	AttrAmountMinor, AttrCurrency, AttrExponent, AttrValueKind, AttrAmountEst,
	AttrSLADeadline,
	AttrSource, AttrError, AttrTraceID,
}
