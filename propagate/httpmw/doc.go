// Package httpmw propagates ValueContext over HTTP: server middleware
// extracts it from W3C Baggage, a client RoundTripper injects it, and an
// ingress stamping hook lets the first hop that recognizes a flow attach
// flow and entity so every downstream failure already carries value context.
package httpmw
