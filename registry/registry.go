// Package registry loads and validates the flow registry: the versioned,
// Finance-co-signed YAML that answers up front the five questions
// otherwise relitigated during every incident — what counts as money,
// where the flow's stages live, when deferred becomes lost, what an
// unknown amount is worth, and how much demand returns after recovery.
// It also declares the two fences other layers enforce: the segment
// enumeration (metric cardinality, ADR-0004) and the propagation host
// allowlist (biz.vc egress, ADR-0003).
//
// Validation is loud and names the offending field: a registry error is
// read at 2am by someone who did not write the file.
package registry

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NightWatchEng/shortfall/biz"
)

// Registry is the validated configuration. Treat it as immutable: the
// fence-bearing collections are defensively copied at load, and Flow()
// returns deep copies of a flow's maps and slices — but Go cannot forbid
// a determined caller from mutating what it holds, so "immutable" is a
// contract this package upholds on its side, not a compiler guarantee.
type Registry struct {
	Version     int
	Segments    []string
	Propagation Propagation
	Severity    []SeverityThreshold // ordered most-severe first; empty = no suggestion
	flows       map[string]Flow

	segmentSet map[string]struct{}
}

// SeverityThreshold maps a dollars-per-minute-at-risk floor to a suggested
// severity: a $/min rate at or above MinPerMinuteMinor suggests Sev. Thresholds
// are ordered most-severe first (largest floor first). Floors are minor units
// per minute, at the registry author's currency and exponent — e.g. a flow
// losing $2M/hour is 200,000,000 minor/hour ÷ 60 ≈ 3,333,333 minor/min at
// exponent 2 (USD cents).
type SeverityThreshold struct {
	Sev               string
	MinPerMinuteMinor int64
}

// Propagation declares where biz.vc may be injected (deny by default).
type Propagation struct {
	AllowHosts []string
}

// Flow is one business flow's contract.
type Flow struct {
	Name  string
	Money MoneySpec
	// Currencies optionally declares the currencies this flow expects,
	// bounding metric cardinality at review time (ADR-0004). EMPTY MEANS
	// UNDECLARED: any valid currency is accepted at runtime and the
	// worst-case cardinality bound falls back to observed traffic — it
	// does not mean "no currencies".
	Currencies []string
	Stages     []Stage
	SLA        map[string]SLA
	Estimator  *Estimator
	Baseline   Baseline
	Recovery   Recovery
	Reconcile  Reconcile

	stageSet map[string]struct{}
}

// MoneySpec declares which money definition the flow reports under.
type MoneySpec struct {
	Kind biz.Kind
}

// Stage is a named step with the telemetry signals that feed it.
type Stage struct {
	Name    string
	Signals []string
}

// Breach is what an SLA breach converts deferred value into.
type Breach string

const (
	BreachLost   Breach = "lost"
	BreachAtRisk Breach = "at_risk"
)

// SLA bounds how long a stage may hold value before breach.
type SLA struct {
	Deadline time.Duration
	OnBreach Breach
}

// Estimator supplies amounts when ingress does not know them; the emit
// layer stamps Estimated=true so the engine never reports them as
// realized. Amounts are minor units at Exponent (default 2) — declare it
// to match your flow's currencies (a JPY flow wants Exponent 0).
type Estimator struct {
	DefaultMinor int64
	Exponent     int8
	BySegment    map[string]int64
}

// Baseline configures the counterfactual expectation (ADR-0006).
type Baseline struct {
	Seasonality   string
	LookbackWeeks int
	Holidays      string
}

// Recovery configures the usage-loss curve applied to suppressed demand.
type Recovery struct {
	Model             string
	RecoveredFraction float64
	Within            time.Duration
}

// Reconcile names the ledger source coverage is measured against.
type Reconcile struct {
	Source string
}

