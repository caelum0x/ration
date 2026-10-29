package httpapi

// This file closes a real gap in this facade's own abuse protection.
//
// gateway.proto's Gateway gRPC server (services/gateway/internal/
// ratelimit) rate-limits every RPC per tenant_id — or, for GetCard/
// AdjustCardLimit, per card_id resolved to its owning tenant — via
// ratelimit.keyFor. But cmd/gateway/main.go's REST facade (this
// package) reaches that same gRPC server over exactly ONE long-lived
// self-dial connection (selfConn in cmd/gateway/main.go), never one
// connection per real HTTP client that hits the facade. For the
// handful of RPCs this package calls that carry neither tenant_id nor
// card_id — GetDashboardSnapshot (dashboard.go, always unscoped by
// design) and StreamActivity when called with an empty tenant_id
// (activity.go's/dashboard.go's own documented "all tenants" mode) —
// ratelimit.keyFor falls back to "peer:<addr>", and that peer is
// always the same loopback selfConn connection no matter which real
// browser or HTTP client actually asked. Every REST caller of those
// specific routes therefore shares ONE token bucket at the gRPC layer,
// the exact opposite of "one noisy caller can't starve every other
// caller" that package's own doc comment promises — most visible on
// GET /v1/dashboard/snapshot, the one surface this whole project
// explicitly designs to be a zero-login, judge-facing, possibly
// multi-viewer public page (dashboard.go's own package doc comment),
// polled every 3 seconds by every open browser tab (dashboard.html's
// REFRESH_MS).
//
// Fix: a second, HTTP-layer limiter in front of exactly those call
// sites, keyed by the real HTTP client's own remote address
// (r.RemoteAddr — not a forwarded header; this project is not deployed
// behind a reverse proxy today, the same no-spoofable-header-trust
// posture services/webhook-ingestion/internal/ratelimit already
// established for its own per-IP bucket). Every other route in this
// package keeps its existing, correctly per-tenant-scoped gRPC-layer
// protection unchanged — this is additive, not a replacement, and
// never double-charges a request that's already correctly tenant- or
// card-scoped at the gRPC layer.

import (
	"log/slog"
	"net"
	"net/http"

	"google.golang.org/grpc/codes"
)

// defaultPublicRateLimitRPS/Burst are deliberately generous for a
// read-only, low-cost route polled every few seconds by a single
// browser tab (dashboard.html's REFRESH_MS=3000) — this exists to stop
// one abusive IP from exhausting the gRPC layer's single collapsed
// bucket for everyone, not to meter normal multi-viewer dashboard
// usage.
const (
	defaultPublicRateLimitRPS   = 10
	defaultPublicRateLimitBurst = 30
)

// allowPublicRoute reports whether r's real client address still has a
// token in h.publicLimiter's IP-keyed bucket, consuming one if so. On
// denial it writes a 429 response (the same JSON error envelope and
// gRPC-status-code-as-string convention writeGRPCError already uses
// for a gRPC-originated ResourceExhausted) and returns false — callers
// must return immediately without calling into h.client.
func (h *Handler) allowPublicRoute(w http.ResponseWriter, r *http.Request) bool {
	if h.publicLimiter.Allow("ip:" + clientIP(r)) {
		return true
	}
	writeTooManyRequests(w, h.log, "rate limit exceeded for "+r.URL.Path)
	return false
}

// clientIP extracts just the IP portion of r.RemoteAddr (host:port),
// falling back to the raw value if it doesn't parse as host:port (e.g.
// a test harness or listener that reports a bare IP with no port).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// writeTooManyRequests mirrors writeGRPCError's JSON error envelope for
// the one case this package produces a ResourceExhausted status
// without an underlying gRPC call to translate it from.
func writeTooManyRequests(w http.ResponseWriter, log *slog.Logger, message string) {
	body := errorBody{}
	body.Error.Code = codes.ResourceExhausted.String()
	body.Error.Message = message
	writeJSON(w, log, http.StatusTooManyRequests, body)
}
