// Package loggolden proves the log-store querier returns the same
// EventGroups as the in-memory reference for a golden scenario's real
// export wire format: the cloudwatch exporter's EMF records put into a
// live LocalStack log group (metric records included, so the
// outcome-marker discrimination is exercised) for cwinsights. It lives
// in its own module so Docker-orchestration test deps never touch the
// adapters' go.mod.
package loggolden

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	cwexport "github.com/NightWatchEng/shortfall/adapters/export/cloudwatch"
	"github.com/NightWatchEng/shortfall/adapters/query/cwinsights"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/testkit"
)

const localstackImage = "localstack/localstack:3.8"

// scenarioEvents runs the api-5xx golden locus over a recent window (both
// stores age-bound ingest) and returns the telemetry-visible events plus
// the incident window to query.
func scenarioEvents() ([]biz.Outcome, query.TimeRange) {
	end := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
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

// engineQueries are the event shapes the engine's legs issue.
func engineQueries(w query.TimeRange) []query.EventQuery {
	return []query.EventQuery{
		{Range: w, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"currency", "entity"}, Agg: query.EventAggMaxPerGroup},
		{Range: w, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount},
		{Range: w, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer", "segment"}},
		{Range: w, Filters: map[string]string{"outcome": "success", "currency": "USD"}},
	}
}

// assertParity compares a querier against memq over the engine queries and
// guards non-vacuity: the api-5xx locus guarantees failed groups.
func assertParity(t *testing.T, name string, q query.Querier, events []biz.Outcome, window query.TimeRange) {
	t.Helper()
	mq := memq.New(memq.WithEvents(events))
	ctx := context.Background()
	for i, qy := range engineQueries(window) {
		want, err := mq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d memq: %v", i, err)
		}
		if i == 0 && len(want) == 0 {
			t.Fatal("parity is vacuous: the api-5xx scenario produced no failed groups")
		}
		got, err := q.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d %s: %v", i, name, err)
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("query %d parity mismatch:\n%s=%+v\nmemq=%+v", i, name, got, want)
		}
	}
}

// requireDocker gates on SHORTFALL_GOLDEN exactly like test/promgolden:
// unset skips honestly; set demands Docker and hard-fails without it.
func requireDocker(t *testing.T, image string) {
	t.Helper()
	if os.Getenv("SHORTFALL_GOLDEN") == "" {
		t.Skip("set SHORTFALL_GOLDEN=1 (and have Docker) to run the live log-store golden harness")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("SHORTFALL_GOLDEN is set but docker is not on PATH; the parity gate cannot run")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("SHORTFALL_GOLDEN is set but the docker daemon is not running: %v", err)
	}
	if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", image, err, out)
	}
}

// startContainer runs an image with one published port and returns the
// mapped host base URL plus a cleanup func.
func startContainer(t *testing.T, image, port string, env []string, args ...string) string {
	t.Helper()
	runArgs := []string{"run", "-d", "--rm", "-p", port}
	for _, e := range env {
		runArgs = append(runArgs, "-e", e)
	}
	runArgs = append(runArgs, image)
	runArgs = append(runArgs, args...)
	out, err := exec.Command("docker", runArgs...).Output()
	if err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	fields := strings.Fields(string(out))
	id := fields[len(fields)-1]
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	var hostPort string
	for i := 0; i < 40; i++ {
		pout, err := exec.Command("docker", "port", id, port).Output()
		if err == nil && len(pout) > 0 {
			line := strings.TrimSpace(strings.Split(string(pout), "\n")[0])
			hostPort = line[strings.LastIndex(line, ":")+1:]
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if hostPort == "" {
		t.Fatalf("no published port for %s", image)
	}
	return "http://127.0.0.1:" + hostPort
}

func waitHTTP(t *testing.T, url, needle string) {
	t.Helper()
	for i := 0; i < 60; i++ {
		resp, err := http.Get(url)
		if err == nil {
			body := make([]byte, 4096)
			n, _ := resp.Body.Read(body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 && (needle == "" || strings.Contains(string(body[:n]), needle)) {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s never became ready", url)
}

// TestCWInsightsParityAgainstLocalStack renders the scenario through the
// real cloudwatch exporter (EMF: metric AND event records, so the
// outcome-marker discrimination is live) and puts the records into a
// LocalStack log group; the cwinsights querier must match memq.
func TestCWInsightsParityAgainstLocalStack(t *testing.T) {
	requireDocker(t, localstackImage)
	base := startContainer(t, localstackImage, "4566", []string{"SERVICES=logs"})
	waitHTTP(t, base+"/_localstack/health", "logs")

	events, window := scenarioEvents()
	var buf bytes.Buffer
	exp := cwexport.New(cwexport.WithWriter(&buf))
	if err := exp.ExportEvents(context.Background(), events); err != nil {
		t.Fatalf("cloudwatch export: %v", err)
	}
	if err := exp.Shutdown(context.Background()); err != nil { // flush the buffered writer
		t.Fatalf("cloudwatch flush: %v", err)
	}

	const group, stream = "/shortfall/golden", "s1"
	cw := &rawLogs{base: base}
	cw.call(t, "CreateLogGroup", map[string]any{"logGroupName": group})
	cw.call(t, "CreateLogStream", map[string]any{"logGroupName": group, "logStreamName": stream})

	type logEvent struct {
		Timestamp int64  `json:"timestamp"`
		Message   string `json:"message"`
	}
	var puts []logEvent
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var rec struct {
			AWS struct {
				Timestamp int64 `json:"Timestamp"`
			} `json:"_aws"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("EMF line: %v", err)
		}
		puts = append(puts, logEvent{Timestamp: rec.AWS.Timestamp, Message: line})
	}
	sort.Slice(puts, func(i, j int) bool { return puts[i].Timestamp < puts[j].Timestamp })
	for i := 0; i < len(puts); i += 1000 {
		end := i + 1000
		if end > len(puts) {
			end = len(puts)
		}
		cw.call(t, "PutLogEvents", map[string]any{
			"logGroupName": group, "logStreamName": stream, "logEvents": puts[i:end],
		})
	}
	time.Sleep(2 * time.Second) // ingest settles

	q := cwinsights.New("us-east-1", group, "test", "test",
		cwinsights.WithEndpoint(base), cwinsights.WithPollInterval(200*time.Millisecond))
	assertParity(t, "cwinsights", q, events, window)
}

// rawLogs is the test's minimal CloudWatch Logs client (LocalStack accepts
// a static dummy signature).
type rawLogs struct{ base string }

func (r *rawLogs) call(t *testing.T, action string, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.base+"/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Logs_20140328."+action)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260829/us-east-1/logs/aws4_request, SignedHeaders=host, Signature=test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		out := make([]byte, 512)
		n, _ := resp.Body.Read(out)
		t.Fatalf("%s: status %d: %s", action, resp.StatusCode, out[:n])
	}
}