// Flow returns a deep copy of a flow by name — callers can never mutate
// the registry's fences through the returned value.
func (r Registry) Flow(name string) (Flow, bool) {
	f, ok := r.flows[name]
	if !ok {
		return Flow{}, false
	}
	out := f
	out.Currencies = append([]string(nil), f.Currencies...)
	out.Stages = make([]Stage, len(f.Stages))
	for i, st := range f.Stages {
		out.Stages[i] = Stage{Name: st.Name, Signals: append([]string(nil), st.Signals...)}
	}
	out.SLA = make(map[string]SLA, len(f.SLA))
	for k, v := range f.SLA {
		out.SLA[k] = v
	}
	if f.Estimator != nil {
		est := Estimator{DefaultMinor: f.Estimator.DefaultMinor, Exponent: f.Estimator.Exponent, BySegment: make(map[string]int64, len(f.Estimator.BySegment))}
		for k, v := range f.Estimator.BySegment {
			est.BySegment[k] = v
		}
		out.Estimator = &est
	}
	return out, true
}

// FlowNames returns the declared flow names (unordered).
func (r Registry) FlowNames() []string {
	names := make([]string, 0, len(r.flows))
	for n := range r.flows {
		names = append(names, n)
	}
	return names
}

// SegmentValid reports whether s is in the declared enumeration.
func (r Registry) SegmentValid(s string) bool {
	_, ok := r.segmentSet[s]
	return ok
}

// StageValid reports whether name is a declared stage of the flow.
func (f Flow) StageValid(name string) bool {
	_, ok := f.stageSet[name]
	return ok
}

// EstimateMinor returns the estimator amount for a segment: the
// by-segment value when declared, the default otherwise. ok is false
// only when the flow declares no estimator at all.
func (f Flow) EstimateMinor(segment string) (int64, bool) {
	if f.Estimator == nil {
		return 0, false
	}
	if v, ok := f.Estimator.BySegment[segment]; ok {
		return v, true
	}
	return f.Estimator.DefaultMinor, true
}

// EstimateMoney returns the estimated Money for a segment in the given
// currency, at the estimator's declared exponent — so an estimate is
// never applied under a mismatched exponent (a JPY/0 stamp cannot inherit
// a USD/2 estimator's 100x error).
func (f Flow) EstimateMoney(segment, currency string) (biz.Money, bool) {
	amt, ok := f.EstimateMinor(segment)
	if !ok {
		return biz.Money{}, false
	}
	return biz.Money{Amount: amt, Currency: currency, Exponent: f.Estimator.Exponent}, true
}

// HostAllowed reports whether biz.vc may be injected toward host.
// Patterns: exact host, or "*.domain" matching any single-or-deeper
// subdomain of domain (never domain itself, and never a suffix trick
// like "evil-domain"). Input contract: pass a bare hostname — no port,
// no trailing dot (use url.URL.Hostname()); case is normalized here, but
// a ported or dotted input is DENIED, not cleaned. An empty allowlist
// denies everything: that is the deny-by-default the ADR mandates.
func (p Propagation) HostAllowed(host string) bool {
	host = strings.ToLower(host)
	if !validHostShape(host) {
		return false
	}
	for _, pat := range p.AllowHosts {
		if wild, ok := strings.CutPrefix(pat, "*."); ok {
			if strings.HasSuffix(host, "."+wild) && len(host) > len(wild)+1 {
				return true
			}
			continue
		}
		if host == pat {
			return true
		}
	}
	return false
}

// validHostShape rejects anything that is not a bare lowercase DNS name:
// empty labels ("x..y"), leading/trailing dots, ports, paths, whitespace.
// Malformed hosts are denied, never cleaned — cleaning is how "evil.com."
// sneaks past an allowlist.
func validHostShape(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
			if !ok || ((c == '-') && (i == 0 || i == len(label)-1)) {
				return false
			}
		}
	}
	return true
}

// ---- wire form ----

type registryDoc struct {
	Version     int                `yaml:"version"`
	Segments    []string           `yaml:"segments"`
	Propagation propagationDoc     `yaml:"propagation"`
	Severity    []severityDoc      `yaml:"severity,omitempty"`
	Flows       map[string]flowDoc `yaml:"flows"`
}

type severityDoc struct {
	Sev          string `yaml:"sev"`
	MinPerMinute int64  `yaml:"min_per_minute"`
}

