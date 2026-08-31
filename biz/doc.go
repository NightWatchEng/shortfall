// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package biz holds the value types every other package speaks:
// Money (int64 minor units, never float), ValueContext (the propagated
// business context: flow, entity, customer, amount), and Outcome (a terminal
// stage transition). It also owns the biz.* attribute names and the PII
// guard that keeps card numbers and emails out of them.
package biz
