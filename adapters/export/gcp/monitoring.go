package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
)

const (
	defaultMonitoringEndpoint = "https://monitoring.googleapis.com"
	defaultMetricPrefix       = "custom.googleapis.com/biz/"

	// maxSeriesPerRequest is the CreateTimeSeries limit the service enforces.
	maxSeriesPerRequest = 200

	// errBodyLimit bounds how much of an error response is quoted back.
	errBodyLimit = 512
)

// Fixed per-family label sets (ADR-0004). Order is the contract: it keys the
// per-series accumulator, so a reordering would split one series into two.
var (
	valueLabels    = []string{"flow", "stage", "outcome", "currency", "kind", "segment"}
	txnLabels      = []string{"flow", "stage", "outcome", "currency", "segment"}
	inflightLabels = []string{"flow", "stage", "age_bucket", "currency"}
	providerLabels = []string{"provider", "op", "outcome"}
	droppedLabels  = []string{"reason"}
)

// labelsFor returns the fixed label set for a metric family, or nil for a
// family this exporter does not recognise.
func labelsFor(name string) []string {
	switch name {
	case "biz_value_total":
		return valueLabels
	case "biz_txn_total":
		return txnLabels
	case "biz_inflight_value", "biz_inflight_count":
		return inflightLabels
	case "biz_provider_calls_total":
		return providerLabels
	case "biz_dropped_events_total":
		return droppedLabels
	default:
		return nil
	}
}

// isGauge reports whether a family carries a level rather than a delta. The
// in-flight families are the only gauges (ADR-0012); every other family
// arrives as a counter delta.
func isGauge(name string) bool {
	return name == "biz_inflight_value" || name == "biz_inflight_count"
}

// unknownFamilyError names a metric family the exporter does not recognise.
type unknownFamilyError struct{ name string }

func (e *unknownFamilyError) Error() string {
	return fmt.Sprintf("gcp: unknown metric family %q", e.name)
}

// Cloud Monitoring REST payload shapes. int64 values are JSON strings
// because that is proto3's encoding for 64-bit integers — money never
// touches a float on this path, which a double-valued point could not
// promise past 2^53 minor units.
type (
	timeSeriesRequest struct {
		TimeSeries []timeSeries `json:"timeSeries"`
	}

	timeSeries struct {
		Metric     metricDescriptor `json:"metric"`
		Resource   monitoredRes     `json:"resource"`
		MetricKind string           `json:"metricKind"`
		ValueType  string           `json:"valueType"`
		Points     []point          `json:"points"`
	}

	metricDescriptor struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	}

	monitoredRes struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	}

	point struct {
		Interval interval   `json:"interval"`
		Value    pointValue `json:"value"`
	}

	interval struct {
		StartTime string `json:"startTime,omitempty"`
		EndTime   string `json:"endTime"`
	}

	pointValue struct {
		Int64Value string `json:"int64Value"`
	}
)

// monitoringClient writes custom time series to Cloud Monitoring.
//
// Counter families arrive as deltas but Cloud Monitoring's custom metrics
// express a counter as CUMULATIVE — a running total over an interval that
// starts when the process did — so the client accumulates per series. The
// in-flight gauges carry levels and are written as GAUGE.
type monitoringClient struct {
	projectID string
	endpoint  string
	prefix    string
	doer      Doer
	start     time.Time

	mu      sync.Mutex
	totals  map[string]int64     // series key -> cumulative total
	gaugeAt map[string]time.Time // series key -> newest level applied
}

func newMonitoringClient(projectID, endpoint, prefix string, doer Doer) *monitoringClient {
	return &monitoringClient{
		projectID: projectID,
		endpoint:  strings.TrimRight(endpoint, "/"),
		prefix:    prefix,
		doer:      doer,
		start:     time.Now().UTC(),
		totals:    map[string]int64{},
		gaugeAt:   map[string]time.Time{},
	}
}

