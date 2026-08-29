// Command genvectors regenerates the committed conformance vectors —
// the language-neutral, executable half of docs/portability.md. Run from
// the repo root:
//
//	go run ./testkit/cmd/genvectors
//
// The case inputs live here; every expected output is produced by
// running the reference implementation, so a vector can never claim
// behaviour the Go code does not have. The generator refuses to write
// when a case does not do what it was authored to do — a rejection case
// that is accepted, an acceptance case that is rejected, a substitution
// that matched nothing — because a vector suite that silently degrades
// into agreement is worse than none.
//
// Regeneration is deliberate: testkit's vector tests replay the
// committed files, so an intentional wire or validator change shows up
// as a reviewable diff here and in the contract, in the same PR
// (ADR-0008).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

func main() {
	if _, err := os.Stat(filepath.Join("testkit", "vectors_test.go")); err != nil {
		fmt.Fprintln(os.Stderr, "genvectors: run from the repo root")
		os.Exit(2)
	}
	dir := filepath.Join("testkit", testkit.VectorsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(err)
	}
	write(filepath.Join(dir, testkit.VCVectorsFile), buildVCVectors())
	write(filepath.Join(dir, testkit.RegistryVectorsFile), buildRegistryVectors())
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "genvectors: %v\n", err)
	os.Exit(2)
}

func failf(format string, args ...any) { fail(fmt.Errorf(format, args...)) }

func write(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", path, len(b)+1)
}

// ---- biz.vc codec ----

func unixOf(rfc3339 string) int64 {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		fail(err)
	}
	return t.Unix()
}

// referenceVC is the worked example docs/portability.md walks through.
func referenceVC() testkit.VC {
	return testkit.VC{
		Flow:         "invoice.pay",
		EntityID:     "inv_8Ka92j",
		CustomerID:   "h:3f9ac2",
		Segment:      "smb",
		AmountMinor:  14900,
		Currency:     "USD",
		Exponent:     2,
		Kind:         "fee",
		Estimated:    true,
		DeadlineUnix: unixOf("2026-08-27T14:32:00Z"),
	}
}

