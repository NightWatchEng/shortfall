package checkout

import (
	"fmt"
	"math"
	"time"
)

// FaultKind names the four degradation loci: API-level failures,
// API-level latency (abandonment), queue consumer stalls, and upstream
// blackouts (traffic never enters).
type FaultKind string

const (
	FaultAPI5xx        FaultKind = "api-5xx"
	FaultAPILatency    FaultKind = "api-latency"
	FaultConsumerStall FaultKind = "queue-consumer-stall"
	FaultBlackout      FaultKind = "upstream-blackout"
)

// Queue names a stalled stage.
type Queue string

const (
	QueueCapture Queue = "capture"
	QueueSettle  Queue = "settle"
)

// FaultSpec is one active degradation over [From, To) — the declarative
// form used both in Go and as a row in the scenario YAML. Fields beyond
// the window apply per kind and are validated by Validate.
type FaultSpec struct {
	Kind FaultKind `yaml:"kind"`
	From time.Time `yaml:"from"`
	To   time.Time `yaml:"to"`

	// Rate: api-5xx failure probability, or api-latency abandonment
	// probability, in (0, 1]. Semantics when faults overlap: each active
	// fault rolls independently per transaction, so two 0.5 faults
	// compound to ~0.75 — overlapping specs model overlapping causes.
	// Abandonment short-circuits the 5xx roll (an abandoned request never
	// reached the api), which makes rng consumption outcome-dependent;
	// deterministic under a seed either way.
	Rate float64 `yaml:"rate,omitempty"`

	// Queue: which stage a queue-consumer-stall halts.
	Queue Queue `yaml:"queue,omitempty"`

	// Blackout recovery: the fraction of suppressed demand that returns
	// after the outage, spread uniformly over the RecoveryWithin window.
	// Zero fraction means the demand is simply gone.
	RecoveredFraction float64       `yaml:"recovered_fraction,omitempty"`
	RecoveryWithin    time.Duration `yaml:"recovery_within,omitempty"`
}

// Validate rejects malformed fault specs loudly — a broken scenario must
// never produce plausible-looking ground truth.
func (f FaultSpec) Validate() error {
	if !f.From.Before(f.To) {
		return fmt.Errorf("fault %s: window [%v, %v) is empty or inverted", f.Kind, f.From, f.To)
	}
	// The simulation is a minute grid: off-grid boundaries would activate
	// on rounded minutes while freeze math used exact sub-minute lengths,
	// leaking off-grid timestamps into ground truth.
	if !f.From.Equal(f.From.Truncate(time.Minute)) || !f.To.Equal(f.To.Truncate(time.Minute)) {
		return fmt.Errorf("fault %s: window [%v, %v) must be minute-aligned", f.Kind, f.From, f.To)
	}
	switch f.Kind {
	case FaultAPI5xx, FaultAPILatency:
		if math.IsNaN(f.Rate) || math.IsInf(f.Rate, 0) {
			return fmt.Errorf("fault %s: rate %v is not a finite number", f.Kind, f.Rate)
		}
		if f.Rate <= 0 || f.Rate > 1 {
			return fmt.Errorf("fault %s: rate %v outside (0, 1]", f.Kind, f.Rate)
		}
	case FaultConsumerStall:
		if f.Queue != QueueCapture && f.Queue != QueueSettle {
			return fmt.Errorf("fault %s: queue %q must be capture or settle", f.Kind, f.Queue)
		}
	case FaultBlackout:
		// Finiteness before the bound, for the reason registry.Parse gives:
		// NaN fails both halves of a range written as two comparisons, and
		// then fails the > 0 test below as well, so it would slip through
		// unvalidated into the ground-truth ledger.
		if math.IsNaN(f.RecoveredFraction) || math.IsInf(f.RecoveredFraction, 0) {
			return fmt.Errorf("fault %s: recovered_fraction %v is not a finite number", f.Kind, f.RecoveredFraction)
		}
		if f.RecoveredFraction < 0 || f.RecoveredFraction > 1 {
			return fmt.Errorf("fault %s: recovered_fraction %v outside [0, 1]", f.Kind, f.RecoveredFraction)
		}
		if f.RecoveredFraction > 0 && f.RecoveryWithin <= 0 {
			return fmt.Errorf("fault %s: recovered_fraction set but recovery_within missing", f.Kind)
		}
	default:
		return fmt.Errorf("unknown fault kind %q", f.Kind)
	}
	return nil
}

func (f FaultSpec) active(t time.Time) bool {
	return !t.Before(f.From) && t.Before(f.To)
}
