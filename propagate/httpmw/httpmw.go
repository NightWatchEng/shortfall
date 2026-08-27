// Package httpmw propagates ValueContext over HTTP: server middleware
// extracts it from W3C Baggage, a client Transport injects it — and only
// it — toward registry-allowlisted hosts (ADR-0003, deny by default),
// and an ingress stamping hook lets the first hop that recognizes a flow
// attach flow, entity, and amount so every downstream failure already
// carries value context.
//
// The Transport is also the egress FENCE, not just an injector: toward a
// host outside the allowlist it strips a biz.vc member that a
// globally-installed generic Baggage propagator may have added — amounts
// and customer hashes leave your estate only on purpose.
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
// downstream. Precedence: a valid biz.vc member on the wire wins; absent
// that, the ingress hook (validated — a hook emitting PII or nonsense is
// rejected loudly, the request itself always proceeds); corrupt wire
// context is logged and dropped, never mistaken for absent.
func Middleware(reg *registry.Registry, opts ...MWOption) func(http.Handler) http.Handler {
	cfg := mwConfig{logger: slog.Default()}
	for _, o := range opts {
		o(&cfg)
	}
	_ = reg // reserved: the estimator hook (next milestone step) reads flow estimators
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if header := r.Header.Get(baggageHeader); header != "" {
				if bag, err := baggage.Parse(header); err == nil {
					ctx = baggage.ContextWithBaggage(ctx, bag)
				} else {
					cfg.logger.Warn("httpmw: unparsable baggage header dropped", "error", err)
				}
			}
			if _, ok, decErr := biz.FromContext(ctx); !ok {
				if decErr != nil {
					cfg.logger.Warn("httpmw: corrupt biz.vc on the wire — dropped loudly", "error", decErr)
				}
				if cfg.ingress != nil {
					if vc, stamp := cfg.ingress(r); stamp {
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

// Transport is the client-side fence. It implements http.RoundTripper.
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

// RoundTrip clones the request (net/http forbids mutating the original)
// and reconciles the outbound baggage header:
//   - host allowed: the ctx's ValueContext is injected as biz.vc
//     (replacing any stale member already on the header);
//   - host NOT allowed: any biz.vc member is STRIPPED — including one a
//     global Baggage propagator added — and foreign members pass through
//     untouched either way.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	allowed := t.reg.Propagation.HostAllowed(host)

	members := map[string]baggage.Member{}
	if header := req.Header.Get(baggageHeader); header != "" {
		if bag, err := baggage.Parse(header); err == nil {
			for _, m := range bag.Members() {
				members[m.Key()] = m
			}
		} else {
			t.logger.Warn("httpmw: unparsable outbound baggage header left untouched", "error", err)
			return t.base.RoundTrip(req)
		}
	}

	changed := false
	if allowed {
		if vc, ok, _ := biz.FromContext(req.Context()); ok {
			enc, err := biz.EncodeVC(vc)
			if err != nil {
				t.logger.Warn("httpmw: ValueContext not encodable at egress — not injected", "error", err)
			} else if m, err := baggage.NewMemberRaw(biz.MemberKey, enc); err == nil {
				members[biz.MemberKey] = m
				changed = true
			}
		}
	}
	if !allowed {
		if _, present := members[biz.MemberKey]; present {
			delete(members, biz.MemberKey)
			changed = true
			t.logger.Warn("httpmw: biz.vc stripped at egress — host is outside the propagation allowlist", "host", host)
		}
	}
	if !changed {
		return t.base.RoundTrip(req)
	}

	clone := req.Clone(req.Context())
	list := make([]baggage.Member, 0, len(members))
	for _, m := range members {
		list = append(list, m)
	}
	if len(list) == 0 {
		clone.Header.Del(baggageHeader)
	} else {
		bag, err := baggage.New(list...)
		if err != nil {
			t.logger.Warn("httpmw: rebuilding outbound baggage failed; header left untouched", "error", err)
			return t.base.RoundTrip(req)
		}
		clone.Header.Set(baggageHeader, bag.String())
	}
	return t.base.RoundTrip(clone)
}
