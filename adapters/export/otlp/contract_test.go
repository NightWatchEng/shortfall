package otlp

import (
	"path/filepath"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/testkit"
)

// TestOutcomeEventContract holds this exporter to the shared wire contract:
// the same Outcome must produce the same biz.* fields here as on every
// other transport (ADR-0002).
//
// The trace id is the one declared difference. OTLP carries it as the log
// record's span context, which is what a trace-aware transport should do,
// so it is absent from the attributes rather than missing — the vector
// lists it under absent for the required-only case and never requires it.
func TestOutcomeEventContract(t *testing.T) {
	v, err := testkit.LoadOutcomeEventVectors(
		filepath.Join("..", "..", "..", "testkit", testkit.VectorsDir, testkit.OutcomeEventVectorsFile))
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	problems := testkit.CheckOutcomeEvent(v, func(o biz.Outcome) (map[string]any, error) {
		m := map[string]any{}
		for _, kv := range outcomeAttrs(o) {
			m[string(kv.Key)] = kv.Value.AsInterface()
		}
		return m, nil
	}, biz.AttrTraceID)
	for _, p := range problems {
		t.Error(p)
	}
}
