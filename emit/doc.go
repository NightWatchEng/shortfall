// Package emit turns stage transitions into the two normalized signals:
// bounded metrics (sums and counts with a fixed label set) and unsampled
// per-transaction outcome events. It never touches a backend directly;
// exporters do.
package emit