type propagationDoc struct {
	AllowHosts []string `yaml:"allow_hosts"`
}

type flowDoc struct {
	Money      moneyDoc          `yaml:"money"`
	Currencies []string          `yaml:"currencies,omitempty"`
	Stages     []stageDoc        `yaml:"stages"`
	SLA        map[string]slaDoc `yaml:"sla,omitempty"`
	Estimator  *estimatorDoc     `yaml:"estimator,omitempty"`
	Baseline   baselineDoc       `yaml:"baseline"`
	Recovery   recoveryDoc       `yaml:"recovery"`
	Reconcile  reconcileDoc      `yaml:"reconcile"`
}

type moneyDoc struct {
	Kind string `yaml:"kind"`
}

type stageDoc struct {
	Name    string   `yaml:"name"`
	Signals []string `yaml:"signals"`
}

type slaDoc struct {
	Deadline string `yaml:"deadline"`
	OnBreach string `yaml:"on_breach"`
}

type estimatorDoc struct {
	DefaultMinor int64            `yaml:"default_minor"`
	Exponent     *int8            `yaml:"exponent,omitempty"`
	BySegment    map[string]int64 `yaml:"by_segment,omitempty"`
}

type baselineDoc struct {
	Seasonality   string `yaml:"seasonality"`
	LookbackWeeks int    `yaml:"lookback_weeks"`
	Holidays      string `yaml:"holidays,omitempty"`
}

type recoveryDoc struct {
	Model             string  `yaml:"model"`
	RecoveredFraction float64 `yaml:"recovered_fraction"`
	Within            string  `yaml:"within,omitempty"`
}

type reconcileDoc struct {
	Source string `yaml:"source"`
}

// Load reads and validates a registry file.
func Load(path string) (Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, fmt.Errorf("registry %s: %w", path, err)
	}
	r, err := Parse(raw)
	if err != nil {
		return Registry{}, fmt.Errorf("registry %s: %w", path, err)
	}
	return r, nil
}

