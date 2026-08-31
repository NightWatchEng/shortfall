// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"strings"
	"testing"

	"github.com/NightWatchEng/shortfall/engine"
)

// TestCustomersCSV pins the vendor-neutral top-accounts export the incident
// writers attach: one row per (customer, currency), deterministic order
// (list order, currencies sorted), minor units, no floats.
func TestCustomersCSV(t *testing.T) {
	b, err := CustomersCSV(summaryFixture())
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"customer_id,segment,currency,amount_minor",
		"h:c000002,enterprise,USD,900000",
		"h:c000001,smb,EUR,700",
		"h:c000001,smb,USD,14900",
		"",
	}, "\n")
	if string(b) != want {
		t.Fatalf("csv =\n%s\nwant\n%s", b, want)
	}
}

// TestCustomersCSVNotAvailable pins the fail-loud contract: a leg that says
// why it is unavailable never renders an empty-but-plausible CSV.
func TestCustomersCSVNotAvailable(t *testing.T) {
	r := summaryFixture()
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	if _, err := CustomersCSV(r); err == nil || !strings.Contains(err.Error(), "no events") {
		t.Fatalf("want the leg's unavailability reason as an error, got %v", err)
	}
}
