// Package checkout is the synthetic reference system: three services
// (api, capture-worker, settle-worker) joined by in-memory queues, seeded
// traffic on an hour-of-week curve, and an in-memory ledger that serves as
// ground truth for the engine.
//
// The simulation is discrete-event on a virtual clock (minute resolution,
// single-threaded): determinism under a seed is structural, not a property
// tests hope for. All fixtures produced here are synthetic by construction
// — never derived from real data.
package checkout

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// State is a transaction's lifecycle state in the ledger.
type State string

const (
	StateCreated  State = "created"        // arrived, auth not yet attempted
	StateAuthed   State = "authed"         // auth ok, waiting in capture queue
	StateCaptured State = "captured"       // capture ok, waiting in settle queue
	StateSettled  State = "settled"        // terminal success
	StateAuthFail State = "failed_auth"    // terminal: rejected at the api
	StateCapFail  State = "failed_capture" // terminal: capture-worker rejected
)

// Segment mirrors the two customer segments the proposal's registry
// enumerates.
type Segment string

const (
	SegmentSMB        Segment = "smb"
	SegmentEnterprise Segment = "enterprise"
)

// Txn is one synthetic invoice payment — a ledger row. Amounts are int64
// minor units; time fields are zero until the stage happens.
type Txn struct {
	ID          string
	CustomerID  string // pre-hashed form, e.g. "h:c000042"
	Segment     Segment
	AmountMinor int64
	Currency    string

	CreatedAt  time.Time
	AuthedAt   time.Time
	CapturedAt time.Time
	SettledAt  time.Time

	State State
}

// Ledger is the ground truth: every synthetic transaction ever created,
// in creation order.
type Ledger struct {
	Txns []Txn
}

// Config drives one simulation run. The zero values of the tuning knobs
// are replaced by defaults in Run.
type Config struct {
	Seed  uint64
	Start time.Time // aligned down to the minute
	End   time.Time

	// Curve is the hour-of-week arrival-rate curve (mean arrivals per
	// minute for each of the 168 hours, Monday 00:00 = index 0). Nil means
	// DefaultCurve.
	Curve *[168]float64

	// EnterpriseFraction is the probability a customer is enterprise
	// (default 0.1).
	EnterpriseFraction float64

	// Customers is the size of the synthetic customer pool (default 3000).
	Customers int

	// Service times (defaults: capture 2 min, settle 5 min). Every
	// transaction spends this long in the queue stage before its worker
	// completes it — a simple, fully deterministic pipeline delay.
	CaptureDelayMin int
	SettleDelayMin  int
}

// Result of a run: the ground-truth ledger plus the effective config.
type Result struct {
	Ledger Ledger
	Config Config
}

func (c *Config) applyDefaults() {
	if c.Curve == nil {
		curve := DefaultCurve()
		c.Curve = &curve
	}
	if c.EnterpriseFraction == 0 {
		c.EnterpriseFraction = 0.1
	}
	if c.Customers == 0 {
		c.Customers = 3000
	}
	if c.CaptureDelayMin == 0 {
		c.CaptureDelayMin = 2
	}
	if c.SettleDelayMin == 0 {
		c.SettleDelayMin = 5
	}
}

// Run executes the simulation and returns the ground-truth ledger.
// Identical Config (including Seed) yields a byte-identical ledger.
func Run(cfg Config) Result {
	cfg.applyDefaults()
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))

	start := cfg.Start.Truncate(time.Minute)
	end := cfg.End.Truncate(time.Minute)

	var ledger Ledger
	n := 0
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		rate := cfg.Curve[hourOfWeek(t)]
		arrivals := poisson(rng, rate)
		for i := 0; i < arrivals; i++ {
			n++
			txn := newTxn(rng, cfg, n, t)

			// auth at the api: instantaneous in the fault-free baseline.
			txn.AuthedAt = t
			txn.State = StateAuthed

			// capture-worker completes after the pipeline delay, then
			// settle-worker after its own — unless the window ends first,
			// in which case the transaction is left in flight at the
			// state it truthfully reached.
			capAt := t.Add(time.Duration(cfg.CaptureDelayMin) * time.Minute)
			if capAt.Before(end) {
				txn.CapturedAt = capAt
				txn.State = StateCaptured
				setAt := capAt.Add(time.Duration(cfg.SettleDelayMin) * time.Minute)
				if setAt.Before(end) {
					txn.SettledAt = setAt
					txn.State = StateSettled
				}
			}
			ledger.Txns = append(ledger.Txns, txn)
		}
	}
	return Result{Ledger: ledger, Config: cfg}
}

func newTxn(rng *rand.Rand, cfg Config, n int, t time.Time) Txn {
	seg := SegmentSMB
	// Amounts in minor units, integer arithmetic only: SMB around $142,
	// enterprise around $910, both with wide spreads.
	amount := int64(4200) + rng.Int64N(20000)
	if rng.Float64() < cfg.EnterpriseFraction {
		seg = SegmentEnterprise
		amount = int64(31000) + rng.Int64N(120000)
	}
	return Txn{
		ID:          fmt.Sprintf("inv_%08d", n),
		CustomerID:  fmt.Sprintf("h:c%06d", rng.IntN(cfg.Customers)),
		Segment:     seg,
		AmountMinor: amount,
		Currency:    "USD",
		CreatedAt:   t,
		State:       StateCreated,
	}
}

// hourOfWeek maps a time to 0..167 with Monday 00:00 as index 0, in UTC.
func hourOfWeek(t time.Time) int {
	t = t.UTC()
	day := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return day*24 + t.Hour()
}

// poisson draws a Poisson-distributed arrival count via Knuth's method —
// exact for the small per-minute rates the curve produces, and fully
// deterministic under the run's rng.
func poisson(rng *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	l := math.Exp(-lambda)
	k := 0
	p := 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