func buildVCVectors() testkit.VCVectors {
	esc := referenceVC()
	esc.EntityID = "a|b%c\"d;e,f\\g"
	esc.CustomerID = "h:1 2\t3"

	utf8VC := referenceVC()
	utf8VC.EntityID = "inv_café"
	utf8VC.CustomerID = "h:naïve"

	jpy := referenceVC()
	jpy.Currency, jpy.Exponent, jpy.Kind, jpy.Estimated = "JPY", 0, "gmv", false

	maxAmt := referenceVC()
	maxAmt.AmountMinor, maxAmt.Estimated = 9223372036854775807, false

	maxDeadline := referenceVC()
	maxDeadline.DeadlineUnix = unixOf("3000-01-01T00:00:00Z")

	bare := testkit.VC{
		Flow: "checkout", EntityID: "ord_1", CustomerID: "", Segment: "",
		AmountMinor: 0, Currency: "USD", Exponent: 2, Kind: "gmv",
		Estimated: false, DeadlineUnix: 0,
	}

	encodeCases := []struct {
		name, note string
		vc         testkit.VC
	}{
		{"reference", "the worked example in docs/portability.md", referenceVC()},
		{"no_optional_fields", "empty customer, empty segment, no deadline, flags 0", bare},
		{"escaping_all_reserved", "every byte the escape rule covers: delimiter, escape, baggage-forbidden, space, control", esc},
		{"utf8_escaped_bytewise", "escaping is over UTF-8 BYTES, not code points or UTF-16 units", utf8VC},
		{"jpy_exponent_zero", "a zero-exponent currency: 14900 means ¥14900, not ¥149.00", jpy},
		{"int64_max_amount", "the largest amount the wire admits", maxAmt},
		{"deadline_max", "the last encodable deadline instant", maxDeadline},
	}
	var encode []testkit.EncodeVector
	for _, c := range encodeCases {
		enc, err := biz.EncodeVC(c.vc.ValueContext())
		if err != nil {
			failf("encode case %s was expected to encode: %v", c.name, err)
		}
		back, err := biz.DecodeVC(enc)
		if err != nil {
			failf("encode case %s does not round trip: %v", c.name, err)
		}
		if testkit.VCOf(back) != c.vc {
			failf("encode case %s round-trips to a different context", c.name)
		}
		encode = append(encode, testkit.EncodeVector{Name: c.name, Note: c.note, VC: c.vc, Encoded: enc})
	}

	oversize := referenceVC()
	oversize.EntityID = strings.Repeat("%", 128)
	oversize.CustomerID = strings.Repeat("%", 128)
	oversize.Flow = strings.Repeat("f", 64)

	preEpoch := referenceVC()
	preEpoch.DeadlineUnix = -1

	pastYear3000 := referenceVC()
	pastYear3000.DeadlineUnix = unixOf("3000-01-01T00:00:00Z") + 1

	encodeRejectCases := []struct {
		name, note, class string
		vc                testkit.VC
	}{
		{"oversize", "escaping inflates: 128 percent signs are 384 bytes per field", "oversize", oversize},
		{"deadline_before_epoch", "0 means \"no deadline\", so the encodable domain is (1970, 3000]", "deadline_domain", preEpoch},
		{"deadline_past_year_3000", "an instant a decoder would refuse must not be encodable", "deadline_domain", pastYear3000},
	}
	var encodeReject []testkit.RejectVector
	for _, c := range encodeRejectCases {
		vc := c.vc
		_, err := biz.EncodeVC(vc.ValueContext())
		if err == nil {
			failf("encode rejection case %s was accepted", c.name)
		}
		encodeReject = append(encodeReject, testkit.RejectVector{
			Name: c.name, Note: c.note, VC: &vc,
			Error: c.class, ReferenceMessage: err.Error(),
		})
	}

	decodeCases := []struct {
		name, note, in string
	}{
		{
			"non_canonical_escape_of_safe_byte",
			"a decoder accepts %XX for a byte that needs no escape; re-encoding shrinks it back to canonical",
			"1|checkout|%69nv1|%68:c|smb|100|USD|2|fee|0|0",
		},
		{
			"zero_padded_amount_and_signed_exponent",
			"integer fields are parsed, not pattern-matched; the canonical form has neither padding nor a sign",
			"1|checkout|ord_1|h:c|smb|0014900|USD|+2|fee|0|0",
		},
		{
			"negative_amount_passes_the_codec",
			"the codec reports what the wire carried; a negative amount is rejected by validation, not by decoding",
			"1|checkout|ord_1|h:c|smb|-1|USD|2|fee|0|0",
		},
		{
			"exponent_outside_semantic_range_passes_the_codec",
			"exponent 7 decodes; [0,4] is a validation rule, not a wire rule",
			"1|checkout|ord_1|h:c|smb|1|USD|7|fee|0|0",
		},
		{
			"undeclared_kind_passes_the_codec",
			"an unknown kind decodes; the kind enumeration is enforced by validation",
			"1|checkout|ord_1|h:c|smb|1|USD|2|mrr|0|0",
		},
		{
			"all_optional_fields_empty",
			"empty customer, segment, currency and kind are four empty fields, not absent ones",
			"1|f|e|||0||0||0|0",
		},
	}
	var decode []testkit.DecodeVector
	for _, c := range decodeCases {
		vc, err := biz.DecodeVC(c.in)
		if err != nil {
			failf("decode case %s was expected to decode: %v", c.name, err)
		}
		canon, err := biz.EncodeVC(vc)
		if err != nil {
			failf("decode case %s does not re-encode: %v", c.name, err)
		}
		if len(canon) > len(c.in) {
			failf("decode case %s re-encodes longer than its input", c.name)
		}
		decode = append(decode, testkit.DecodeVector{
			Name: c.name, Note: c.note, Encoded: c.in, VC: testkit.VCOf(vc), Canonical: canon,
		})
	}

	canonical, err := biz.EncodeVC(referenceVC().ValueContext())
	if err != nil {
		fail(err)
	}
	decodeRejectCases := []struct {
		name, note, class, in string
	}{
		{"empty_input", "an empty value is one field, not a default context", "field_count", ""},
		{"too_few_fields", "ten fields", "field_count", "1|f|e|c|s|0|USD|2|fee|0"},
		{"too_many_fields", "a version-2 producer's extra field is not silently ignored", "field_count", canonical + "|extra"},
		{"unknown_version", "decoders reject versions they do not know rather than guessing", "unknown_version", "2" + canonical[1:]},
		{"amount_not_a_number", "", "amount_syntax", "1|f|e|c|s|14x00|USD|2|fee|0|0"},
		{
			"amount_past_int64",
			"the int64 range is a WIRE rule an implementation enforces, not a language property it inherits",
			"amount_range", "1|f|e|c|s|9223372036854775808|USD|2|fee|0|0",
		},
		{"amount_past_int64_negative", "", "amount_range", "1|f|e|c|s|-9223372036854775809|USD|2|fee|0|0"},
		{"exponent_not_a_number", "", "exponent_syntax", "1|f|e|c|s|1|USD|x|fee|0|0"},
		{"exponent_past_int8", "the wire field is a signed 8-bit integer", "exponent_range", "1|f|e|c|s|1|USD|300|fee|0|0"},
		{"bad_hex_digits", "", "escape_syntax", "1|f|%zz|c|s|1|USD|2|fee|0|0"},
		{"truncated_escape", "", "escape_syntax", "1|f|ab%7|c|s|1|USD|2|fee|0|0"},
		{
			"lowercase_hex_escape",
			"escapes are UPPERCASE hex only — the single likeliest thing a port gets wrong",
			"escape_syntax", "1|f|%7c|c|s|1|USD|2|fee|0|0",
		},
		{"raw_space", "", "raw_unescaped_byte", "1|f|a b|c|s|1|USD|2|fee|0|0"},
		{"raw_quote", "", "raw_unescaped_byte", "1|f|a\"b|c|s|1|USD|2|fee|0|0"},
		{"flags_undefined_bit", "version 1 defines flag values 0 and 1 only", "flags_undefined", "1|f|e|c|s|1|USD|2|fee|2|0"},
		{"deadline_negative", "", "deadline_domain", "1|f|e|c|s|1|USD|2|fee|0|-5"},
		{"deadline_past_year_3000", "a peer cannot smuggle an overflowing instant into SLA math", "deadline_domain", "1|f|e|c|s|1|USD|2|fee|0|9223372036854775807"},
		{"oversize", "the cap is checked before parsing", "oversize", "1|" + strings.Repeat("a", 600)},
	}
	var decodeReject []testkit.RejectVector
	for _, c := range decodeRejectCases {
		_, err := biz.DecodeVC(c.in)
		if err == nil {
			failf("decode rejection case %s was accepted", c.name)
		}
		decodeReject = append(decodeReject, testkit.RejectVector{
			Name: c.name, Note: c.note, Encoded: c.in,
			Error: c.class, ReferenceMessage: err.Error(),
		})
	}

	return testkit.VCVectors{
		Vectors:        "biz.vc",
		VectorsVersion: testkit.VectorsVersion,
		MemberKey:      biz.MemberKey,
		CodecVersion:   strings.SplitN(canonical, "|", 2)[0],
		Delimiter:      "|",
		FieldOrder: []string{
			"version", "flow", "entity_id", "customer_id", "segment",
			"amount_minor", "currency", "exponent", "kind", "flags", "deadline_unix",
		},
		MaxEncodedBytes: biz.MaxEncodedBytes,
		Encode:          encode,
		EncodeReject:    encodeReject,
		Decode:          decode,
		DecodeReject:    decodeReject,
	}
}

