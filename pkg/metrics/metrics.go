// Package metrics provides the Prometheus /metrics endpoint and the
// handful of named metrics docs/SCALE-EXPANSION.md section 2 calls out
// by name: ration_bridge_grants_total and
// ration_reserve_floor_headroom_minor_units, plus generic per-RPC
// request count/latency instrumentation for every gRPC server in the
// project (gateway, ledger-core is instrumented separately in Rust).
//
// Each service owns its own Metrics (and its own prometheus.Registry) —
// metrics are not process-global state here, so tests can construct a
// throwaway Metrics without colliding with prometheus's default global
// registry.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/status"
)

// namespace prefixes every metric this package registers, matching the
// ration_* naming already specified for the two named metrics.
const namespace = "ration"

// Metrics bundles one service's Prometheus registry and metric
// instruments. Construct one with New(serviceName) in main.go and keep
// it alive for the process lifetime.
type Metrics struct {
	registry *prometheus.Registry

	grpcRequestsTotal   *prometheus.CounterVec
	grpcRequestDuration *prometheus.HistogramVec

	// BridgeGrantsTotal is ration_bridge_grants_total from
	// docs/SCALE-EXPANSION.md section 2: every time
	// bridge.Service.RationBridgeFunding resolves to granted > 0 (fully
	// or partially), the "result" label records which, and declines are
	// counted too so the ratio is visible. Tenant ID is deliberately NOT
	// a label (unbounded-cardinality labels are a real production
	// Prometheus footgun) — per-tenant history is already in
	// ledger-core's AgentDecision audit trail for that granularity.
	BridgeGrantsTotal *prometheus.CounterVec

	// ReserveFloorHeadroom is
	// ration_reserve_floor_headroom_minor_units from
	// docs/SCALE-EXPANSION.md section 2: the gauge is set to a tenant's
	// current (available_balance - reserve_floor), clamped at zero,
	// every time the bridge or card services recompute it. Labeled by
	// tenant_id: cardinality here is bounded by the number of real
	// tenants onboarded (tens to low thousands at this project's scale),
	// which is the normal, documented tradeoff for a per-entity gauge.
	ReserveFloorHeadroom *prometheus.GaugeVec
}

// New constructs a Metrics instance registered against a fresh
// (non-global) prometheus.Registry, tagged with serviceName so multi-
// service dashboards can filter by it.
func New(serviceName string) *Metrics {
	reg := prometheus.NewRegistry()
	constLabels := prometheus.Labels{"service": serviceName}

	m := &Metrics{
		registry: reg,
		grpcRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "grpc_server_requests_total",
			Help:        "Total gRPC server requests, by RPC method and outcome.",
			ConstLabels: constLabels,
		}, []string{"method", "code"}),
		grpcRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   namespace,
			Name:        "grpc_server_request_duration_seconds",
			Help:        "gRPC server request latency, by RPC method.",
			ConstLabels: constLabels,
			Buckets:     prometheus.DefBuckets,
		}, []string{"method"}),
		BridgeGrantsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "bridge_grants_total",
			Help:        "Total bridge-funding decisions, by result (granted, partial, declined).",
			ConstLabels: constLabels,
		}, []string{"result"}),
		ReserveFloorHeadroom: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "reserve_floor_headroom_minor_units",
			Help:        "Current (available_balance - reserve_floor) per tenant, clamped at zero, in minor currency units.",
			ConstLabels: constLabels,
		}, []string{"tenant_id"}),
	}

	reg.MustRegister(
		m.grpcRequestsTotal,
		m.grpcRequestDuration,
		m.BridgeGrantsTotal,
		m.ReserveFloorHeadroom,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return m
}

// Handler returns the http.Handler to mount at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// observe records one completed RPC's outcome against both the counter
// and the latency histogram. Shared by the unary and streaming
// interceptors in interceptors.go.
func (m *Metrics) observe(method string, dur time.Duration, err error) {
	m.grpcRequestsTotal.WithLabelValues(method, status.Code(err).String()).Inc()
	m.grpcRequestDuration.WithLabelValues(method).Observe(dur.Seconds())
}
