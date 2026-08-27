package checkout

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is a named, file-loadable simulation configuration — the unit
// the testkit runs and the golden fixtures are written in.
type Scenario struct {
	Name   string
	Config Config
}

// scenarioDoc is the YAML wire form. Durations are strings ("45m", "2h")
// because yaml.v3 has no native time.Duration; timestamps are RFC 3339.
type scenarioDoc struct {
	Name  string    `yaml:"name"`
	Seed  uint64    `yaml:"seed"`
	Start time.Time `yaml:"start"`
	End   time.Time `yaml:"end"`

	EnterpriseFraction    float64 `yaml:"enterprise_fraction,omitempty"`
	Customers             int     `yaml:"customers,omitempty"`
	CaptureDelayMin       int     `yaml:"capture_delay_min,omitempty"`
	SettleDelayMin        int     `yaml:"settle_delay_min,omitempty"`
	CaptureCapacityPerMin int     `yaml:"capture_capacity_per_min,omitempty"`
	SettleCapacityPerMin  int     `yaml:"settle_capacity_per_min,omitempty"`

	Faults []faultDoc `yaml:"faults,omitempty"`
}

type faultDoc struct {
	Kind              FaultKind `yaml:"kind"`
	From              time.Time `yaml:"from"`
	To                time.Time `yaml:"to"`
	Rate              float64   `yaml:"rate,omitempty"`
	Queue             Queue     `yaml:"queue,omitempty"`
	RecoveredFraction float64   `yaml:"recovered_fraction,omitempty"`
	RecoveryWithin    string    `yaml:"recovery_within,omitempty"`
}

// LoadScenario reads and validates a scenario YAML file. Every validation
// failure is an error with the field named — a broken scenario must never
// produce plausible-looking ground truth.
func LoadScenario(path string) (Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("scenario %s: %w", path, err)
	}
	return ParseScenario(raw)
}

// ParseScenario parses scenario YAML bytes; see LoadScenario.
func ParseScenario(raw []byte) (Scenario, error) {
	var doc scenarioDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typoed knob must fail, not silently default
	if err := dec.Decode(&doc); err != nil {
		return Scenario{}, fmt.Errorf("scenario: %w", err)
	}
	if doc.Name == "" {
		return Scenario{}, fmt.Errorf("scenario: name is required")
	}
	if !doc.Start.Before(doc.End) {
		return Scenario{}, fmt.Errorf("scenario %s: window [%v, %v) is empty or inverted", doc.Name, doc.Start, doc.End)
	}

	cfg := Config{
		Seed:                  doc.Seed,
		Start:                 doc.Start,
		End:                   doc.End,
		EnterpriseFraction:    doc.EnterpriseFraction,
		Customers:             doc.Customers,
		CaptureDelayMin:       doc.CaptureDelayMin,
		SettleDelayMin:        doc.SettleDelayMin,
		CaptureCapacityPerMin: doc.CaptureCapacityPerMin,
		SettleCapacityPerMin:  doc.SettleCapacityPerMin,
	}
	for i, fd := range doc.Faults {
		f := FaultSpec{
			Kind:              fd.Kind,
			From:              fd.From,
			To:                fd.To,
			Rate:              fd.Rate,
			Queue:             fd.Queue,
			RecoveredFraction: fd.RecoveredFraction,
		}
		if fd.RecoveryWithin != "" {
			d, err := time.ParseDuration(fd.RecoveryWithin)
			if err != nil {
				return Scenario{}, fmt.Errorf("scenario %s: faults[%d].recovery_within: %w", doc.Name, i, err)
			}
			f.RecoveryWithin = d
		}
		if err := f.Validate(); err != nil {
			return Scenario{}, fmt.Errorf("scenario %s: faults[%d]: %w", doc.Name, i, err)
		}
		if f.From.Before(doc.Start) || f.To.After(doc.End) {
			return Scenario{}, fmt.Errorf("scenario %s: faults[%d]: window [%v, %v) outside the run window", doc.Name, i, f.From, f.To)
		}
		cfg.Faults = append(cfg.Faults, f)
	}
	return Scenario{Name: doc.Name, Config: cfg}, nil
}
