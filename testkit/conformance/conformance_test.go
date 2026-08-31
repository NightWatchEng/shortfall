// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// memBackend counts what reached the backend.
type memBackend struct{ metrics, events int }

func (b *memBackend) MetricPoints() int { return b.metrics }
func (b *memBackend) Events() int       { return b.events }

// fakeCfg dials in a specific (mis)behavior so the suite can be tested
// against known-good and known-bad exporters.
type fakeCfg struct {
	caps             emit.Caps
	buffer           bool // hold until Shutdown instead of delivering eagerly
	dropOnShutdown   bool // lose the buffer on Shutdown (the bug the suite must catch)
	dishonestMetrics bool // deliver metrics though caps.Metrics is false
	dishonestEvents  bool // deliver events though caps.Events is false
	errOnExport      bool // error on a capable export (also a conformance failure)

	// Post-Shutdown behavior. A buffering fake refuses by default (the
	// terminal push-exporter shape) and an eager fake keeps delivering (the
	// pull-collected shape) — both conformant. These dial in the two ways an
	// implementation can break the disjunction instead.
	absorbAfterShutdown bool // accept post-Shutdown exports and lose them silently
	refuseButDeliver    bool // error on post-Shutdown exports yet deliver them anyway
	errOnRepeatShutdown bool // error on a second Shutdown (the sdk-sentinel leak)
}

type fakeExporter struct {
	cfg        fakeCfg
	be         *memBackend
	bufM, bufE int
	closed     bool
}

func (f *fakeExporter) Capabilities() emit.Caps { return f.cfg.caps }

func (f *fakeExporter) ExportMetrics(_ context.Context, b []emit.MetricPoint) error {
	if len(b) == 0 {
		return nil
	}
	if !f.cfg.caps.Metrics && !f.cfg.dishonestMetrics {
		return nil // honest incapable: no-op
	}
	if f.cfg.errOnExport && f.cfg.caps.Metrics {
		return errors.New("metric backend down")
	}
	if f.closed {
		switch {
		case f.cfg.absorbAfterShutdown:
			return nil // accepted, delivered nowhere
		case f.cfg.refuseButDeliver:
			f.be.metrics += len(b)
			return errors.New("exporter is shut down")
		case f.cfg.buffer:
			return errors.New("exporter is shut down")
		}
		// eager: pull-collected, stays functional after Shutdown
	}
	if f.cfg.buffer {
		f.bufM += len(b)
		return nil
	}
	f.be.metrics += len(b)
	return nil
}

func (f *fakeExporter) ExportEvents(_ context.Context, b []biz.Outcome) error {
	if len(b) == 0 {
		return nil
	}
	if !f.cfg.caps.Events && !f.cfg.dishonestEvents {
		return nil
	}
	if f.cfg.errOnExport && f.cfg.caps.Events {
		return errors.New("log backend down")
	}
	if f.closed {
		switch {
		case f.cfg.absorbAfterShutdown:
			return nil
		case f.cfg.refuseButDeliver:
			f.be.events += len(b)
			return errors.New("exporter is shut down")
		case f.cfg.buffer:
			return errors.New("exporter is shut down")
		}
	}
	if f.cfg.buffer {
		f.bufE += len(b)
		return nil
	}
	f.be.events += len(b)
	return nil
}

func (f *fakeExporter) Shutdown(context.Context) error {
	if f.closed && f.cfg.errOnRepeatShutdown {
		return errors.New("exporter is shutdown")
	}
	if f.cfg.buffer && !f.cfg.dropOnShutdown {
		f.be.metrics += f.bufM
		f.be.events += f.bufE
	}
	f.bufM, f.bufE = 0, 0
	f.closed = true
	return nil
}

type fakeHarness struct{ cfg fakeCfg }

func (h fakeHarness) New() (emit.Exporter, Backend) {
	be := &memBackend{}
	return &fakeExporter{cfg: h.cfg, be: be}, be
}

