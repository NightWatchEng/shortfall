// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package report renders an engine.Report three ways: JSON (machine), the
// text ledger block (incident channels), and markdown (postmortems). Every
// leg shows its evidence tag (deterministic | estimate | trust), and the
// rendering invariant this package exists to hold is that realized value and
// estimated value are never merged into one headline number.
// TestNoRendererSumsRealizedWithEstimate pins it.
package report
