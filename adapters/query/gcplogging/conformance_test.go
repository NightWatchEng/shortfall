package gcplogging

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

// scenarioEvents runs the api-5xx golden locus and returns the
// telemetry-visible outcome events plus the incident window — the same
// fixture shape test/loggolden uses for the CloudWatch pairing, so the two
// log-store queriers are held to one standard.
func scenarioEvents() ([]biz.Outcome, query.TimeRange) {
	end := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	start := end.Add(-2 * time.Hour)
	incidentFrom := end.Add(-70 * time.Minute)
	incidentTo := end.Add(-15 * time.Minute)
	res := checkout.Run(checkout.Config{
		Seed: 901, Start: start, End: end,
		Faults: []checkout.FaultSpec{
			{Kind: checkout.FaultAPI5xx, Rate: 0.35, From: incidentFrom, To: incidentTo},
			{Kind: checkout.FaultAPILatency, Rate: 0.15, From: incidentFrom, To: incidentTo},
		},
	})
	return testkit.EventsFromResult(res), query.TimeRange{From: incidentFrom, To: incidentTo}
}

// engineQueries are the event shapes the engine's legs actually issue —
// the four verbs this adapter claims: grouped sums, distinct count, the
// per-entity max that ADR-0009 de-dup is built on, and an ordered+limited
// top-accounts read.
//
// Between them they filter on EVERY label in payloadPaths, and each of
// those filters MATCHES something in the fixture. Both halves are load-
// bearing. A filter label is the only thing that puts its JSONPath into the
// pushed-down WHERE, and the fake evaluates that WHERE — but a filter that
// matches nothing compares empty to empty, and a JSONPath is then free to
// drift to any other key the exporter writes without a single test noticing.
// (That is not hypothetical: the first version of this suite filtered stage
// on "capture", which the api-5xx locus never produces, and mis-mapping
// $."biz.stage" to $."biz.flow" left the whole package green.) The
// conformance loop therefore records a per-label WITNESS — a query filtering
// on that label whose oracle answer is non-empty — and fails on any label
// that has none.
func engineQueries(w query.TimeRange, entity, customer string) []query.EventQuery {
	return []query.EventQuery{
		{Range: w, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"currency", "entity"}, Agg: query.EventAggMaxPerGroup},
		{Range: w, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount},
		{Range: w, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer", "segment"}},
		{Range: w, Filters: map[string]string{"outcome": "success", "currency": "USD"}},
		{Range: w, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer"}, OrderBy: query.OrderSumDesc, Limit: 5},
		{Range: w, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"segment"}, OrderBy: query.OrderCountDesc, Limit: 2},
		// The flow filter the engine adds whenever Request.Flows is set,
		// plus the remaining pushed-down labels.
		{Range: w, Filters: map[string]string{"flow": "invoice.pay", "outcome": "failed", "currency": "USD"}, GroupBy: []string{"stage"}},
		{Range: w, Filters: map[string]string{"stage": "settle", "currency": "USD"}, GroupBy: []string{"outcome"}},
		{Range: w, Filters: map[string]string{"kind": "fee", "segment": "smb", "currency": "USD"}, GroupBy: []string{"outcome"}},
		{Range: w, Filters: map[string]string{"entity": entity, "currency": "USD"}, GroupBy: []string{"entity"}, Agg: query.EventAggMaxPerGroup},
		{Range: w, Filters: map[string]string{"customer": customer, "currency": "USD"}, GroupBy: []string{"customer"}},
	}
}

// pickIDs returns an entity and customer id the fixture actually contains,
// so the two queries that filter on them match something. Filtering on an
// id that is not in the window would make both sides empty and the
// comparison vacuous — which is exactly the shape these harnesses keep
// being caught in.
func pickIDs(t *testing.T, events []biz.Outcome, w query.TimeRange) (string, string) {
	t.Helper()
	for _, e := range events {
		if !e.At.Before(w.From) && e.At.Before(w.To) &&
			e.VC.EntityID != "" && e.VC.CustomerID != "" && e.VC.Money.Currency == "USD" {
			return e.VC.EntityID, e.VC.CustomerID
		}
	}
	t.Fatal("fixture carries no in-window USD event with both ids — the id-filtered parity rows would be vacuous")
	return "", ""
}

