package cloudwatch

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/testkit"
)

// TestOutcomeEventContract holds this exporter to the shared wire contract:
// the same Outcome must produce the same biz.* fields here as on every
// other transport (ADR-0002).
func TestOutcomeEventContract(t *testing.T) {
	v, err := testkit.LoadOutcomeEventVectors(
		filepath.Join("..", "..", "..", "testkit", testkit.VectorsDir, testkit.OutcomeEventVectorsFile))
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	problems := testkit.CheckOutcomeEvent(v, func(o biz.Outcome) (map[string]any, error) {
		b, err := buildEventRecord(o)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		return m, nil
	})
	for _, p := range problems {
		t.Error(p)
	}
}
