package promgolden

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	promql "github.com/NightWatchEng/shortfall/adapters/query/promql"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/testkit"
)

const promImage = "prom/prometheus:v2.53.5"

// startPrometheus runs a throwaway Prometheus with the remote-write receiver on,
// returns its base URL and a cleanup func. Without SHORTFALL_GOLDEN it skips
// (a no-op on a Docker-less machine and in the core CI job); WITH the env set it
// requires Docker and hard-fails if it is missing, so the golden CI job can
// never go green without actually running the parity assertions.
func startPrometheus(t *testing.T) (string, func()) {
	t.Helper()
	// No env: a no-op on a Docker-less dev machine and in the core CI job.
	if os.Getenv("SHORTFALL_GOLDEN") == "" {
		t.Skip("set SHORTFALL_GOLDEN=1 (and have Docker) to run the live Prometheus golden harness")
	}
	// SHORTFALL_GOLDEN is an explicit demand to run the parity gate (the golden
	// CI job sets it). Docker being absent here is a HARD FAILURE, never a skip:
	// a skip would exit 0 and let the required-looking parity gate go green
	// having asserted nothing — a silent gate-weakening.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("SHORTFALL_GOLDEN is set but docker is not on PATH; the parity gate cannot run")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("SHORTFALL_GOLDEN is set but the docker daemon is not running; the parity gate cannot run: %v", err)
	}
	run := exec.Command("docker", "run", "-d", "--rm", "-p", "9090",
		promImage,
		"--config.file=/etc/prometheus/prometheus.yml",
		"--web.enable-remote-write-receiver",
		"--storage.tsdb.retention.time=1y",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	cleanup := func() { _ = exec.Command("docker", "rm", "-f", id).Run() }

	// The published host port may not be queryable the instant `docker run -d`
	// returns — poll until the mapping appears.
	var hostPort string
	for i := 0; i < 50 && hostPort == ""; i++ {
		portOut, err := exec.Command("docker", "port", id, "9090/tcp").CombinedOutput()
		if err == nil {
			if first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]; strings.Contains(first, ":") {
				hostPort = first[strings.LastIndex(first, ":")+1:]
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if hostPort == "" {
		cleanup()
		t.Fatal("could not resolve the published Prometheus port")
	}
	base := "http://127.0.0.1:" + hostPort

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := waitReady(ctx, base); err != nil {
		cleanup()
		t.Fatalf("prometheus not ready: %v", err)
	}
	return base, cleanup
}

// TestPromQLParityAgainstRealPrometheus is the workspace-0ka correctness bar:
// the same harness metrics, fed to memq and to a real Prometheus, must yield
// identical Series through the frozen query AST for the api-5xx and queue-stall
// scenarios.
func TestPromQLParityAgainstRealPrometheus(t *testing.T) {
	scenarios := map[string][]checkout.FaultSpec{
		"api-5xx":     {{Kind: checkout.FaultAPI5xx, Rate: 0.5}},
		"queue-stall": {{Kind: checkout.FaultConsumerStall, Queue: checkout.QueueCapture}},
	}
	for name, faults := range scenarios {
		t.Run(name, func(t *testing.T) {
			// A fresh Prometheus per scenario: a shared head would reject the
			// second scenario's samples as out-of-bounds and mix the two
			// scenarios' identically-labelled series.
			base, cleanup := startPrometheus(t)
			defer cleanup()
			// The whole window must sit within ~1h of now: Prometheus' remote-write
			// receiver rejects samples older than roughly an hour (out of bounds).
			// A 50-minute window ending 5 minutes ago keeps every sample fresh.
			end := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Minute)
			start := end.Add(-50 * time.Minute)
			for i := range faults {
				faults[i].From = start.Add(10 * time.Minute)
				faults[i].To = start.Add(35 * time.Minute)
			}
			res := checkout.Run(checkout.Config{Seed: 5, Start: start, End: end, Faults: faults})
			// Snapshot the in-flight gauge one minute before the window closes.
			// A sample stamped exactly at To (MetricsFromResult's default, the
			// run end == window end here) is dropped by the half-open [From,To)
			// read on BOTH sides — memq excludes At>=To and the promql adapter
			// evaluates last_over_time at To-1ms — so it would make the gauge
			// parity vacuous. Inside the window it exercises last_over_time.
			gaugeAt := end.Add(-time.Minute)
			points := testkit.MetricsFromResultAt(res, gaugeAt)
			if len(points) == 0 {
				t.Fatal("scenario produced no metric points")
			}

			ctx := context.Background()
			// Seed Prometheus with the CUMULATIVE counter series a real client
			// would expose; memq reads the raw deltas. Both must agree.
			if err := remoteWrite(ctx, base, cumulativeForPrometheus(points)); err != nil {
				t.Fatalf("remote_write: %v", err)
			}
			// Give Prometheus a moment to commit the remote-write batch to its head.
			mq := memq.New(memq.WithMetrics(points))
			pq := promql.New(base)
			window := query.TimeRange{From: start, To: end}

			queries := goldenQueries(window)
			// Poll on a query guaranteed to have data (total txn count, all
			// outcomes) — a leg-specific filter like outcome=failed can be empty
			// for a stall scenario and would never signal ingestion.
			waitIngested(t, ctx, pq, query.Query{Metric: "biz_txn_total", Agg: query.AggSum, Range: window})
			for i, qy := range queries {
				want, err := mq.QueryMetric(ctx, qy)
				if err != nil {
					t.Fatalf("query %d memq: %v", i, err)
				}
				// The gauge parity must never be silently vacuous (empty ==
				// empty proves nothing): the stall scenario guarantees a
				// capture-queue backlog, so its in-flight gauge is non-empty.
				if qy.Metric == "biz_inflight_value" && name == "queue-stall" && len(want) == 0 {
					t.Fatalf("query %d gauge parity is vacuous: memq returned no biz_inflight_value series for the stall scenario", i)
				}
				got, err := pq.QueryMetric(ctx, qy)
				if err != nil {
					t.Fatalf("query %d promql: %v", i, err)
				}
				if !sameSeries(want, got) {
					t.Fatalf("query %d (%s) parity mismatch:\n memq=%v\nprom=%v", i, qy.Metric, want, got)
				}
			}
		})
	}
}

func goldenQueries(w query.TimeRange) []query.Query {
	return []query.Query{
		// Counter: realized value sum, per currency (the @-diff translation).
		{Metric: "biz_value_total", Agg: query.AggSum, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"currency"}, Range: w},
		// Counter: txn count, failed, no group-by (scalar over the window).
		{Metric: "biz_txn_total", Agg: query.AggSum, Filters: map[string]string{"outcome": "failed"}, Range: w},
		// Counter grouped by stage+outcome (multi-label @-diff).
		{Metric: "biz_txn_total", Agg: query.AggSum, GroupBy: []string{"stage", "outcome"}, Range: w},
		// Gauge: in-flight value by age bucket (last_over_time translation).
		{Metric: "biz_inflight_value", GroupBy: []string{"age_bucket", "currency"}, Range: w},
	}
}
