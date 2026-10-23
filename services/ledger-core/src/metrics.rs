//! Prometheus metrics for ledger-core — the Rust counterpart of
//! `pkg/metrics` (Go), scoped to exactly the TODO
//! `docs/THREAT-MODEL.md`'s "Known gaps" #3 names: "a Prometheus
//! counter ... incremented on every `spawn_append` failure in
//! `services/ledger-core/src/audit_mirror.rs`, alerted on past a small
//! non-zero threshold."
//!
//! This is ledger-core's *first* Prometheus metric, not a full port of
//! `pkg/metrics`'s per-RPC request/latency instrumentation
//! (`ledger_core_grpc_server_requests_total` /
//! `..._request_duration_seconds`) — that is a separate, larger,
//! already-scoped TODO (`docs/SLO.md`'s "ledger-core" section,
//! "Tracing landed, metrics did not") deliberately left for its own
//! reviewable unit, same reasoning `src/telemetry.rs`'s doc comment
//! gives for why tracing and metrics landed as separate passes.
//!
//! Like `pkg/metrics`, each process owns its own [`Registry`] (not the
//! `prometheus` crate's process-global default registry) so tests can
//! construct a throwaway [`Metrics`] without colliding with another
//! test's counters.

use std::net::SocketAddr;
use std::sync::Arc;

use prometheus::{CounterVec, Encoder, Opts, Registry, TextEncoder};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// `namespace` prefixes every metric this module registers, matching
/// `pkg/metrics`'s `ration_*` convention but naming the service
/// explicitly (`pkg/metrics` instead carries a `service` label; a
/// single-metric Rust registry has no need for that label, since
/// nothing else is multiplexed onto it today).
const METRIC_NAME: &str = "ledger_core_audit_mirror_spawn_append_failures_total";

/// Bundles ledger-core's Prometheus registry and metric instruments.
/// Construct one with [`Metrics::new`] in `main.rs`, keep it alive for
/// the process lifetime, and share it (via `Arc`) between
/// [`AuditMirror`](crate::audit_mirror::AuditMirror) (which records
/// into it) and [`serve`] (which renders it on every scrape).
pub struct Metrics {
    registry: Registry,

    /// `ledger_core_audit_mirror_spawn_append_failures_total{kind}` —
    /// incremented every time `AuditMirror::spawn_append`'s detached
    /// task fails to reach or write to `audit-log`
    /// (`services/ledger-core/src/audit_mirror.rs`). `kind` is
    /// `"ledger_entry"` or `"agent_decision"`
    /// (`pb::SourceKind::as_str_name()`, lowercased to match
    /// Prometheus label-value convention), so a sustained-outage spike
    /// in one kind doesn't hide in an aggregate across both.
    audit_mirror_spawn_append_failures_total: CounterVec,
}

impl Metrics {
    /// Builds a fresh registry with every metric this module owns
    /// already registered. Registration only fails on a duplicate
    /// metric name within the same registry, which cannot happen here
    /// (one `Metrics` = one fresh `Registry`) — `expect` is correct,
    /// not a silently-swallowed error.
    pub fn new() -> Self {
        let registry = Registry::new();

        let audit_mirror_spawn_append_failures_total = CounterVec::new(
            Opts::new(
                METRIC_NAME,
                "Count of AuditMirror::spawn_append calls that failed to reach or write to \
                 audit-log, by event kind. ledger-core's own WAL remains the durable record \
                 regardless; a sustained non-zero rate here means the independent audit-log \
                 mirror (docs/THREAT-MODEL.md) is silently falling behind.",
            ),
            &["kind"],
        )
        .expect("static metric name/help/labels are always valid");
        registry
            .register(Box::new(audit_mirror_spawn_append_failures_total.clone()))
            .expect("fresh registry never has a name collision");

        // Pre-touch every known `kind` label value at zero rather than
        // leaving them unobserved. The `prometheus` crate's CounterVec
        // (unlike Go client_golang) omits a label combination from
        // `gather()` entirely until it's first incremented, so an
        // alert rule watching for "no failures reported" can't tell
        // "zero failures" from "this binary predates the metric" —
        // pre-touching closes that gap from process start.
        for kind in ["ledger_entry", "agent_decision", "unspecified"] {
            audit_mirror_spawn_append_failures_total
                .with_label_values(&[kind])
                .reset();
        }

        Self {
            registry,
            audit_mirror_spawn_append_failures_total,
        }
    }

    /// Records one failed `spawn_append` call for the given event
    /// `kind` (`"ledger_entry"` or `"agent_decision"`).
    pub fn record_audit_mirror_spawn_append_failure(&self, kind: &str) {
        self.audit_mirror_spawn_append_failures_total
            .with_label_values(&[kind])
            .inc();
    }

    /// Renders every registered metric in Prometheus's text exposition
    /// format — what `GET /metrics` returns. `pub(crate)` rather than
    /// private: `audit_mirror`'s tests assert on rendered output to
    /// verify the counter this module exists for actually increments,
    /// without needing a real `/metrics` HTTP round trip per test.
    pub(crate) fn render(&self) -> Result<String, prometheus::Error> {
        let encoder = TextEncoder::new();
        let metric_families = self.registry.gather();
        let mut buf = Vec::new();
        encoder.encode(&metric_families, &mut buf)?;
        Ok(String::from_utf8(buf).expect("Prometheus text encoding is always valid UTF-8"))
    }
}

impl Default for Metrics {
    fn default() -> Self {
        Self::new()
    }
}

