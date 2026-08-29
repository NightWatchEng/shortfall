// Conformance vectors: the language-neutral, executable half of the
// portability contract (docs/portability.md).
//
// The types below are the JSON schema of testkit/vectors/*.json, which a
// Java or Python port loads to check itself against the Go reference
// implementation without linking any Go. Every expected value in those
// files is produced by running the reference implementation
// (`go run ./testkit/cmd/genvectors`), and vectors_test.go replays them
// back through it — so spec and implementation cannot drift apart
// silently in either direction.
//
// One field is deliberately NOT part of the cross-language contract:
// `reference_message` records the Go error text for a rejected input. It
// exists so a wording change is a reviewed diff rather than an invisible
// one; a port must reject the same inputs under the same `error` class
// and is free to word its own diagnostics however it likes.

package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// Where the committed vectors live, relative to the testkit package.
const (
	VectorsDir          = "vectors"
	VCVectorsFile       = "vc-codec.json"
	RegistryVectorsFile = "registry.json"
)

// VectorsVersion is the schema version of the vector files themselves.
// Bump it when the JSON shape changes; a port pins what it understands.
const VectorsVersion = 1

// VCErrorClasses are the biz.vc rejection classes the contract defines.
// Each must be named in docs/portability.md and exercised by at least one
// vector — both enforced in vectors_test.go.
var VCErrorClasses = []string{
	"oversize",
	"deadline_domain",
	"field_count",
	"unknown_version",
	"amount_syntax",
	"amount_range",
	"exponent_syntax",
	"exponent_range",
	"escape_syntax",
	"raw_unescaped_byte",
	"flags_undefined",
}

// RegistryErrorClasses are the flow-registry rejection classes the
// contract defines, under the same two obligations as VCErrorClasses.
var RegistryErrorClasses = []string{
	"yaml_syntax",
	"unknown_field",
	"multi_document",
	"version_unsupported",
	"no_segments",
	"segment_token",
	"segment_duplicate",
	"allow_host_pattern",
	"severity_sev_shape",
	"severity_duplicate",
	"severity_min_per_minute",
	"severity_order",
	"no_flows",
	"flow_name_token",
	"money_kind",
	"no_stages",
	"stage_token",
	"stage_duplicate",
	"stage_no_signals",
	"stage_empty_signal",
	"currency_code",
	"currency_duplicate",
	"sla_unknown_stage",
	"sla_deadline",
	"sla_on_breach",
	"estimator_default_minor",
	"estimator_segment_unknown",
	"estimator_by_segment_value",
	"estimator_exponent_range",
	"baseline_seasonality",
	"baseline_lookback",
	"recovery_model",
	"recovery_fraction",
	"recovery_within_missing",
	"recovery_within_without_fraction",
	"reconcile_source_required",
	"reconcile_source_scheme",
	"reconcile_stage_unknown",
}

// ---- biz.vc codec vectors ----

