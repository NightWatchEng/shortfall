// Package query defines the only questions the engine may ask a backend:
// sum, count, group-by, and time range over metrics, and filter plus
// group-by over events. Query adapters translate this AST; nothing
// vendor-specific crosses it.
package query