/// Serves `GET /metrics` on `addr` until the process exits. Deliberately
/// hand-rolled over raw `tokio::net::TcpListener` rather than pulling in
/// `hyper`/`axum` as a direct dependency: Prometheus's own scraper only
/// ever sends a bare `GET /metrics HTTP/1.1` request with no body, so a
/// minimal line-oriented HTTP/1.1 responder is sufficient and keeps this
/// additive instrumentation from growing this crate's dependency surface
/// (`services/gateway` etc. already use the full `net/http` stack on the
/// Go side; a second HTTP framework in the one Rust binary isn't worth it
/// for a single read-only endpoint).
///
/// Like every other optional subsystem in this crate (OTLP export,
/// audit-log mirroring), a bind failure here is logged and returned to
/// the caller rather than panicking the whole process — `main.rs` treats
/// it as non-fatal (see its own doc comment on why metrics must never
/// become a new way for the ledger to go down).
pub async fn serve(metrics: Arc<Metrics>, addr: SocketAddr) -> std::io::Result<()> {
    let listener = TcpListener::bind(addr).await?;
    loop {
        let (mut socket, _peer) = match listener.accept().await {
            Ok(pair) => pair,
            Err(err) => {
                tracing::warn!(error = %err, "metrics: accept failed, continuing to serve");
                continue;
            }
        };
        let metrics = Arc::clone(&metrics);
        tokio::spawn(async move {
            // Draining the request is best-effort and bounded: this
            // endpoint only ever needs to notice *that* a request
            // arrived, not parse it, since it serves exactly one
            // resource unconditionally. A slow/partial client write
            // (or a non-HTTP probe) must never hang this task forever,
            // so a read that returns 0 bytes or errors just falls
            // through to writing the response anyway.
            let mut buf = [0u8; 1024];
            let _ = socket.read(&mut buf).await;

            let body = match metrics.render() {
                Ok(body) => body,
                Err(err) => {
                    tracing::warn!(error = %err, "metrics: failed to render /metrics response");
                    format!("# render error: {err}\n")
                }
            };
            let response = format!(
                "HTTP/1.1 200 OK\r\n\
                 Content-Type: text/plain; version=0.0.4\r\n\
                 Content-Length: {}\r\n\
                 Connection: close\r\n\
                 \r\n\
                 {}",
                body.len(),
                body
            );
            if let Err(err) = socket.write_all(response.as_bytes()).await {
                tracing::warn!(error = %err, "metrics: failed to write /metrics response");
            }
            let _ = socket.shutdown().await;
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn new_registers_the_audit_mirror_counter_at_zero() {
        let metrics = Metrics::new();
        let rendered = metrics.render().expect("render should succeed");
        // `Metrics::new` pre-touches every known `kind` label value,
        // so both the metric's TYPE line and an explicit zero sample
        // per kind are present from construction — before anything
        // increments it.
        assert!(
            rendered.contains(&format!("# TYPE {METRIC_NAME} counter")),
            "expected the audit-mirror counter to be registered, got:\n{rendered}"
        );
        assert!(
            rendered.contains(&format!("{METRIC_NAME}{{kind=\"ledger_entry\"}} 0")),
            "expected an explicit ledger_entry=0 sample, got:\n{rendered}"
        );
        assert!(
            rendered.contains(&format!("{METRIC_NAME}{{kind=\"agent_decision\"}} 0")),
            "expected an explicit agent_decision=0 sample, got:\n{rendered}"
        );
    }

    #[test]
    fn record_audit_mirror_spawn_append_failure_increments_by_kind() {
        let metrics = Metrics::new();

        metrics.record_audit_mirror_spawn_append_failure("ledger_entry");
        metrics.record_audit_mirror_spawn_append_failure("ledger_entry");
        metrics.record_audit_mirror_spawn_append_failure("agent_decision");

        let rendered = metrics.render().expect("render should succeed");
        assert!(
            rendered.contains(&format!("{METRIC_NAME}{{kind=\"ledger_entry\"}} 2")),
            "expected ledger_entry=2 in rendered metrics, got:\n{rendered}"
        );
        assert!(
            rendered.contains(&format!("{METRIC_NAME}{{kind=\"agent_decision\"}} 1")),
            "expected agent_decision=1 in rendered metrics, got:\n{rendered}"
        );
    }

    #[tokio::test]
    async fn serve_responds_to_a_bare_get_with_the_rendered_metrics() {
        let metrics = Arc::new(Metrics::new());
        metrics.record_audit_mirror_spawn_append_failure("ledger_entry");

        // Reserve an ephemeral port via a throwaway std listener, then
        // drop it immediately so serve() below can rebind the same
        // address — simpler than threading tokio's bound listener
        // through serve()'s signature just for this one test.
        let addr = {
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            listener.local_addr().unwrap()
        };

        let serve_metrics = Arc::clone(&metrics);
        let handle = tokio::spawn(async move {
            let _ = serve(serve_metrics, addr).await;
        });

        // Give the server a moment to bind before connecting.
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;

        use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
        let mut stream = tokio::net::TcpStream::connect(addr)
            .await
            .expect("should connect to /metrics listener");
        stream
            .write_all(b"GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
            .await
            .unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).await.unwrap();

        assert!(response.starts_with("HTTP/1.1 200 OK"));
        assert!(response.contains(&format!("{METRIC_NAME}{{kind=\"ledger_entry\"}} 1")));

        handle.abort();
    }
}
