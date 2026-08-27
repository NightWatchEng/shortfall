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
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NightWatchEng/shortfall/biz"
)

// Registry is the validated, immutable-after-load configuration.
type Registry struct {
	Version     int
	Segments    []string
	Propagation Propagation
	flows       map[string]Flow

	segmentSet map[string]struct{}
}

// Propagation declares where biz.vc may be injected (deny by default).
type Propagation struct {
	AllowHosts []string
}

// Flow is one business flow's contract.
type Flow struct {
	Name       string
	Money      MoneySpec
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
// realized.
type Estimator struct {
	DefaultMinor int64
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

// Flow returns a flow by name.
func (r Registry) Flow(name string) (Flow, bool) {
	f, ok := r.flows[name]
	return f, ok
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

// HostAllowed reports whether biz.vc may be injected toward host.
// Patterns: exact host, or "*.domain" matching any single-or-deeper
// subdomain of domain (never domain itself, and never a suffix trick
// like "evil-domain").
func (p Propagation) HostAllowed(host string) bool {
	if host == "" {
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

// ---- wire form ----

type registryDoc struct {
	Version     int                `yaml:"version"`
	Segments    []string           `yaml:"segments"`
	Propagation propagationDoc     `yaml:"propagation"`
	Flows       map[string]flowDoc `yaml:"flows"`
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
	if doc.Version != 1 {
		return Registry{}, fmt.Errorf("version %d is not supported (want 1)", doc.Version)
	}

	r := Registry{
		Version:    doc.Version,
		Segments:   doc.Segments,
		flows:      map[string]Flow{},
		segmentSet: map[string]struct{}{},
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
	for i, h := range doc.Propagation.AllowHosts {
		if strings.TrimSpace(h) == "" {
			return Registry{}, fmt.Errorf("propagation.allow_hosts[%d] is empty", i)
		}
	}
	r.Propagation = Propagation{AllowHosts: doc.Propagation.AllowHosts}

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
	for _, c := range fd.Currencies {
		if len(c) != 3 || strings.ToUpper(c) != c || strings.ContainsFunc(c, func(r rune) bool { return r < 'A' || r > 'Z' }) {
			return fail("currencies entry %q is not an ISO 4217 code", c)
		}
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
		f.Stages = append(f.Stages, Stage{Name: sd.Name, Signals: sd.Signals})
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
		f.Estimator = &Estimator{DefaultMinor: fd.Estimator.DefaultMinor, BySegment: fd.Estimator.BySegment}
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
	if fd.Recovery.RecoveredFraction > 0 {
		if fd.Recovery.Within == "" {
			return fail("recovery recovered_fraction is set but within is missing")
		}
		d, err := ParseISODuration(fd.Recovery.Within)
		if err != nil {
			return fail("recovery within: %v", err)
		}
		rec.Within = d
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
