// Package httpmw propagates ValueContext over HTTP: server middleware
// extracts it from W3C Baggage, a client Transport injects it — and only
// it — toward registry-allowlisted hosts (ADR-0003, deny by default),
// and an ingress stamping hook lets the first hop that recognizes a flow
// attach flow, entity, and amount so every downstream failure already
// carries value context.
//
// The Transport is the egress FENCE, not just an injector. It fails
// CLOSED: toward a host outside the allowlist it removes the biz.vc
// member — even one a globally-installed generic Baggage propagator
// added, even when the surrounding header is otherwise malformed —
// rewriting the header from the recovered members rather than forwarding
// unknown bytes onward. Amounts and customer hashes leave your estate
// only on purpose.
//
// Host shapes: allowlist entries are lowercase DNS names or dotted IPv4
// literals. IPv6 literals and non-punycode IDNs are always OUTSIDE the
// fence (they cannot be allowlisted), so biz.vc is never injected toward
// them and always stripped.
package httpmw

import (
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/baggage"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

const baggageHeader = "baggage"

// recoverMembers parses EVERY baggage field line (HTTP allows more than
// one) and returns the valid members by key. otel's Parse skips invalid
// members and returns the valid ones ALONGSIDE an error, so a malformed
// neighbour never hides a valid biz.vc — the fence works on what was
// recoverable, and a genuinely unparsable value simply yields no member
// for that key rather than being waved through. hadError reports whether
// anything was dropped, for logging.
func recoverMembers(h http.Header) (members map[string]baggage.Member, hadError bool) {
	members = map[string]baggage.Member{}
	for _, line := range h.Values(baggageHeader) {
		if line == "" {
			continue
		}
		bag, err := baggage.Parse(line)
		if err != nil {
			hadError = true
		}
		for _, m := range bag.Members() {
			members[m.Key()] = m
		}
	}
	return members, hadError
}

// writeMembers replaces ALL baggage field lines with a single canonical
// one built from members (or deletes the header when none remain).
func writeMembers(h http.Header, members map[string]baggage.Member) error {
	if len(members) == 0 {
		h.Del(baggageHeader)
		return nil
	}
	list := make([]baggage.Member, 0, len(members))
	for _, m := range members {
		list = append(list, m)
	}
	bag, err := baggage.New(list...)
	if err != nil {
		return err
	}
	h.Set(baggageHeader, bag.String())
	return nil
}

// IngressFunc recognizes a flow on an incoming request and builds its
// ValueContext. Return false when the request is not a flow entry point.
type IngressFunc func(*http.Request) (biz.ValueContext, bool)

// MWOption configures Middleware.
type MWOption func(*mwConfig)

type mwConfig struct {
	ingress IngressFunc
	logger  *slog.Logger
}

// WithIngress installs the stamping hook.
func WithIngress(f IngressFunc) MWOption { return func(c *mwConfig) { c.ingress = f } }

// WithMWLogger sets the warning logger (default slog.Default).
func WithMWLogger(l *slog.Logger) MWOption { return func(c *mwConfig) { c.logger = l } }

// Middleware returns server middleware that makes biz.FromContext work
// downstream. Precedence: a valid biz.vc member on the wire wins — even
// if a neighbouring member was malformed (the valid one is recovered).
// Absent a valid wire context, the ingress hook stamps (its output is
// validated — a hook emitting PII or nonsense is rejected loudly, the
// request itself always proceeds). Corrupt/absent are logged distinctly.
func Middleware(reg *registry.Registry, opts ...MWOption) func(http.Handler) http.Handler {
	cfg := mwConfig{logger: slog.Default()}
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			members, hadErr := recoverMembers(r.Header)
			if hadErr {
				cfg.logger.Warn("httpmw: malformed member(s) in inbound baggage dropped; valid members kept")
			}
			if len(members) > 0 {
				list := make([]baggage.Member, 0, len(members))
				for _, m := range members {
					list = append(list, m)
				}
				if bag, err := baggage.New(list...); err == nil {
					ctx = baggage.ContextWithBaggage(ctx, bag)
				}
			}

			if _, ok, decErr := biz.FromContext(ctx); !ok {
				if decErr != nil {
					cfg.logger.Warn("httpmw: corrupt biz.vc on the wire — dropped loudly", "error", decErr)
				}
				if cfg.ingress != nil {
					if vc, stamp := cfg.ingress(r); stamp {
						vc = estimate(reg, vc)
						if err := vc.Validate(); err != nil {
							cfg.logger.Warn("httpmw: ingress stamp rejected — the hook's output must satisfy the same fences as the wire", "error", err)
						} else if stamped, err := biz.WithValueContext(ctx, vc); err != nil {
							cfg.logger.Warn("httpmw: ingress stamp not encodable", "error", err)
						} else {
							ctx = stamped
						}
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// estimate fills an ingress stamp's UNKNOWN amount from the registry's
// entry-point estimator (proposal 4.2): when the hook knows the flow,
// segment, and currency but not the amount (e.g. GET /cart leaves Amount
// 0), the registry's default or by-segment value is stamped with
// Estimated=true so the engine reports it as estimate, never realized. A
// known amount, an already-estimated context, or a flow without an
// estimator is returned unchanged. Currency and exponent stay as the
// hook set them — the estimator supplies minor units only.
func estimate(reg *registry.Registry, vc biz.ValueContext) biz.ValueContext {
	if vc.Money.Amount != 0 || vc.Estimated || reg == nil {
		return vc
	}
	f, ok := reg.Flow(vc.Flow)
	if !ok {
		return vc
	}
	if est, ok := f.EstimateMinor(vc.Segment); ok {
		vc.Money.Amount = est
		vc.Estimated = true
	}
	return vc
}

// Transport is the client-side egress fence. It implements
// http.RoundTripper and is invoked once per redirect hop, so a redirect
// from an allowed host to a disallowed one is fenced at the second hop.
type Transport struct {
	reg    *registry.Registry
	base   http.RoundTripper
	logger *slog.Logger
}

// TransportOption configures NewTransport.
type TransportOption func(*Transport)

// WithTransportLogger sets the warning logger (default slog.Default).
func WithTransportLogger(l *slog.Logger) TransportOption {
	return func(t *Transport) { t.logger = l }
}

// NewTransport wraps base (nil means http.DefaultTransport) with the
// biz.vc egress fence for the registry's propagation allowlist.
func NewTransport(reg *registry.Registry, base http.RoundTripper, opts ...TransportOption) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &Transport{reg: reg, base: base, logger: slog.Default()}
	for _, o := range opts {
		o(t)
	}
	return t
}

// RoundTrip fences the outbound baggage header. It ALWAYS rebuilds the
// header from the recovered members (never forwards unknown bytes),
// cloning the request first (net/http forbids mutating the original):
//   - host allowed: inject the ctx's ValueContext as biz.vc, replacing
//     any stale member;
//   - host NOT allowed: remove biz.vc — including one a global propagator
//     added or one hidden behind a malformed neighbour;
//   - foreign members pass through in every case.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	allowed := t.reg.Propagation.HostAllowed(host)

	members, hadErr := recoverMembers(req.Header)
	if hadErr {
		t.logger.Warn("httpmw: malformed member(s) in outbound baggage dropped", "host", host)
	}

	_, hadBizVC := members[biz.MemberKey]

	if allowed {
		if vc, ok, decErr := biz.FromContext(req.Context()); ok {
			if enc, err := biz.EncodeVC(vc); err != nil {
				t.logger.Warn("httpmw: ValueContext not encodable at egress — not injected", "error", err)
			} else if m, err := baggage.NewMemberRaw(biz.MemberKey, enc); err != nil {
				t.logger.Warn("httpmw: biz.vc member not constructible at egress — not injected", "error", err)
			} else {
				members[biz.MemberKey] = m
			}
		} else if decErr != nil {
			t.logger.Warn("httpmw: corrupt biz.vc in egress context — not injected", "error", decErr)
		}
	} else if hadBizVC {
		delete(members, biz.MemberKey)
		t.logger.Warn("httpmw: biz.vc stripped at egress — host is outside the propagation allowlist", "host", host)
	}

	// Always rebuild from the recovered members onto a clone: the
	// original header's raw bytes never reach the wire, so a malformed
	// or multi-line header can never smuggle biz.vc past the fence.
	clone := req.Clone(req.Context())
	if err := writeMembers(clone.Header, members); err != nil {
		// Fail CLOSED: if we cannot express a safe header, send none
		// rather than forward a possibly-leaky original.
		t.logger.Warn("httpmw: could not rebuild outbound baggage; sending no baggage header", "error", err)
		clone.Header.Del(baggageHeader)
	}
	return t.base.RoundTrip(clone)
}