// Parse validates registry YAML bytes. Unknown fields are errors: a
// typoed knob must fail, not silently default.
func Parse(raw []byte) (Registry, error) {
	var doc registryDoc
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return Registry{}, fmt.Errorf("parse: %w", err)
	}
	// A second document after '---' would otherwise be silently ignored —
	// and a silently ignored document is a silently ignored typo.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Registry{}, fmt.Errorf("parse: registry must be a single YAML document (content found after ---)")
	}
	if doc.Version != 1 {
		return Registry{}, fmt.Errorf("version %d is not supported (want 1)", doc.Version)
	}

	r := Registry{
		Version:    doc.Version,
		flows:      map[string]Flow{},
		segmentSet: map[string]struct{}{},
	}
	if len(doc.Segments) == 0 {
		return Registry{}, fmt.Errorf("at least one segment must be declared — the enumeration is the metric-cardinality fence (ADR-0004)")
	}
	for _, s := range doc.Segments {
		if err := token(s, 32); err != nil {
			return Registry{}, fmt.Errorf("segment %q: %w", s, err)
		}
		if _, dup := r.segmentSet[s]; dup {
			return Registry{}, fmt.Errorf("segment %q declared twice", s)
		}
		r.segmentSet[s] = struct{}{}
	}
	// Defensive copy: Segments is exported and must not alias the doc.
	r.Segments = append([]string(nil), doc.Segments...)

	// Allowlist patterns are the egress fence (ADR-0003): malformed ones
	// must fail at load, not become near-allow-all ("*.") or silently
	// dead (uppercase, whitespace) at match time.
	hosts := make([]string, 0, len(doc.Propagation.AllowHosts))
	for i, h := range doc.Propagation.AllowHosts {
		bare, _ := strings.CutPrefix(h, "*.")
		if h == "*" || h == "*." || bare == "" || !validHostShape(bare) {
			return Registry{}, fmt.Errorf("propagation.allow_hosts[%d] %q is not a bare lowercase DNS name or *.domain pattern", i, h)
		}
		hosts = append(hosts, h)
	}
	r.Propagation = Propagation{AllowHosts: hosts}

	// Severity thresholds (optional): a $/min-at-risk ladder, most-severe first.
	// Each floor must be positive and STRICTLY DECREASING, so "the highest sev
	// whose floor a rate clears" is unambiguous; a duplicate sev or an
	// out-of-order floor is a config error, not a silent tie.
	seen := map[string]struct{}{}
	for i, sd := range doc.Severity {
		// A severity name is a display label (SEV1, P1, Critical) — not a metric
		// token — so it allows mixed case, but must be a single non-empty,
		// bounded, whitespace-free word (it lands in the report and a pager).
		if sd.Sev == "" || len(sd.Sev) > 32 || strings.ContainsFunc(sd.Sev, func(r rune) bool { return r <= ' ' || r == 127 }) {
			return Registry{}, fmt.Errorf("severity[%d].sev %q must be a non-empty word of at most 32 characters with no whitespace", i, sd.Sev)
		}
		if _, dup := seen[sd.Sev]; dup {
			return Registry{}, fmt.Errorf("severity[%d]: sev %q declared twice", i, sd.Sev)
		}
		seen[sd.Sev] = struct{}{}
		if sd.MinPerMinute <= 0 {
			return Registry{}, fmt.Errorf("severity[%d] (%s): min_per_minute %d must be positive minor units", i, sd.Sev, sd.MinPerMinute)
		}
		if i > 0 && sd.MinPerMinute >= doc.Severity[i-1].MinPerMinute {
			return Registry{}, fmt.Errorf("severity[%d] (%s): min_per_minute %d must be strictly less than the previous threshold %d (order most-severe first)", i, sd.Sev, sd.MinPerMinute, doc.Severity[i-1].MinPerMinute)
		}
		r.Severity = append(r.Severity, SeverityThreshold{Sev: sd.Sev, MinPerMinuteMinor: sd.MinPerMinute})
	}

	if len(doc.Flows) == 0 {
		return Registry{}, fmt.Errorf("no flows declared")
	}
	for name, fd := range doc.Flows {
		f, err := buildFlow(name, fd, r.segmentSet)
		if err != nil {
			return Registry{}, err
		}
		r.flows[name] = f
	}
	return r, nil
}

