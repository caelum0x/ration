//! OpenTelemetry tracing setup for ledger-core — the Rust equivalent of
//! `pkg/telemetry` (Go), per docs/SCALE-EXPANSION.md section 2: "Rust
//! ledger-core: `tracing` + `tracing-opentelemetry`, same trace
//! propagation via gRPC metadata."
//!
//! Design, mirrored 1:1 from pkg/telemetry's Go side:
//!   - [`init`] installs a batching OTLP/gRPC trace exporter talking to
//!     the local otel-collector (infra/otel-collector) and registers
//!     `service_name`/`service_version` as resource attributes, exactly
//!     like `telemetry.Setup` on the Go side.
//!   - The actual W3C `traceparent` extraction/propagation across the
//!     gRPC hop is handled by `tonic-tracing-opentelemetry`'s
//!     `OtelGrpcLayer` (wired onto the server in `src/main.rs`), the Rust
//!     analog of `pkg/telemetry.ServerOption`/`DialOption` wrapping
//!     `otelgrpc`'s stats handlers — rather than hand-rolling header
//!     parsing here. ledger-core is a pure gRPC server in this project
//!     (it issues no outgoing RPCs of its own), so only the server side
//!     is needed.
//!   - Every `tracing::info!`/`tracing::error!`/`#[tracing::instrument]`
//!     call anywhere in this crate becomes both a structured JSON log
//!     line (via the `tracing_subscriber::fmt` JSON layer) and an
//!     OpenTelemetry span event (via the `tracing-opentelemetry` layer),
//!     so a gateway/agent-issued RPC's trace extends all the way into
//!     ledger-core's own ledger-write logic.
//!
//! Like the Go side, this is deliberately fail-open: the OTLP/gRPC
//! exporter connects lazily (`tonic`'s `connect_lazy`), so a missing
//! collector never blocks startup or the request path — observability
//! must never become a new way for the ledger to go down.

use std::error::Error;
use std::task::{Context, Poll};

use http::{Request, Response};
use opentelemetry::trace::TracerProvider as _;
use opentelemetry::KeyValue;
use opentelemetry_otlp::{SpanExporter, WithExportConfig};
use opentelemetry_sdk::trace::SdkTracerProvider;
use opentelemetry_sdk::Resource;
use tonic::server::NamedService;
use tonic_tracing_opentelemetry::middleware::server::{OtelGrpcLayer, OtelGrpcService};
use tower::{Layer, Service};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::EnvFilter;

/// Matches infra/otel-collector's gRPC OTLP receiver port (4317) in the
/// local docker-compose observability stack — the same default
/// `pkg/telemetry.defaultOTLPEndpoint` uses on the Go side.
const DEFAULT_OTLP_ENDPOINT: &str = "http://localhost:4317";

/// Keeps the installed [`SdkTracerProvider`] alive and flushes it on
/// drop — the idiomatic Rust equivalent of `pkg/telemetry.ShutdownFunc`.
/// Bind this to a variable with process-lifetime scope in `main`
/// (`let _telemetry = telemetry::init(...)?;`); dropping it immediately
/// (`let _ = telemetry::init(...)?;`) would flush and shut the exporter
/// down right away, silently losing every span the process emits.
pub struct TelemetryGuard {
    provider: SdkTracerProvider,
}

impl Drop for TelemetryGuard {
    fn drop(&mut self) {
        if let Err(err) = self.provider.shutdown() {
            // Mirrors pkg/telemetry.Setup's shutdown: best-effort, logged,
            // never panics — a flush failure on exit must not turn into a
            // crash on the way out.
            tracing::warn!(error = %err, "telemetry: tracer provider shutdown failed");
        }
    }
}

