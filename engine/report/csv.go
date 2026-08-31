// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"

	"github.com/NightWatchEng/shortfall/engine"
)

// sortStrings is a local alias so summary.go needs no extra import line.
func sortStrings(s []string) { sort.Strings(s) }

// CustomersCSV renders the customers leg's top accounts as CSV — the
// vendor-neutral attachment the incident writers post. One row per
// (customer, currency) in list order with currencies sorted; amounts are
// int64 minor units, never floats. A leg that names why it is unavailable
// returns that reason as an error rather than an empty-but-plausible file.
func CustomersCSV(r engine.Report) ([]byte, error) {
	if reason := r.Customers.NotAvailableReason; reason != "" {
		return nil, fmt.Errorf("report: customers leg unavailable: %s", reason)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"customer_id", "segment", "currency", "amount_minor"}); err != nil {
		return nil, err
	}
	for _, c := range r.Customers.TopN {
		curs := make([]string, 0, len(c.ByCurrency))
		for cur := range c.ByCurrency {
			curs = append(curs, cur)
		}
		sort.Strings(curs)
		for _, cur := range curs {
			if err := w.Write([]string{
				c.CustomerID, c.Segment, cur, strconv.FormatInt(c.ByCurrency[cur], 10),
			}); err != nil {
				return nil, err
			}
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
