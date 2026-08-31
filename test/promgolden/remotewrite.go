// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package promgolden seeds a real Prometheus with the harness's metric points
// (via the remote-write protocol) and asserts the promql adapter reads back the
// same Series the in-memory reference (memq) does — the live numeric-parity
// harness. It lives in its own module so its test-only deps
// (snappy, the Docker orchestration) never touch the promql adapter's go.mod.
package promgolden

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/golang/snappy"

	"github.com/NightWatchEng/shortfall/emit"
)

// gaugeFamilies are the metric names Prometheus stores as a level (written
// as-is). Everything else is a counter and must be cumulated before seeding.
var gaugeFamilies = map[string]bool{"biz_inflight_value": true, "biz_inflight_count": true}

// cumulativeForPrometheus converts the harness's per-event delta counter points
// into the cumulative (monotonically increasing) series a real Prometheus
// counter client would expose, so the adapter's `m@To - m@From` recovers the
// in-window sum — matching memq, which sums the deltas directly. Gauge families
// pass through unchanged. Within each counter series, points are sorted by time
// and each value becomes the running total.
func cumulativeForPrometheus(points []emit.MetricPoint) []emit.MetricPoint {
	key := func(p emit.MetricPoint) string {
		labels := make([][2]string, 0, len(p.Labels))
		for k, v := range p.Labels {
			labels = append(labels, [2]string{k, v})
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i][0] < labels[j][0] })
		s := p.Name
		for _, l := range labels {
			s += "\x00" + l[0] + "=" + l[1]
		}
		return s
	}

	var out []emit.MetricPoint
	// Counter series -> one representative point + the delta summed per timestamp.
	// Same-minute events share a timestamp; Prometheus keeps one sample per
	// (series, timestamp), so cumulating per raw point would drop the extra
	// steps and undercount. Collapse to one cumulative sample per distinct
	// timestamp: sum the deltas at each timestamp, then running-sum those.
	type acc struct {
		rep   emit.MetricPoint
		perTS map[int64]int64 // unixNano -> summed delta at that instant
	}
	counters := map[string]*acc{}
	var order []string
	for _, p := range points {
		if gaugeFamilies[p.Name] {
			out = append(out, p) // gauges pass through as levels
			continue
		}
		k := key(p)
		a := counters[k]
		if a == nil {
			a = &acc{rep: p, perTS: map[int64]int64{}}
			counters[k] = a
			order = append(order, k)
		}
		a.perTS[p.At.UnixNano()] += p.Value
	}
	for _, k := range order {
		a := counters[k]
		tss := make([]int64, 0, len(a.perTS))
		for ts := range a.perTS {
			tss = append(tss, ts)
		}
		sort.Slice(tss, func(i, j int) bool { return tss[i] < tss[j] })
		var running int64
		for _, ts := range tss {
			running += a.perTS[ts]
			p := a.rep
			p.At = time.Unix(0, ts).UTC()
			p.Value = running
			out = append(out, p)
		}
	}
	return out
}

// remoteWrite groups metric points into Prometheus timeseries and pushes them
// through Prometheus's remote-write receiver. The WriteRequest protobuf is
// hand-encoded (four tiny messages) so this module needs no protobuf toolchain
// or the heavy prometheus module — only snappy, which the protocol mandates.
func remoteWrite(ctx context.Context, baseURL string, points []emit.MetricPoint) error {
	// Group samples by their full identity: metric name + sorted labels.
	type series struct {
		labels  [][2]string // sorted; includes __name__
		samples [][2]int64  // (bits, tsMillis) — value stored as Float64bits
	}
	byKey := map[string]*series{}
	var order []string
	for _, p := range points {
		labels := make([][2]string, 0, len(p.Labels)+1)
		labels = append(labels, [2]string{"__name__", p.Name})
		for k, v := range p.Labels {
			labels = append(labels, [2]string{k, v})
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i][0] < labels[j][0] })
		key := ""
		for _, l := range labels {
			key += l[0] + "\x00" + l[1] + "\x00"
		}
		s := byKey[key]
		if s == nil {
			s = &series{labels: labels}
			byKey[key] = s
			order = append(order, key)
		}
		s.samples = append(s.samples, [2]int64{int64(math.Float64bits(float64(p.Value))), p.At.UnixMilli()})
	}

	var wr []byte
	for _, key := range order {
		s := byKey[key]
		sort.Slice(s.samples, func(i, j int) bool { return s.samples[i][1] < s.samples[j][1] })
		var ts []byte
		for _, l := range s.labels {
			ts = append(ts, field(1, lengthDelim(labelMsg(l[0], l[1])))...) // TimeSeries.labels
		}
		for _, smp := range s.samples {
			ts = append(ts, field(2, lengthDelim(sampleMsg(math.Float64frombits(uint64(smp[0])), smp[1])))...) // TimeSeries.samples
		}
		wr = append(wr, field(1, lengthDelim(ts))...) // WriteRequest.timeseries
	}

	body := snappy.Encode(nil, wr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/write", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		return fmt.Errorf("remote_write: HTTP %d: %s", resp.StatusCode, b[:n])
	}
	return nil
}

// --- minimal protobuf wire encoding (proto3) ---

func labelMsg(name, value string) []byte {
	var b []byte
	b = append(b, field(1, lengthDelim([]byte(name)))...)
	b = append(b, field(2, lengthDelim([]byte(value)))...)
	return b
}

func sampleMsg(value float64, tsMillis int64) []byte {
	var b []byte
	b = append(b, tag(1, 1)) // field 1, wire type 1 (fixed64)
	var f [8]byte
	binary.LittleEndian.PutUint64(f[:], math.Float64bits(value))
	b = append(b, f[:]...)
	b = append(b, tag(2, 0)) // field 2, wire type 0 (varint)
	b = append(b, varint(uint64(tsMillis))...)
	return b
}

// field emits a length-delimited (wire type 2) field: tag + len + payload.
func field(num int, payload []byte) []byte {
	out := []byte{tag(num, 2)}
	out = append(out, varint(uint64(len(payload)))...)
	return append(out, payload...)
}

func lengthDelim(b []byte) []byte { return b }

func tag(num, wire int) byte { return byte(num<<3 | wire) }

func varint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// waitReady polls the Prometheus readiness endpoint until it responds or ctx
// expires.
func waitReady(ctx context.Context, baseURL string) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/-/ready", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}