// TestQuerierConformanceAgainstMemq is the parity fence: the adapter reads
// the scenario's outcome events back out of a fake Log Analytics dataset
// (real record shape, real BigQuery REST envelope, paging and a
// not-yet-complete job included) and must return byte-identical
// EventGroups to the memq oracle for every verb it claims.
//
// Non-vacuity is asserted, not assumed: the oracle's own answers must be
// non-empty and must carry money, so a harness that silently degraded to
// "no rows == no rows" fails here instead of passing.
func TestQuerierConformanceAgainstMemq(t *testing.T) {
	events, window := scenarioEvents()
	entity, customer := pickIDs(t, events, window)
	mq := memq.New(memq.WithEvents(events))
	ctx := context.Background()

	// Two backends, one adapter. "honours the pushdown" is real BigQuery:
	// the fake evaluates the emitted WHERE, so every JSONPath in
	// payloadPaths is load-bearing. "ignores the pushdown" is the backend
	// that serves everything regardless (LocalStack does this to an
	// Insights filter): there the client-side window, filters and marker
	// discrimination are what must carry the answer. The adapter has to be
	// right on both, and a defect in either half fails exactly one mode.
	modes := []struct {
		name     string
		evaluate bool
	}{
		{"backend honours the pushdown", true},
		{"backend ignores the pushdown", false},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			srv := newFakeBigQuery(t, entriesFor(events))
			srv.evaluate = mode.evaluate
			q := srv.querier(t)

			var sawMoney, sawGroups, sawDistinct bool
			// witnessed[label] means: some query filtered on that label and
			// the oracle answered with data. Only then is that label's
			// JSONPath actually compared through the fake's evaluation of
			// the pushed-down WHERE.
			witnessed := map[string]bool{}
			for i, qy := range engineQueries(window, entity, customer) {
				want, err := mq.QueryEvents(ctx, qy)
				if err != nil {
					t.Fatalf("query %d memq: %v", i, err)
				}
				got, err := q.QueryEvents(ctx, qy)
				if err != nil {
					t.Fatalf("query %d gcplogging: %v", i, err)
				}
				if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
					t.Fatalf("query %d parity mismatch:\ngcplogging=%+v\nmemq      =%+v", i, got, want)
				}
				if len(want) > 0 {
					for label := range qy.Filters {
						witnessed[label] = true
					}
				}
				for _, g := range want {
					if g.SumMinor != 0 || g.MaxMinor != 0 {
						sawMoney = true
					}
					if len(g.Key) > 0 {
						sawGroups = true
					}
					if qy.Agg == query.EventAggDistinctCount && g.Count > 0 {
						sawDistinct = true
					}
				}
			}
			// Guard the assertions themselves: without these the loop
			// above would pass on a scenario that produced nothing at all.
			if !sawMoney {
				t.Fatal("parity is vacuous: no query returned a non-zero money figure")
			}
			if !sawGroups {
				t.Fatal("parity is vacuous: no query returned a keyed group")
			}
			if !sawDistinct {
				t.Fatal("parity is vacuous: the distinct-count verb returned zero customers")
			}
			if srv.queries == 0 {
				t.Fatal("parity is vacuous: the adapter never issued a query")
			}
			// The per-label witness. A label with no witness is a JSONPath
			// this suite compares empty against empty — free to drift to any
			// other key the exporter writes, which against real BigQuery is
			// a silent zero on a money leg.
			if len(payloadPaths) == 0 {
				t.Fatal("payloadPaths is empty — the witness check would pass vacuously")
			}
			for label, path := range payloadPaths {
				if !witnessed[label] {
					t.Errorf("label %q has no witness: no conformance query filters on it AND matches data, "+
						"so its JSONPath %s is never really compared", label, path)
				}
			}
		})
	}
}

