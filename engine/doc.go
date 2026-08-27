// Package engine computes the impact report for a window and scope:
// realized, deferred, unrealized, customers, and coverage. It imports only
// query and registry; if an engine change needs a new Querier method, that
// is a design smell, not a reason to widen the boundary.
package engine
