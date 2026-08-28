package biz

import (
	"fmt"
	"time"
)

// Kind is the money definition a flow reports under — the thing Finance
// argues about, made explicit (registry-declared per flow).
type Kind string

const (
	KindGMV        Kind = "gmv"
	KindNetRevenue Kind = "net_revenue"
	KindFee        Kind = "fee"
	KindTakeRate   Kind = "take_rate"
)

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	switch k {
	case KindGMV, KindNetRevenue, KindFee, KindTakeRate:
		return true
	}
	return false
}

// Result is the terminal state of a stage transition.
type Result string

const (
	ResultSuccess   Result = "success"
	ResultFailed    Result = "failed"
	ResultDeferred  Result = "deferred"
	ResultAbandoned Result = "abandoned"
	ResultUnknown   Result = "unknown"
)

// Valid reports whether r is a declared result.
func (r Result) Valid() bool {
	switch r {
	case ResultSuccess, ResultFailed, ResultDeferred, ResultAbandoned, ResultUnknown:
		return true
	}
	return false
}

// ValueContext is the business context that propagates with a request:
// which flow, which entity, whose money, how much. CustomerID arrives
// already hashed by the caller — this library never sees a raw account
// id, and the PII guard enforces that contract mechanically.
type ValueContext struct {
	Flow       string // registry flow name, bounded cardinality
	EntityID   string // invoice/order id — events only, never metrics
	CustomerID string // hashed account id — events only, never metrics
	Segment    string // registry-enumerated segment; may be empty
	Money      Money
	Kind       Kind
	Estimated  bool      // amount came from a registry estimator, not the transaction
	Deadline   time.Time // optional SLA deadline for deferred→lost conversion
}

const (
	maxFlowLen    = 64
	maxStageLen   = 32
	maxIDLen      = 128
	maxSegmentLen = 32
)

// lowerToken enforces the bounded-cardinality name shape: lowercase
// letters, digits, and . _ - separators.
func lowerToken(s string, maxLen int) error {
	if s == "" || len(s) > maxLen {
		return fmt.Errorf("length %d outside [1, %d]", len(s), maxLen)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("character %q not in [a-z0-9._-]", r)
		}
	}
	return nil
}

// idToken bounds identifier fields: printable ASCII, no spaces.
func idToken(s string, maxLen int) error {
	if len(s) > maxLen {
		return fmt.Errorf("length %d exceeds %d", len(s), maxLen)
	}
	for _, r := range s {
		if r <= ' ' || r > '~' {
			return fmt.Errorf("character %q outside printable ASCII", r)
		}
	}
	return nil
}

// Validate rejects malformed or PII-carrying context. The PII guard runs
// on EntityID and CustomerID: a PAN, email, or IBAN smuggled into an id
// field would flow into every event sink this library exports to.
func (vc ValueContext) Validate() error {
	if err := lowerToken(vc.Flow, maxFlowLen); err != nil {
		return fmt.Errorf("biz: flow %q: %w", vc.Flow, err)
	}
	if vc.EntityID == "" {
		return fmt.Errorf("biz: entity id is required")
	}
	if err := idToken(vc.EntityID, maxIDLen); err != nil {
		return fmt.Errorf("biz: entity id: %w", err)
	}
	if err := idToken(vc.CustomerID, maxIDLen); err != nil {
		return fmt.Errorf("biz: customer id: %w", err)
	}
	if vc.Segment != "" {
		if err := lowerToken(vc.Segment, maxSegmentLen); err != nil {
			return fmt.Errorf("biz: segment %q: %w", vc.Segment, err)
		}
	}
	if !vc.Kind.Valid() {
		return fmt.Errorf("biz: kind %q is not declared", vc.Kind)
	}
	if err := vc.Money.Validate(); err != nil {
		return err
	}
	if err := rejectPII("entity id", vc.EntityID); err != nil {
		return err
	}
	if err := rejectPII("customer id", vc.CustomerID); err != nil {
		return err
	}
	return nil
}

// Outcome is one terminal stage transition for one transaction — the
// per-transaction event that money accounting rides on, emitted
// regardless of trace sampling.
type Outcome struct {
	At      time.Time
	VC      ValueContext
	Stage   string
	Result  Result
	Source  string // e.g. "stripe:webhook", "httpmw"
	TraceID string // W3C trace id (32 lowercase hex) when a trace exists; never load-bearing
	Err     string // short failure description; PII-guarded free text, max 512 bytes
}

const maxErrLen = 512

// Validate rejects malformed outcomes. Every string field is covered:
// Stage by charset, Source by charset + the PII guard, Err (free text —
// the field upstream error strings echo card numbers into) by length +
// the PII guard, and TraceID by shape (32 lowercase hex admits no PII by
// construction).
func (o Outcome) Validate() error {
	if o.At.IsZero() {
		return fmt.Errorf("biz: outcome time is zero")
	}
	if err := lowerToken(o.Stage, maxStageLen); err != nil {
		return fmt.Errorf("biz: stage %q: %w", o.Stage, err)
	}
	if !o.Result.Valid() {
		return fmt.Errorf("biz: result %q is not declared", o.Result)
	}
	if o.Source != "" {
		if err := idToken(o.Source, maxIDLen); err != nil {
			return fmt.Errorf("biz: source: %w", err)
		}
		if err := CheckPII("source", o.Source); err != nil {
			return err
		}
	}
	if o.TraceID != "" {
		if len(o.TraceID) != 32 {
			return fmt.Errorf("biz: trace id must be 32 lowercase hex characters")
		}
		for _, r := range o.TraceID {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return fmt.Errorf("biz: trace id must be 32 lowercase hex characters")
			}
		}
	}
	if len(o.Err) > maxErrLen {
		return fmt.Errorf("biz: err text %d bytes exceeds %d", len(o.Err), maxErrLen)
	}
	if o.Err != "" {
		if err := CheckPII("err", o.Err); err != nil {
			return err
		}
	}
	return o.VC.Validate()
}