// TestPayloadPathsMatchTheExporterRecord is the drift fence between the two
// halves of one wire convention. Every JSONPath the adapter pushes down
// must name a key the exporter actually writes; a path that drifts (the
// biz.customer.id -> biz_customer_id sanitization the package doc cites as
// the reason a BigQuery sink was rejected, say) makes real BigQuery return
// no rows for that filter, which the engine grounds as a confident zero.
// The record here is rendered by the same fixture the parity suite feeds
// in, which mirrors adapters/export/gcp's buildEventRecord.
func TestPayloadPathsMatchTheExporterRecord(t *testing.T) {
	full := biz.Outcome{
		At: from, Stage: "capture", Result: biz.ResultFailed, Source: "harness", Err: "card_declined",
		VC: biz.ValueContext{
			Flow: "invoice.pay", EntityID: "inv_00000042", CustomerID: "h:c000007",
			Segment: "smb", Kind: biz.KindFee,
			Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2},
		},
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(eventRecord(full)), &record); err != nil {
		t.Fatal(err)
	}
	if len(payloadPaths) == 0 {
		t.Fatal("payloadPaths is empty — this check would pass vacuously")
	}
	quoted := regexp.MustCompile(`^\$\."(.+)"$`)
	for label, path := range payloadPaths {
		m := quoted.FindStringSubmatch(path)
		if m == nil {
			t.Errorf("label %q: JSONPath %q is not the quoted $.\"key\" form a dotted key needs", label, path)
			continue
		}
		if _, ok := record[m[1]]; !ok {
			t.Errorf("label %q pushes down key %q, which the exporter's record does not carry (keys: %v)",
				label, m[1], sortedKeys(record))
		}
	}
	// The marker the decoder and the pushdown both key on is the
	// exporter's too.
	if got, _ := record["event"].(string); got != eventMarker {
		t.Fatalf("exporter record event = %q, want the marker %q this adapter filters on", got, eventMarker)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRealizedLegParity is the ADR-0009 assertion the bead asks for: the
// engine's realized leg — entity de-dup by max-per-group, minus every
// entity that also succeeded anywhere in the window — must produce the
// same figure over this adapter as over memq. The de-dup lives in the
// engine, so what is being proved here is that the adapter returns the
// per-entity groups and the success set faithfully enough to feed it.
func TestRealizedLegParity(t *testing.T) {
	events, window := scenarioEvents()
	srv := newFakeBigQuery(t, entriesFor(events))
	q := srv.querier(t)
	mq := memq.New(memq.WithEvents(events))

	var reg registry.Registry
	req := engine.Request{Window: window}
	want, err := engine.RealizedLeg(context.Background(), &reg, mq, req)
	if err != nil {
		t.Fatalf("memq realized: %v", err)
	}
	got, err := engine.RealizedLeg(context.Background(), &reg, q, req)
	if err != nil {
		t.Fatalf("gcplogging realized: %v", err)
	}
	if want.Count == 0 || len(want.ByCurrency) == 0 {
		t.Fatal("realized parity is vacuous: the oracle found no realized loss in the window")
	}
	var total int64
	for _, v := range want.ByCurrency {
		total += v
	}
	if total == 0 {
		t.Fatal("realized parity is vacuous: the oracle's realized loss is zero minor units")
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("realized leg mismatch:\ngcplogging=%+v\nmemq      =%+v", got, want)
	}
}

// TestLaterSuccessExclusionMatchesMemq pins ADR-0009's set-membership rule
// on a hand-built fixture where the answer is checkable by eye: entity
// inv_r fails for 900_00 and later succeeds, so it is excluded entirely;
// inv_d is redelivered twice at different amounts and counts once, at the
// larger. Both queriers must say 149_00 over one entity.
func TestLaterSuccessExclusionMatchesMemq(t *testing.T) {
	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	window := query.TimeRange{From: base, To: base.Add(time.Hour)}
	events := []biz.Outcome{
		outcome("invoice.pay", "capture", "failed", "inv_d", "h:c1", "smb", "USD", 10000, base.Add(1*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_d", "h:c1", "smb", "USD", 14900, base.Add(2*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_r", "h:c2", "enterprise", "USD", 90000, base.Add(3*time.Minute)),
		outcome("invoice.pay", "settle", "success", "inv_r", "h:c2", "enterprise", "USD", 90000, base.Add(4*time.Minute)),
	}
	srv := newFakeBigQuery(t, entriesFor(events))
	q := srv.querier(t)
	mq := memq.New(memq.WithEvents(events))

	var reg registry.Registry
	req := engine.Request{Window: window}
	want, err := engine.RealizedLeg(context.Background(), &reg, mq, req)
	if err != nil {
		t.Fatalf("memq realized: %v", err)
	}
	if want.Count != 1 || want.ByCurrency["USD"] != 14900 {
		t.Fatalf("oracle disagrees with the hand-computed answer: %+v (want 1 entity, 14900 USD)", want)
	}
	got, err := engine.RealizedLeg(context.Background(), &reg, q, req)
	if err != nil {
		t.Fatalf("gcplogging realized: %v", err)
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("realized leg mismatch:\ngcplogging=%+v\nmemq      =%+v", got, want)
	}
}

// TestEventOrderIsFaithful pins that the adapter neither reorders nor drops
// events on the way through: the decoded outcomes it hands the reference
// are exactly the entries the backend returned, in the store's timestamp
// order. ADR-0009's de-dup and later-success exclusion live in the engine,
// so faithful order and completeness here is the whole of the adapter's
// contribution to the realized figure — asserted, not assumed.
func TestEventOrderIsFaithful(t *testing.T) {
	events, w := scenarioEvents()
	cases := []struct {
		name     string
		evaluate bool
		// wantWindowed: the backend applied the pushdown, so only in-window
		// entries come back. Otherwise every fixture entry does.
		wantWindowed bool
	}{
		{name: "backend honours the pushdown", evaluate: true, wantWindowed: true},
		{name: "backend ignores the pushdown", evaluate: false, wantWindowed: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newFakeBigQuery(t, entriesFor(events))
			srv.evaluate = c.evaluate
			got, err := srv.querier(t).fetch(context.Background(), query.EventQuery{Range: w})
			if err != nil {
				t.Fatal(err)
			}
			want := append([]biz.Outcome(nil), events...)
			if c.wantWindowed {
				var in []biz.Outcome
				for _, e := range want {
					if !e.At.Before(w.From) && e.At.Before(w.To) {
						in = append(in, e)
					}
				}
				want = in
			}
			sortByTime(want)
			if len(want) == 0 {
				t.Fatal("order assertion is vacuous: the fixture produced no events to order")
			}
			if len(got) != len(want) {
				t.Fatalf("decoded %d events, want %d", len(got), len(want))
			}
			for i := range want {
				if !got[i].At.Equal(want[i].At) || got[i].VC.EntityID != want[i].VC.EntityID ||
					got[i].Result != want[i].Result || got[i].VC.Money != want[i].VC.Money {
					t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
				}
			}
			// Order is only meaningful over a sequence that is not already
			// sorted by accident of a single timestamp.
			if got[0].At.Equal(got[len(got)-1].At) {
				t.Fatal("order assertion is vacuous: every event shares one timestamp")
			}
		})
	}
}

// --- fixture rendering -------------------------------------------------

// eventRecord renders one outcome exactly as adapters/export/gcp's
// buildEventRecord does, minus the two keys Cloud Logging lifts out of the
// payload into the entry itself (`time` and `severity`). That exporter is
// the authority for this shape; this fixture is deliberately a separate
// spelling of it so a drift in either is a test failure and not a silent
// agreement (the cwinsights test makes the same trade for EMF).
func eventRecord(o biz.Outcome) string {
	rec := map[string]any{
		"event":            "biz.outcome",
		"biz.flow":         o.VC.Flow,
		"biz.stage":        o.Stage,
		"biz.outcome":      string(o.Result),
		"biz.entity.id":    o.VC.EntityID,
		"biz.customer.id":  o.VC.CustomerID,
		"biz.amount_minor": o.VC.Money.Amount,
		"biz.currency":     o.VC.Money.Currency,
		"biz.exponent":     o.VC.Money.Exponent,
		"biz.value.kind":   string(o.VC.Kind),
		"biz.amount.est":   o.VC.Estimated,
	}
	if o.VC.Segment != "" {
		rec["biz.segment"] = o.VC.Segment
	}
	if o.Source != "" {
		rec["source"] = o.Source
	}
	if o.Err != "" {
		rec["error"] = o.Err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// entriesFor renders the scenario as the (event_micros, payload) rows a
// Log Analytics read returns, in timestamp order, with two foreign entries
// mixed in: the `_AllLogs` view of a log bucket carries every log the
// project writes, so the marker discrimination has to be live.
func entriesFor(events []biz.Outcome) []fakeRow {
	sorted := append([]biz.Outcome(nil), events...)
	sortByTime(sorted)
	rows := make([]fakeRow, 0, len(sorted)+2)
	if len(sorted) > 0 {
		rows = append(rows, fakeRow{
			micros:  sorted[0].At.UnixMicro(),
			payload: `{"message":"starting checkout worker","severity_hint":"info"}`,
		})
	}
	for _, o := range sorted {
		rows = append(rows, fakeRow{micros: o.At.UnixMicro(), payload: eventRecord(o)})
	}
	rows = append(rows, fakeRow{
		micros:  time.Now().UnixMicro(),
		payload: `{"event":"biz.flush","flushed":12}`,
	})
	return rows
}

func sortByTime(o []biz.Outcome) {
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && o[j].At.Before(o[j-1].At); j-- {
			o[j], o[j-1] = o[j-1], o[j]
		}
	}
}

func outcome(flow, stage, result, entity, customer, segment, currency string, amount int64, at time.Time) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: biz.Result(result),
		VC: biz.ValueContext{
			Flow: flow, EntityID: entity, CustomerID: customer, Segment: segment, Kind: biz.KindFee,
			Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2},
		},
	}
}

// mustContain fails unless s contains sub — used to pin SQL fragments.
func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("query text is missing %q:\n%s", sub, s)
	}
}