/// Installs the global `tracing` subscriber — structured JSON logs (the
/// Rust side of docs/SCALE-EXPANSION.md section 2's "Structured JSON
/// logging everywhere") layered with an OpenTelemetry exporter so every
/// span is also exported over OTLP/gRPC to the local collector.
///
/// Call once, at the very top of `main`, before any other `tracing::*`
/// call. Returns a [`TelemetryGuard`] that must be kept alive for the
/// process lifetime so the final batch of spans is flushed on exit
/// rather than dropped.
pub fn init(service_name: &str, service_version: &str) -> Result<TelemetryGuard, Box<dyn Error>> {
    let endpoint = endpoint_from_env().unwrap_or_else(|| DEFAULT_OTLP_ENDPOINT.to_string());

    let exporter = SpanExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint)
        .build()?;

    let resource = Resource::builder()
        .with_service_name(service_name.to_string())
        .with_attribute(KeyValue::new(
            "service.version",
            service_version.to_string(),
        ))
        .with_attribute(KeyValue::new(
            "ration.deployment.component",
            service_name.to_string(),
        ))
        .build();

    let provider = SdkTracerProvider::builder()
        .with_resource(resource)
        .with_batch_exporter(exporter)
        .build();

    opentelemetry::global::set_tracer_provider(provider.clone());

    let tracer = provider.tracer(service_name.to_string());
    let otel_layer = tracing_opentelemetry::layer().with_tracer(tracer);

    let env_filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));

    tracing_subscriber::registry()
        .with(env_filter)
        .with(tracing_subscriber::fmt::layer().json())
        .with(otel_layer)
        .try_init()?;

    Ok(TelemetryGuard { provider })
}

/// Wraps a generated tonic server (e.g. `LedgerCoreServer::new(..)`) with
/// `tonic-tracing-opentelemetry`'s [`OtelGrpcLayer`] — the W3C
/// `traceparent` extraction/span-creation half of docs/SCALE-EXPANSION.md
/// section 2's "gRPC client/server interceptors propagating trace
/// context across every service hop" — while preserving
/// [`tonic::server::NamedService`] so the result can still be passed to
/// `tonic::transport::Server::add_service`.
///
/// This exists instead of the crate's own documented
/// `Server::builder().layer(OtelGrpcLayer::default())` pattern because
/// that pattern only type-checks against tonic versions whose merged
/// `Routes` service has a `Sized`, directly-`std::error::Error`
/// `Error` type. tonic 0.12 (pinned above for `tonic::transport::tls`)
/// gives `Routes` the error type `Box<dyn std::error::Error + Send +
/// Sync>`, which — because `std`'s blanket `impl<T: Error> Error for
/// Box<T>` requires `T: Sized` — does **not** itself implement
/// `std::error::Error`, and `OtelGrpcService`'s own `Service` impl
/// requires exactly that bound on the wrapped service's `Error` type.
/// Applying the layer one level lower, directly to the generated
/// per-service type (whose `Service::Error` is `Infallible`, which
/// trivially satisfies the bound), sidesteps the mismatch entirely —
/// `wrap` is the one extra step that makes that work while keeping
/// `NAME` intact for routing.
#[derive(Clone)]
pub struct TracedService<S>(OtelGrpcService<S>);

impl<S> TracedService<S> {
    /// Wraps `inner` so every RPC it serves gets a tracing span joined
    /// to the caller's trace (or a fresh root span if the caller sent
    /// none).
    pub fn wrap(inner: S) -> Self {
        Self(OtelGrpcLayer::default().layer(inner))
    }
}

impl<S: NamedService> NamedService for TracedService<S> {
    const NAME: &'static str = S::NAME;
}

impl<S, B, B2> Service<Request<B>> for TracedService<S>
where
    S: Service<Request<B>, Response = Response<B2>> + Clone + Send + 'static,
    S::Error: std::error::Error,
    B: Send + 'static,
{
    type Response = S::Response;
    type Error = S::Error;
    type Future = <OtelGrpcService<S> as Service<Request<B>>>::Future;

    fn poll_ready(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        self.0.poll_ready(cx)
    }

    fn call(&mut self, req: Request<B>) -> Self::Future {
        self.0.call(req)
    }
}

/// Resolves the OTLP collector endpoint, preferring the
/// OpenTelemetry-standard env var and falling back to the
/// project-specific one — identical precedence to
/// `pkg/telemetry.endpointFromEnv` on the Go side, so one `.env` setting
/// configures every service's exporter the same way.
fn endpoint_from_env() -> Option<String> {
    std::env::var("OTEL_EXPORTER_OTLP_ENDPOINT")
        .ok()
        .filter(|v| !v.is_empty())
        .or_else(|| {
            std::env::var("RATION_OTEL_EXPORTER_ENDPOINT")
                .ok()
                .filter(|v| !v.is_empty())
        })
}