// export converts the batch to time series and writes them in chunks the
// service will accept.
func (m *monitoringClient) export(ctx context.Context, batch []emit.MetricPoint) error {
	series, err := m.seriesFor(batch)
	if err != nil {
		return err
	}
	for start := 0; start < len(series); start += maxSeriesPerRequest {
		end := min(start+maxSeriesPerRequest, len(series))
		if err := m.post(ctx, series[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// seriesFor converts points to time series under the accumulator lock, so a
// concurrent flush cannot interleave two reads of one running total.
func (m *monitoringClient) seriesFor(batch []emit.MetricPoint) ([]timeSeries, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]timeSeries, 0, len(batch))
	for _, p := range batch {
		labels := labelsFor(p.Name)
		if labels == nil {
			return nil, &unknownFamilyError{name: p.Name}
		}
		key := seriesKey(p.Name, p.Labels, labels)
		ts := timeSeries{
			Metric:    metricDescriptor{Type: m.metricType(p.Name), Labels: labelValues(p.Labels, labels)},
			Resource:  monitoredRes{Type: "global", Labels: map[string]string{"project_id": m.projectID}},
			ValueType: "INT64",
		}
		if isGauge(p.Name) {
			// A stale sample from an overlapping flush must not overwrite a
			// fresher level (emit's order-by-At contract).
			if last, ok := m.gaugeAt[key]; ok && p.At.Before(last) {
				continue
			}
			m.gaugeAt[key] = p.At
			ts.MetricKind = "GAUGE"
			ts.Points = []point{{
				Interval: interval{EndTime: rfc3339(p.At)},
				Value:    pointValue{Int64Value: strconv.FormatInt(p.Value, 10)},
			}}
			out = append(out, ts)
			continue
		}
		if p.Value < 0 {
			return nil, fmt.Errorf("gcp: negative delta %d for counter family %q — counters only increase", p.Value, p.Name)
		}
		m.totals[key] += p.Value
		ts.MetricKind = "CUMULATIVE"
		ts.Points = []point{{
			Interval: interval{StartTime: rfc3339(m.start), EndTime: rfc3339(m.endAfterStart(p.At))},
			Value:    pointValue{Int64Value: strconv.FormatInt(m.totals[key], 10)},
		}}
		out = append(out, ts)
	}
	return out, nil
}

// endAfterStart keeps a cumulative interval non-empty: the service rejects a
// point whose end is not after its start, which a point observed in the same
// millisecond the process started would otherwise be.
func (m *monitoringClient) endAfterStart(at time.Time) time.Time {
	if at.After(m.start) {
		return at
	}
	return m.start.Add(time.Millisecond)
}

func (m *monitoringClient) metricType(family string) string {
	return m.prefix + strings.TrimPrefix(family, "biz_")
}

func (m *monitoringClient) post(ctx context.Context, series []timeSeries) error {
	body, err := json.Marshal(timeSeriesRequest{TimeSeries: series})
	if err != nil {
		return fmt.Errorf("gcp: marshal time series: %w", err)
	}
	url := fmt.Sprintf("%s/v3/projects/%s/timeSeries", m.endpoint, m.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gcp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.doer.Do(req)
	if err != nil {
		return fmt.Errorf("gcp: create time series: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return fmt.Errorf("gcp: create time series: %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}
	return nil
}

// seriesKey identifies one time series: the family plus its label values in
// the family's fixed order.
func seriesKey(family string, got map[string]string, order []string) string {
	var b strings.Builder
	b.WriteString(family)
	for _, name := range order {
		b.WriteByte(0)
		b.WriteString(got[name])
	}
	return b.String()
}

// labelValues pulls the family's labels out of the point. A label the point
// omits becomes the empty string: the emit layer already applied ADR-0004's
// fallbacks, so an absent key means "no value", never a defect to paper over.
func labelValues(got map[string]string, order []string) map[string]string {
	out := make(map[string]string, len(order))
	for _, name := range order {
		out[name] = got[name]
	}
	return out
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
