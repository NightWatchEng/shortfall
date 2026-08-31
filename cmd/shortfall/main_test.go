// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown verb", []string{"frobnicate"}, 2},
		{"version", []string{"version"}, 0},
		{"validate ok", []string{"validate", "../../registry/testdata/registry.yaml"}, 0},
		{"validate missing file", []string{"validate", "nope.yaml"}, 1},
		{"validate wrong arity", []string{"validate"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run(c.args); got != c.want {
				t.Errorf("run(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}