// ---- flow registry ----

// baseYAML is the reference registry every mutation below starts from —
// deliberately the shape docs/registry.md documents, so a rejection
// vector differs from a valid file by exactly one edit.
//
// It is split at the flows boundary only so the no_flows case can be
// that same one edit rather than a hand-written second document that
// would differ in four places at once (review finding, workspace-ldf).
const baseHeaderYAML = `version: 1
segments: [smb, enterprise]
propagation:
  allow_hosts: ["*.internal.example.com", "api.example.com"]
severity:
  - { sev: SEV1, min_per_minute: 100000 }
  - { sev: SEV2, min_per_minute: 10000 }
`

const baseYAML = baseHeaderYAML + `flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }
    estimator: { default_minor: 18750, exponent: 2, by_segment: { smb: 14200 } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 8, holidays: us }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.6, within: PT2H }
    reconcile: { source: "sql:ledger.payments", stage: capture }
`

// minimalYAML is the smallest document that validates: no severity
// ladder, no estimator, no declared currencies, no SLA, no propagation
// block (an absent allowlist denies every host), and a value stage
// derived from the last declared stage.
const minimalYAML = `version: 1
segments: [default]
flows:
  checkout:
    money: { kind: gmv }
    stages:
      - { name: pay, signals: ["http:POST /pay"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "stripe:charges" }
`