// VC is the language-neutral rendering of a ValueContext. The deadline
// is unix seconds (0 = none) rather than a formatted timestamp: a
// timestamp string would make every port's date library part of the
// codec contract, which it is not.
//
// amount_minor is a JSON number that can reach the full int64 range. A
// port whose JSON reader backs numbers with a float64 (JavaScript, and
// some JVM defaults) must read this field with a bigint-preserving
// reader, or it will silently fail the very vectors that exist to catch
// silent money corruption.
type VC struct {
	Flow         string `json:"flow"`
	EntityID     string `json:"entity_id"`
	CustomerID   string `json:"customer_id"`
	Segment      string `json:"segment"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	Exponent     int8   `json:"exponent"`
	Kind         string `json:"kind"`
	Estimated    bool   `json:"estimated"`
	DeadlineUnix int64  `json:"deadline_unix"`
}

// ValueContext converts a vector context into the Go type.
func (v VC) ValueContext() biz.ValueContext {
	vc := biz.ValueContext{
		Flow:       v.Flow,
		EntityID:   v.EntityID,
		CustomerID: v.CustomerID,
		Segment:    v.Segment,
		Money:      biz.Money{Amount: v.AmountMinor, Currency: v.Currency, Exponent: v.Exponent},
		Kind:       biz.Kind(v.Kind),
		Estimated:  v.Estimated,
	}
	if v.DeadlineUnix != 0 {
		vc.Deadline = time.Unix(v.DeadlineUnix, 0).UTC()
	}
	return vc
}

// VCOf renders a Go ValueContext in the vector form.
func VCOf(vc biz.ValueContext) VC {
	out := VC{
		Flow:        vc.Flow,
		EntityID:    vc.EntityID,
		CustomerID:  vc.CustomerID,
		Segment:     vc.Segment,
		AmountMinor: vc.Money.Amount,
		Currency:    vc.Money.Currency,
		Exponent:    vc.Money.Exponent,
		Kind:        string(vc.Kind),
		Estimated:   vc.Estimated,
	}
	if !vc.Deadline.IsZero() {
		out.DeadlineUnix = vc.Deadline.Unix()
	}
	return out
}

// EncodeVector is one context and the exact bytes it must encode to.
type EncodeVector struct {
	Name    string `json:"name"`
	Note    string `json:"note,omitempty"`
	VC      VC     `json:"vc"`
	Encoded string `json:"encoded"`
}

// DecodeVector is one wire value a decoder must accept, the context it
// yields, and the canonical encoding of that context — which for a
// non-canonical input differs from the input itself.
type DecodeVector struct {
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	Encoded   string `json:"encoded"`
	VC        VC     `json:"vc"`
	Canonical string `json:"canonical"`
}

// RejectVector is one input that must be rejected. Exactly one of VC
// (encode direction) or Encoded (decode direction) is set.
type RejectVector struct {
	Name             string `json:"name"`
	Note             string `json:"note,omitempty"`
	VC               *VC    `json:"vc,omitempty"`
	Encoded          string `json:"encoded,omitempty"`
	Error            string `json:"error"`
	ReferenceMessage string `json:"reference_message"`
}

// VCVectors is the whole biz.vc vector file.
type VCVectors struct {
	Vectors         string         `json:"vectors"`
	VectorsVersion  int            `json:"vectors_version"`
	MemberKey       string         `json:"member_key"`
	CodecVersion    string         `json:"codec_version"`
	Delimiter       string         `json:"delimiter"`
	FieldOrder      []string       `json:"field_order"`
	MaxEncodedBytes int            `json:"max_encoded_bytes"`
	Encode          []EncodeVector `json:"encode"`
	EncodeReject    []RejectVector `json:"encode_reject"`
	Decode          []DecodeVector `json:"decode"`
	DecodeReject    []RejectVector `json:"decode_reject"`
}

// Names lists every vector name in the file, for the uniqueness guard.
func (v VCVectors) Names() []string {
	names := make([]string, 0, len(v.Encode)+len(v.Decode)+len(v.EncodeReject)+len(v.DecodeReject))
	for _, e := range v.Encode {
		names = append(names, "encode/"+e.Name)
	}
	for _, d := range v.Decode {
		names = append(names, "decode/"+d.Name)
	}
	for _, r := range v.EncodeReject {
		names = append(names, "encode_reject/"+r.Name)
	}
	for _, r := range v.DecodeReject {
		names = append(names, "decode_reject/"+r.Name)
	}
	return names
}

// LoadVCVectors reads a biz.vc vector file.
func LoadVCVectors(path string) (VCVectors, error) {
	var v VCVectors
	if err := readJSON(path, &v); err != nil {
		return VCVectors{}, err
	}
	if v.VectorsVersion != VectorsVersion {
		return VCVectors{}, fmt.Errorf("%s: vectors_version %d, this build understands %d", path, v.VectorsVersion, VectorsVersion)
	}
	return v, nil
}

// ---- flow-registry vectors ----

// SLAFact is one stage's SLA, with the deadline resolved to seconds so
// no port has to agree with Go about duration formatting.
type SLAFact struct {
	DeadlineSeconds int64  `json:"deadline_seconds"`
	OnBreach        string `json:"on_breach"`
}

// StageFact is one declared stage.
type StageFact struct {
	Name    string   `json:"name"`
	Signals []string `json:"signals"`
}

// EstimatorFact is the resolved estimator, exponent defaulted.
type EstimatorFact struct {
	DefaultMinor int64            `json:"default_minor"`
	Exponent     int8             `json:"exponent"`
	BySegment    map[string]int64 `json:"by_segment,omitempty"`
}

// BaselineFact, RecoveryFact and ReconcileFact are the remaining
// per-flow settings a port must derive identically.
type (
	BaselineFact struct {
		Seasonality   string `json:"seasonality"`
		LookbackWeeks int    `json:"lookback_weeks"`
		Holidays      string `json:"holidays,omitempty"`
	}
	RecoveryFact struct {
		Model             string  `json:"model"`
		RecoveredFraction float64 `json:"recovered_fraction"`
		WithinSeconds     int64   `json:"within_seconds"`
	}
	ReconcileFact struct {
		Source string `json:"source"`
		Stage  string `json:"stage,omitempty"`
	}
)

// FlowFact is everything a loaded flow must expose, including the
// derived value stage (ADR-0016) that is not written in the file.
type FlowFact struct {
	Kind       string             `json:"kind"`
	Currencies []string           `json:"currencies,omitempty"`
	Stages     []StageFact        `json:"stages"`
	ValueStage string             `json:"value_stage"`
	SLA        map[string]SLAFact `json:"sla,omitempty"`
	Estimator  *EstimatorFact     `json:"estimator,omitempty"`
	Baseline   BaselineFact       `json:"baseline"`
	Recovery   RecoveryFact       `json:"recovery"`
	Reconcile  ReconcileFact      `json:"reconcile"`
}

// SeverityFact is one rung of the severity ladder.
type SeverityFact struct {
	Sev               string `json:"sev"`
	MinPerMinuteMinor int64  `json:"min_per_minute_minor"`
}

// RegistryFacts is what a conforming loader must derive from a valid
// registry document — the observable output of validation, as opposed to
// whatever in-memory shape an implementation happens to use.
type RegistryFacts struct {
	Version    int                 `json:"version"`
	Segments   []string            `json:"segments"`
	AllowHosts []string            `json:"allow_hosts"`
	Severity   []SeverityFact      `json:"severity,omitempty"`
	Flows      map[string]FlowFact `json:"flows"`
}

// FactsOf derives the observable facts from a loaded registry.
func FactsOf(r registry.Registry) RegistryFacts {
	f := RegistryFacts{
		Version:    r.Version,
		Segments:   slices.Clone(r.Segments),
		AllowHosts: slices.Clone(r.Propagation.AllowHosts),
		Flows:      map[string]FlowFact{},
	}
	for _, s := range r.Severity {
		f.Severity = append(f.Severity, SeverityFact{Sev: s.Sev, MinPerMinuteMinor: s.MinPerMinuteMinor})
	}
	for _, name := range r.FlowNames() {
		flow, ok := r.Flow(name)
		if !ok {
			continue
		}
		ff := FlowFact{
			Kind:       string(flow.Money.Kind),
			Currencies: slices.Clone(flow.Currencies),
			ValueStage: flow.ValueStage(),
			Baseline: BaselineFact{
				Seasonality:   flow.Baseline.Seasonality,
				LookbackWeeks: flow.Baseline.LookbackWeeks,
				Holidays:      flow.Baseline.Holidays,
			},
			Recovery: RecoveryFact{
				Model:             flow.Recovery.Model,
				RecoveredFraction: flow.Recovery.RecoveredFraction,
				WithinSeconds:     int64(flow.Recovery.Within / time.Second),
			},
			Reconcile: ReconcileFact{Source: flow.Reconcile.Source, Stage: flow.Reconcile.Stage},
		}
		for _, st := range flow.Stages {
			ff.Stages = append(ff.Stages, StageFact{Name: st.Name, Signals: slices.Clone(st.Signals)})
		}
		if len(flow.SLA) > 0 {
			ff.SLA = map[string]SLAFact{}
			for stage, sla := range flow.SLA {
				ff.SLA[stage] = SLAFact{
					DeadlineSeconds: int64(sla.Deadline / time.Second),
					OnBreach:        string(sla.OnBreach),
				}
			}
		}
		if flow.Estimator != nil {
			ff.Estimator = &EstimatorFact{
				DefaultMinor: flow.Estimator.DefaultMinor,
				Exponent:     flow.Estimator.Exponent,
				BySegment:    maps.Clone(flow.Estimator.BySegment),
			}
		}
		f.Flows[name] = ff
	}
	return f
}

// Equal compares two fact sets structurally. Comparison runs over the
// JSON rendering so map ordering and nil-vs-empty never masquerade as
// drift — the same reason the scenario goldens round-trip before
// comparing.
func (f RegistryFacts) Equal(other RegistryFacts) bool { return f.JSON() == other.JSON() }

// JSON renders the facts for comparison and for failure messages.
func (f RegistryFacts) JSON() string {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Sprintf("<unmarshalable facts: %v>", err)
	}
	return string(b)
}

// RegistryAcceptVector is one valid document and the facts it yields.
type RegistryAcceptVector struct {
	Name  string        `json:"name"`
	Note  string        `json:"note,omitempty"`
	YAML  string        `json:"yaml"`
	Facts RegistryFacts `json:"facts"`
}

// RegistryRejectVector is one document validation must refuse.
type RegistryRejectVector struct {
	RejectVector
	YAML string `json:"yaml"`
}

// HostVector is one propagation-allowlist match decision (ADR-0003).
type HostVector struct {
	Name       string   `json:"name"`
	Note       string   `json:"note,omitempty"`
	AllowHosts []string `json:"allow_hosts"`
	Host       string   `json:"host"`
	Allowed    bool     `json:"allowed"`
}

// DurationVector is one accepted ISO-8601 duration and its length.
type DurationVector struct {
	Name    string `json:"name"`
	Input   string `json:"input"`
	Seconds int64  `json:"seconds"`
}

// DurationRejectVector is one duration string the subset refuses.
type DurationRejectVector struct {
	Name  string `json:"name"`
	Note  string `json:"note,omitempty"`
	Input string `json:"input"`
}

// RegistryVectors is the whole flow-registry vector file.
type RegistryVectors struct {
	Vectors        string                 `json:"vectors"`
	VectorsVersion int                    `json:"vectors_version"`
	SchemaVersion  int                    `json:"schema_version"`
	Accept         []RegistryAcceptVector `json:"accept"`
	Reject         []RegistryRejectVector `json:"reject"`
	HostAllowlist  []HostVector           `json:"host_allowlist"`
	Duration       []DurationVector       `json:"duration"`
	DurationReject []DurationRejectVector `json:"duration_reject"`
}

// Names lists every vector name in the file, for the uniqueness guard.
func (v RegistryVectors) Names() []string {
	names := make([]string, 0,
		len(v.Accept)+len(v.Reject)+len(v.HostAllowlist)+len(v.Duration)+len(v.DurationReject))
	for _, a := range v.Accept {
		names = append(names, "accept/"+a.Name)
	}
	for _, r := range v.Reject {
		names = append(names, "reject/"+r.Name)
	}
	for _, h := range v.HostAllowlist {
		names = append(names, "host/"+h.Name)
	}
	for _, d := range v.Duration {
		names = append(names, "duration/"+d.Name)
	}
	for _, d := range v.DurationReject {
		names = append(names, "duration_reject/"+d.Name)
	}
	return names
}

// LoadRegistryVectors reads a flow-registry vector file.
func LoadRegistryVectors(path string) (RegistryVectors, error) {
	var v RegistryVectors
	if err := readJSON(path, &v); err != nil {
		return RegistryVectors{}, err
	}
	if v.VectorsVersion != VectorsVersion {
		return RegistryVectors{}, fmt.Errorf("%s: vectors_version %d, this build understands %d", path, v.VectorsVersion, VectorsVersion)
	}
	return v, nil
}

// readJSON decodes a vector file, refusing unknown fields: a vector key
// this build does not understand is a version skew, not a default.
func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