func resultsByName(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

// TestSuiteVerdicts pins what the conformance suite must conclude for a
// range of well- and mis-behaved exporters: it must pass a conformant one
// and fail exactly the invariant each defect breaks.
func TestSuiteVerdicts(t *testing.T) {
	both := emit.Caps{Metrics: true, Events: true, MetricHistoryWeeks: 2, EventHistoryWeeks: 8}
	metricsOnly := emit.Caps{Metrics: true}
	eventsOnly := emit.Caps{Events: true}

	cases := []struct {
		name     string
		cfg      fakeCfg
		wantFail []string // invariants expected to fail; all others must pass or skip
	}{
		{
			name:     "conformant, both signals, buffered then flushed",
			cfg:      fakeCfg{caps: both, buffer: true},
			wantFail: nil,
		},
		{
			name:     "conformant, both signals, eager delivery",
			cfg:      fakeCfg{caps: both},
			wantFail: nil,
		},
		{
			name: "drops buffer on shutdown",
			cfg:  fakeCfg{caps: both, buffer: true, dropOnShutdown: true},
			wantFail: []string{
				"metrics flush on shutdown with no loss",
				"events flush on shutdown with no loss",
			},
		},
		{
			name: "errors on capable export",
			cfg:  fakeCfg{caps: both, errOnExport: true},
			wantFail: []string{
				"metrics flush on shutdown with no loss",
				"events flush on shutdown with no loss",
			},
		},
		{
			name:     "metrics-only, honest about events",
			cfg:      fakeCfg{caps: metricsOnly},
			wantFail: nil,
		},
		{
			name:     "events-only, honest about metrics",
			cfg:      fakeCfg{caps: eventsOnly},
			wantFail: nil,
		},
		{
			name:     "declares Events=false but delivers events",
			cfg:      fakeCfg{caps: metricsOnly, dishonestEvents: true},
			wantFail: []string{"events-incapable exporter delivers no events"},
		},
		{
			name:     "declares Metrics=false but delivers metrics",
			cfg:      fakeCfg{caps: eventsOnly, dishonestMetrics: true},
			wantFail: []string{"metrics-incapable exporter delivers no metrics"},
		},
		{
			name:     "declares no signal at all",
			cfg:      fakeCfg{caps: emit.Caps{}},
			wantFail: []string{"declares at least one signal"},
		},
		{
			// Post-Shutdown exports accepted into a buffer nothing will
			// ever flush again. Neither refused nor delivered — the one
			// behavior the contract forbids.
			name: "silently absorbs post-shutdown exports",
			cfg:  fakeCfg{caps: both, buffer: true, absorbAfterShutdown: true},
			wantFail: []string{
				"post-shutdown metric export is refused or delivered",
				"post-shutdown event export is refused or delivered",
			},
		},
		{
			// A refusal must actually refuse: erroring while still delivering
			// is a double answer, not a terminal exporter.
			name: "errors on post-shutdown export but delivers it anyway",
			cfg:  fakeCfg{caps: both, buffer: true, refuseButDeliver: true},
			wantFail: []string{
				"post-shutdown metric export is refused or delivered",
				"post-shutdown event export is refused or delivered",
			},
		},
		{
			// A dependency's shutdown sentinel leaking out of a repeat call:
			// the no-op has no work left to fail at.
			name:     "errors on a repeat shutdown",
			cfg:      fakeCfg{caps: both, errOnRepeatShutdown: true},
			wantFail: []string{"a repeat shutdown after a successful one returns nil"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resultsByName(Check(fakeHarness{cfg: c.cfg}))
			wantFail := map[string]bool{}
			for _, n := range c.wantFail {
				wantFail[n] = true
				r, ok := got[n]
				if !ok {
					t.Fatalf("expected invariant %q to run, but it was absent (got %v)", n, keys(got))
				}
				if r.Err == "" {
					t.Fatalf("expected invariant %q to FAIL, but it passed", n)
				}
			}
			for name, r := range got {
				if wantFail[name] || r.Skipped {
					continue
				}
				if r.Err != "" {
					t.Fatalf("invariant %q unexpectedly failed: %s", name, r.Err)
				}
			}
		})
	}
}

func keys(m map[string]Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRunExporterPassesConformant proves the *testing.T wrapper drives a
// conformant exporter to green (no subtest fails).
func TestRunExporterPassesConformant(t *testing.T) {
	RunExporter(t, fakeHarness{cfg: fakeCfg{
		caps:   emit.Caps{Metrics: true, Events: true},
		buffer: true,
	}})
}

// TestSampleReasonsMatchADR0002 pins adr0002DropReasons — the list
// sampleMetrics seeds from — to ADR-0002's enum, verbatim and in order. The
// enum drifted once before (encode, retired by ADR-0002's 2026-08-30
// amendment), and
// a seeded-values check cannot catch a re-drift: sampleMetrics caps n at 12,
// which reaches only the first two cycle slots, so the pin must be on the
// list itself, where every slot is visible.
func TestSampleReasonsMatchADR0002(t *testing.T) {
	want := []string{"invalid", "overflow", "export"}
	if len(adr0002DropReasons) != len(want) {
		t.Fatalf("adr0002DropReasons = %v, want ADR-0002's enum %v", adr0002DropReasons, want)
	}
	for i, r := range want {
		if adr0002DropReasons[i] != r {
			t.Fatalf("adr0002DropReasons[%d] = %q, want %q (ADR-0002's enum %v)", i, adr0002DropReasons[i], r, want)
		}
	}
}