// noFlowsYAML is baseYAML with its flows block — and nothing else —
// replaced by an empty mapping.
const noFlowsYAML = baseHeaderYAML + "flows: {}\n"

// sub applies one-for-one substitutions to baseYAML, failing when a
// pattern matches nothing (a silently no-op mutation would produce a
// "rejection" vector that is really the valid document).
func sub(pairs ...string) string {
	if len(pairs)%2 != 0 {
		failf("sub: odd argument count")
	}
	out := baseYAML
	for i := 0; i < len(pairs); i += 2 {
		old, new := pairs[i], pairs[i+1]
		if !strings.Contains(out, old) {
			failf("sub: %q is not in the base registry", old)
		}
		if strings.Count(out, old) != 1 {
			failf("sub: %q appears %d times in the base registry", old, strings.Count(out, old))
		}
		out = strings.Replace(out, old, new, 1)
	}
	return out
}

func buildRegistryVectors() testkit.RegistryVectors {
	acceptCases := []struct {
		name, note, yaml string
	}{
		{"reference", "the shape docs/registry.md documents", baseYAML},
		{"minimal", "every optional block omitted; value_stage falls back to the last stage (ADR-0016)", minimalYAML},
		{
			"zero_exponent_estimator",
			"a JPY flow's estimator declares exponent 0, so an estimate cannot inherit a 100x error",
			sub("currencies: [USD]", "currencies: [JPY]", "exponent: 2", "exponent: 0"),
		},
	}
	var accept []testkit.RegistryAcceptVector
	for _, c := range acceptCases {
		reg, err := registry.Parse([]byte(c.yaml))
		if err != nil {
			failf("acceptance case %s was rejected: %v", c.name, err)
		}
		accept = append(accept, testkit.RegistryAcceptVector{
			Name: c.name, Note: c.note, YAML: c.yaml, Facts: testkit.FactsOf(reg),
		})
	}

	rejectCases := []struct {
		name, note, class, yaml string
	}{
		{"yaml_syntax", "", "yaml_syntax", sub("segments: [smb, enterprise]", "segments: [smb, enterprise")},
		{"unknown_field", "a typoed knob must fail, never silently default", "unknown_field", sub("version: 1\n", "version: 1\nbogus: true\n")},
		{"multi_document", "a second document after --- is a silently ignored typo", "multi_document", baseYAML + "---\nversion: 1\n"},
		{"version_unsupported", "", "version_unsupported", sub("version: 1\n", "version: 2\n")},
		{"no_segments", "the enumeration is the metric-cardinality fence (ADR-0004)", "no_segments", sub("segments: [smb, enterprise]", "segments: []")},
		{"segment_token", "", "segment_token", sub("segments: [smb, enterprise]", "segments: [SMB]")},
		{"segment_duplicate", "", "segment_duplicate", sub("segments: [smb, enterprise]", "segments: [smb, smb]")},
		{
			"allow_host_pattern",
			"a malformed pattern must fail at load, not become near-allow-all at match time (ADR-0003)",
			"allow_host_pattern",
			sub(`["*.internal.example.com", "api.example.com"]`, `["*."]`),
		},
		{"severity_sev_shape", "", "severity_sev_shape", sub("sev: SEV2", `sev: ""`)},
		{"severity_duplicate", "", "severity_duplicate", sub("sev: SEV2", "sev: SEV1")},
		{"severity_min_per_minute", "", "severity_min_per_minute", sub("min_per_minute: 10000 }", "min_per_minute: 0 }")},
		{"severity_order", "thresholds are strictly decreasing, so \"highest sev cleared\" is unambiguous", "severity_order", sub("min_per_minute: 10000 }", "min_per_minute: 100000 }")},
		{"no_flows", "", "no_flows", noFlowsYAML},
		{"flow_name_token", "", "flow_name_token", sub("  invoice.pay:", "  Invoice.Pay:")},
		{"money_kind", "", "money_kind", sub("money: { kind: fee }", "money: { kind: revenue }")},
		{"no_stages", "", "no_stages", sub("stages:\n      - { name: auth,    signals: [\"http:POST /pay\"] }\n      - { name: capture, signals: [\"queue:capture.q\"] }\n", "stages: []\n")},
		{"stage_token", "", "stage_token", sub("name: auth,", "name: AUTH,")},
		{"stage_duplicate", "", "stage_duplicate", sub("{ name: capture,", "{ name: auth,")},
		{"stage_no_signals", "", "stage_no_signals", sub(`signals: ["queue:capture.q"]`, "signals: []")},
		{"stage_empty_signal", "", "stage_empty_signal", sub(`signals: ["queue:capture.q"]`, `signals: ["  "]`)},
		{"currency_code", "", "currency_code", sub("currencies: [USD]", "currencies: [usd]")},
		{"currency_duplicate", "", "currency_duplicate", sub("currencies: [USD]", "currencies: [USD, USD]")},
		{"sla_unknown_stage", "", "sla_unknown_stage", sub("      capture: { deadline: PT30M", "      settle: { deadline: PT30M")},
		{"sla_deadline", "durations are the ISO-8601 subset, never Go duration strings", "sla_deadline", sub("deadline: PT30M", "deadline: 30m")},
		{"sla_on_breach", "", "sla_on_breach", sub("on_breach: lost", "on_breach: gone")},
		{"estimator_default_minor", "", "estimator_default_minor", sub("default_minor: 18750", "default_minor: 0")},
		{"estimator_segment_unknown", "an estimator may not invent a segment outside the enumeration", "estimator_segment_unknown", sub("by_segment: { smb: 14200 }", "by_segment: { vip: 14200 }")},
		{"estimator_by_segment_value", "", "estimator_by_segment_value", sub("by_segment: { smb: 14200 }", "by_segment: { smb: 0 }")},
		{"estimator_exponent_range", "", "estimator_exponent_range", sub("exponent: 2", "exponent: 9")},
		{"baseline_seasonality", "", "baseline_seasonality", sub("seasonality: hour_of_week", "seasonality: day_of_week")},
		{"baseline_lookback", "", "baseline_lookback", sub("lookback_weeks: 8", "lookback_weeks: 0")},
		{"recovery_model", "", "recovery_model", sub("model: usage_loss_curve", "model: linear")},
		{"recovery_fraction", "", "recovery_fraction", sub("recovered_fraction: 0.6", "recovered_fraction: 1.5")},
		{"recovery_within_missing", "", "recovery_within_missing", sub(", within: PT2H }", " }")},
		{"recovery_within_without_fraction", "the iff holds in both directions", "recovery_within_without_fraction", sub("recovered_fraction: 0.6,", "recovered_fraction: 0,")},
		{"reconcile_source_required", "coverage is how Finance comes to trust the numbers", "reconcile_source_required", sub(`source: "sql:ledger.payments"`, `source: ""`)},
		{"reconcile_source_scheme", "", "reconcile_source_scheme", sub(`source: "sql:ledger.payments"`, `source: "mongo:ledger.payments"`)},
		{"reconcile_stage_unknown", "", "reconcile_stage_unknown", sub("stage: capture }", "stage: settle }")},
	}
	var reject []testkit.RegistryRejectVector
	for _, c := range rejectCases {
		_, err := registry.Parse([]byte(c.yaml))
		if err == nil {
			failf("rejection case %s was accepted", c.name)
		}
		reject = append(reject, testkit.RegistryRejectVector{
			RejectVector: testkit.RejectVector{
				Name: c.name, Note: c.note, Error: c.class, ReferenceMessage: err.Error(),
			},
			YAML: c.yaml,
		})
	}

	internal := []string{"*.internal.example.com", "api.example.com"}
	hostCases := []testkit.HostVector{
		{Name: "exact", AllowHosts: internal, Host: "api.example.com", Allowed: true},
		{Name: "wildcard_one_label", AllowHosts: internal, Host: "svc.internal.example.com", Allowed: true},
		{Name: "wildcard_deeper", AllowHosts: internal, Host: "a.b.internal.example.com", Allowed: true},
		{Name: "uppercase_is_normalized", AllowHosts: internal, Host: "API.EXAMPLE.COM", Allowed: true},
		{
			Name: "wildcard_excludes_the_apex", Note: "*.domain never matches domain itself",
			AllowHosts: internal, Host: "internal.example.com", Allowed: false,
		},
		{
			Name: "suffix_trick", Note: "the label boundary is part of the match",
			AllowHosts: internal, Host: "evil-internal.example.com", Allowed: false,
		},
		{Name: "unlisted_host", AllowHosts: internal, Host: "notapi.example.com", Allowed: false},
		{
			Name: "exact_entry_grants_no_subdomains", AllowHosts: internal,
			Host: "x.api.example.com", Allowed: false,
		},
		{
			Name: "trailing_dot", Note: "malformed hosts are denied, never cleaned",
			AllowHosts: internal, Host: "api.example.com.", Allowed: false,
		},
		{
			Name: "port_included", Note: "the input contract is a bare hostname",
			AllowHosts: internal, Host: "api.example.com:443", Allowed: false,
		},
		{Name: "empty_host", AllowHosts: internal, Host: "", Allowed: false},
		{
			Name: "empty_allowlist_denies_everything", Note: "deny by default is the ADR-0003 mandate",
			AllowHosts: []string{}, Host: "api.example.com", Allowed: false,
		},
	}
	for _, h := range hostCases {
		p := registry.Propagation{AllowHosts: h.AllowHosts}
		if got := p.HostAllowed(h.Host); got != h.Allowed {
			failf("host case %s: implementation says %v, the vector claims %v", h.Name, got, h.Allowed)
		}
	}

	durationCases := []testkit.DurationVector{
		{Name: "minutes", Input: "PT30M", Seconds: 1800},
		{Name: "one_day", Input: "P1D", Seconds: 86400},
		{Name: "hours_and_minutes", Input: "PT1H30M", Seconds: 5400},
		{Name: "all_units", Input: "P1DT2H3M4S", Seconds: 93784},
		{Name: "one_second", Input: "PT1S", Seconds: 1},
		{Name: "ten_years_exactly", Input: "P3650D", Seconds: 315360000},
	}
	for _, d := range durationCases {
		got, err := registry.ParseISODuration(d.Input)
		if err != nil {
			failf("duration case %s was rejected: %v", d.Name, err)
		}
		if int64(got.Seconds()) != d.Seconds {
			failf("duration case %s: implementation says %ds, the vector claims %ds", d.Name, int64(got.Seconds()), d.Seconds)
		}
	}

	durationRejectCases := []testkit.DurationRejectVector{
		{Name: "empty", Input: ""},
		{Name: "designator_only", Input: "P"},
		{Name: "time_designator_empty", Input: "P1DT"},
		{Name: "go_duration", Note: "the commonest wrong guess", Input: "30m"},
		{Name: "unit_missing", Input: "PT30"},
		{Name: "months", Note: "a month's length depends on when you ask", Input: "P1M"},
		{Name: "years", Input: "P1Y"},
		{Name: "units_out_of_order", Input: "PT1M1H"},
		{Name: "past_ten_years", Input: "P3651D"},
		{Name: "zero", Note: "durations are strictly positive", Input: "PT0S"},
		{Name: "negative", Input: "PT-5M"},
		{Name: "lowercase", Input: "p1d"},
	}
	for _, d := range durationRejectCases {
		if _, err := registry.ParseISODuration(d.Input); err == nil {
			failf("duration rejection case %s was accepted", d.Name)
		}
	}

	return testkit.RegistryVectors{
		Vectors:        "registry",
		VectorsVersion: testkit.VectorsVersion,
		SchemaVersion:  1,
		Accept:         accept,
		Reject:         reject,
		HostAllowlist:  hostCases,
		Duration:       durationCases,
		DurationReject: durationRejectCases,
	}
}
