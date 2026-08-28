// Package report renders an engine.Report three ways: JSON (machine),
// the text ledger block (the incident-channel artifact from proposal 4.5),
// and markdown (postmortems). Every leg shows its evidence tag
// (deterministic | estimate | trust), and the RENDERING INVARIANT that this
// package exists to hold is that realized value and estimated value are never
// merged into one headline number — a counterfactual added to a measurement
// is how these figures get weaponized. TestNoRendererSumsRealizedWithEstimate
// pins it.
package report