func buildFlow(name string, fd flowDoc, segments map[string]struct{}) (Flow, error) {
	fail := func(format string, args ...any) (Flow, error) {
		return Flow{}, fmt.Errorf("flow %s: %s", name, fmt.Sprintf(format, args...))
	}
	if err := token(name, 64); err != nil {
		return Flow{}, fmt.Errorf("flow name %q: %w", name, err)
	}
	kind := biz.Kind(fd.Money.Kind)
	if !kind.Valid() {
		return fail("money.kind %q is not one of gmv, net_revenue, fee, take_rate", fd.Money.Kind)
	}
	if len(fd.Stages) == 0 {
		return fail("at least one stage is required")
	}

	f := Flow{
		Name:       name,
		Money:      MoneySpec{Kind: kind},
		Currencies: fd.Currencies,
		SLA:        map[string]SLA{},
		stageSet:   map[string]struct{}{},
	}
	seenCur := map[string]struct{}{}
	for _, c := range fd.Currencies {
		if len(c) != 3 || strings.ToUpper(c) != c || strings.ContainsFunc(c, func(r rune) bool { return r < 'A' || r > 'Z' }) {
			return fail("currencies entry %q is not an ISO 4217 code", c)
		}
		if _, dup := seenCur[c]; dup {
			return fail("currencies entry %q declared twice", c)
		}
		seenCur[c] = struct{}{}
	}
	for i, sd := range fd.Stages {
		if err := token(sd.Name, 32); err != nil {
			return fail("stages[%d] name %q: %v", i, sd.Name, err)
		}
		if _, dup := f.stageSet[sd.Name]; dup {
			return fail("stage %q declared twice", sd.Name)
		}
		if len(sd.Signals) == 0 {
			return fail("stage %q declares no signals", sd.Name)
		}
		for j, sig := range sd.Signals {
			if strings.TrimSpace(sig) == "" {
				return fail("stage %q signals[%d] is empty", sd.Name, j)
			}
		}
		f.stageSet[sd.Name] = struct{}{}
		f.Stages = append(f.Stages, Stage(sd))
	}
	for stage, sd := range fd.SLA {
		if !f.StageValid(stage) {
			return fail("sla names unknown stage %q", stage)
		}
		d, err := ParseISODuration(sd.Deadline)
		if err != nil {
			return fail("sla %s deadline: %v", stage, err)
		}
		switch Breach(sd.OnBreach) {
		case BreachLost, BreachAtRisk:
		default:
			return fail("sla %s on_breach %q is not lost or at_risk", stage, sd.OnBreach)
		}
		f.SLA[stage] = SLA{Deadline: d, OnBreach: Breach(sd.OnBreach)}
	}
	if fd.Estimator != nil {
		if fd.Estimator.DefaultMinor <= 0 {
			return fail("estimator default_minor %d must be positive minor units", fd.Estimator.DefaultMinor)
		}
		for seg, v := range fd.Estimator.BySegment {
			if _, ok := segments[seg]; !ok {
				return fail("estimator by_segment names %q, which is outside the declared segment enumeration", seg)
			}
			if v <= 0 {
				return fail("estimator by_segment[%s] %d must be positive minor units", seg, v)
			}
		}
		exponent := int8(2)
		if fd.Estimator.Exponent != nil {
			exponent = *fd.Estimator.Exponent
			if exponent < 0 || exponent > 4 {
				return fail("estimator exponent %d outside [0, 4]", exponent)
			}
		}
		f.Estimator = &Estimator{DefaultMinor: fd.Estimator.DefaultMinor, Exponent: exponent, BySegment: fd.Estimator.BySegment}
	}
	if fd.Baseline.Seasonality != "hour_of_week" {
		return fail("baseline seasonality %q is not supported (hour_of_week)", fd.Baseline.Seasonality)
	}
	if fd.Baseline.LookbackWeeks < 1 {
		return fail("baseline lookback_weeks %d must be >= 1", fd.Baseline.LookbackWeeks)
	}
	f.Baseline = Baseline{Seasonality: fd.Baseline.Seasonality, LookbackWeeks: fd.Baseline.LookbackWeeks, Holidays: fd.Baseline.Holidays}

	if fd.Recovery.Model != "usage_loss_curve" {
		return fail("recovery model %q is not supported (usage_loss_curve)", fd.Recovery.Model)
	}
	if fd.Recovery.RecoveredFraction < 0 || fd.Recovery.RecoveredFraction > 1 {
		return fail("recovery recovered_fraction %v outside [0, 1]", fd.Recovery.RecoveredFraction)
	}
	rec := Recovery{Model: fd.Recovery.Model, RecoveredFraction: fd.Recovery.RecoveredFraction}
	switch {
	case fd.Recovery.RecoveredFraction > 0:
		if fd.Recovery.Within == "" {
			return fail("recovery recovered_fraction is set but within is missing")
		}
		d, err := ParseISODuration(fd.Recovery.Within)
		if err != nil {
			return fail("recovery within: %v", err)
		}
		rec.Within = d
	case fd.Recovery.Within != "":
		// The iff holds in both directions: a within with no fraction is
		// a typo, and typos fail loudly here.
		return fail("recovery within %q is set but recovered_fraction is 0 — remove one or set both", fd.Recovery.Within)
	}
	f.Recovery = rec

	src := strings.TrimSpace(fd.Reconcile.Source)
	if src == "" {
		return fail("reconcile source is required — coverage is how Finance comes to trust the numbers")
	}
	scheme, _, ok := strings.Cut(src, ":")
	if !ok || (scheme != "sql" && scheme != "stripe") {
		return fail("reconcile source %q must use a known scheme (sql: or stripe:)", src)
	}
	f.Reconcile = Reconcile{Source: src}

	return f, nil
}

// token enforces the bounded lowercase name shape shared with biz.
func token(s string, maxLen int) error {
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
