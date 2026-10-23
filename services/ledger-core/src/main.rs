use std::net::{SocketAddr, ToSocketAddrs};
use std::path::PathBuf;
use std::sync::Arc;

use ledger_core::audit_mirror::AuditMirror;
use ledger_core::authclient::AuthVerifier;
use ledger_core::grpc::LedgerCoreService;
use ledger_core::ledger::Ledger;
use ledger_core::metrics::Metrics;
use ledger_core::mtls;
use ledger_core::pb::ration::ledger::v1::ledger_core_server::LedgerCoreServer;
use ledger_core::telemetry::TracedService;
use ledger_core::tenantauth;

/// Tags every span, trace resource attribute, and log line this process
/// emits — mirrors the Go services' `serviceName`/`serviceVersion`
/// constants (see e.g. services/gateway/cmd/gateway/main.go).
const SERVICE_NAME: &str = "ledger-core";
const SERVICE_VERSION: &str = "0.1.0";

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

/// Parses an env var as a bool ("true"/"false", case-insensitive),
/// falling back to `default` when unset. Mirrors
/// `services/gateway/internal/config`'s `getenvBoolDefault` exactly —
/// same accepted values, same default-on-unset behavior — so
/// `.env.example`'s documented values work identically on both sides
/// of `RATION_LEDGER_REQUIRE_TENANT_AUTH`/
/// `RATION_GATEWAY_REQUIRE_TENANT_AUTH`.
fn env_bool_or(key: &str, default: bool) -> Result<bool, Box<dyn std::error::Error>> {
    match std::env::var(key) {
        Err(_) => Ok(default),
        Ok(v) => match v.to_lowercase().as_str() {
            "true" => Ok(true),
            "false" => Ok(false),
            other => Err(format!("{key} must be \"true\" or \"false\", got {other:?}").into()),
        },
    }
}

/// Turns a configured "host:port" string (e.g. "localhost:7001", the
/// .env.example default) into a concrete bind address by resolving the
/// host through the system resolver — `SocketAddr::parse` alone rejects
/// "localhost:7001" because "localhost" isn't an IP literal. Takes the
/// first resolved address (loopback for "localhost"), which is sufficient
/// since every client in this project dials the same configured string.
fn resolve_bind_addr(addr: &str) -> Result<SocketAddr, Box<dyn std::error::Error>> {
    addr.to_socket_addrs()?
        .next()
        .ok_or_else(|| format!("LEDGER_CORE_ADDR {addr:?} did not resolve to any address").into())
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Kept alive for the whole process so the final batch of spans is
    // flushed on exit rather than dropped — see
    // ledger_core::telemetry::TelemetryGuard's doc comment.
    let _telemetry = ledger_core::telemetry::init(SERVICE_NAME, SERVICE_VERSION)?;

    let wal_path = PathBuf::from(env_or("RATION_LEDGER_WAL_PATH", "./data/ledger.wal"));
    let db_path = PathBuf::from(env_or("RATION_LEDGER_DB_PATH", "./data/ledger.db"));
    // LEDGER_CORE_ADDR is the single env var every service agrees on (see
    // .env.example, services/gateway/internal/config, services/agent,
    // services/cli), all dialing it as the same "host:port" string.
    let ledger_addr = env_or("LEDGER_CORE_ADDR", "localhost:7001");
    let addr: SocketAddr = resolve_bind_addr(&ledger_addr)?;

    tracing::info!(
        ?wal_path,
        ?db_path,
        "opening ledger (recovering WAL before accepting RPCs)"
    );
    let (ledger, replayed) = Ledger::open(&wal_path, &db_path)?;
    tracing::info!(replayed_events = replayed, "WAL recovery complete");

    // mTLS (see docs/SCALE-EXPANSION.md section 8, ledger_core::mtls,
    // infra/dev-ca/) is opt-in and disabled by default: with no
    // LEDGER_CORE_MTLS_* env vars set, mtls_cfg.enabled is false and
    // every method below returns None / plaintext, so a bare `cargo
    // run` keeps working with zero certs on disk. The same identity
    // doubles as this process's client cert when it dials audit-log
    // below, matching how infra/dev-ca/generate-dev-ca.sh issues one
    // cert per service usable as both.
    let mtls_cfg = mtls::Config::load_from_env("LEDGER_CORE")?;
    tracing::info!(mtls_enabled = mtls_cfg.enabled, "mTLS config loaded");

    // audit-log mirroring (see docs/SCALE-EXPANSION.md section 8,
    // services/audit-log, ledger_core::audit_mirror) is best-effort and
    // connects lazily: a down/unreachable audit-log never blocks or
    // fails ledger-core's own startup or RPCs. AUDIT_LOG_ADDR has no
    // default (matching .env.example/docker-compose.yml's convention
    // for this var, set on every service that mirrors into audit-log)
    // — leaving it unset disables mirroring entirely rather than
    // dialing a default address nothing may be listening on.
    // Prometheus metrics (docs/THREAT-MODEL.md's "Known gaps" #3,
    // docs/SLO.md's "ledger-core" section): ledger-core's first
    // /metrics endpoint, scoped to the audit-mirror spawn_append
    // failure counter that gap names precisely. Binding failures are
    // logged and non-fatal — like OTLP export and audit-log mirroring,
    // observability must never become a new way for the ledger to go
    // down.
    let metrics = Arc::new(Metrics::new());
    let metrics_addr_str = env_or("LEDGER_CORE_METRICS_ADDR", "localhost:9099");
    match resolve_bind_addr(&metrics_addr_str) {
        Ok(metrics_addr) => {
            let metrics_for_server = Arc::clone(&metrics);
            tokio::spawn(async move {
                if let Err(err) =
                    ledger_core::metrics::serve(metrics_for_server, metrics_addr).await
                {
                    tracing::warn!(error = %err, %metrics_addr, "metrics: /metrics server exited");
                }
            });
            tracing::info!(%metrics_addr, "ledger-core /metrics listening");
        }
        Err(err) => {
            tracing::warn!(
                error = %err,
                %metrics_addr_str,
                "metrics: failed to resolve LEDGER_CORE_METRICS_ADDR, /metrics disabled"
            );
        }
    }

    let audit_log_addr = env_or("AUDIT_LOG_ADDR", "");
    let audit_mirror = if audit_log_addr.is_empty() {
        tracing::info!("AUDIT_LOG_ADDR is unset; audit-log mirroring disabled");
        AuditMirror::disabled(Arc::clone(&metrics))
    } else {
        tracing::info!(%audit_log_addr, "audit-log mirror configured");
        AuditMirror::connect(&audit_log_addr, &mtls_cfg, Arc::clone(&metrics))?
    };

    // Tenant-scoped authorization directly on LedgerCore's own gRPC
    // surface (docs/THREAT-MODEL.md's "Known gaps" item 1's "the
    // biggest one" sub-gap, ledger_core::tenantauth): defaults to
    // true, the same posture services/gateway's own
    // RATION_GATEWAY_REQUIRE_TENANT_AUTH now defaults to. AUTH_GRPC_ADDR
    // is the same env var every Go service already dials auth at (see
    // .env.example).
    let auth_grpc_addr = env_or("AUTH_GRPC_ADDR", "localhost:7010");
    let require_tenant_auth = env_bool_or("RATION_LEDGER_REQUIRE_TENANT_AUTH", true)?;
    let auth_verifier = if auth_grpc_addr.is_empty() {
        AuthVerifier::disabled()
    } else {
        AuthVerifier::connect(&auth_grpc_addr, &mtls_cfg)?
    };
    if require_tenant_auth && !auth_verifier.is_configured() {
        // Fail closed, loudly, at startup — never silently start up
        // with every tenant-scoped RPC unauthenticated-but-accepted
        // because AUTH_GRPC_ADDR was blank. Set
        // RATION_LEDGER_REQUIRE_TENANT_AUTH=false explicitly for a
        // deployment that genuinely wants ledger-core to run standalone
        // with no Auth to check against.
        return Err(
            "RATION_LEDGER_REQUIRE_TENANT_AUTH=true but AUTH_GRPC_ADDR is unset; \
             set AUTH_GRPC_ADDR or explicitly set RATION_LEDGER_REQUIRE_TENANT_AUTH=false"
                .into(),
        );
    }
    tracing::info!(
        %auth_grpc_addr,
        require_tenant_auth,
        "ledger-core tenant authorization configured"
    );
    let tenantauth_cfg = tenantauth::Config {
        verifier: auth_verifier,
        required: require_tenant_auth,
    };

    let service = LedgerCoreService::new(Arc::new(ledger), audit_mirror, tenantauth_cfg);

    let mut server_builder = tonic::transport::Server::builder();
    if let Some(tls) = mtls_cfg.server_tls_config()? {
        server_builder = server_builder.tls_config(tls)?;
    }

    tracing::info!(%addr, "LedgerCore gRPC server listening");
    server_builder
        // TracedService::wrap extracts the W3C traceparent the Go
        // services' client interceptors (pkg/telemetry.DialOption)
        // inject into outgoing gRPC metadata, and starts a server span
        // as its child — the gRPC hop half of "gRPC client/server
        // interceptors propagating trace context across every service
        // hop" from docs/SCALE-EXPANSION.md section 2. If the caller
        // carries no trace context (e.g. a bare grpcurl probe), this
        // starts a fresh root span instead of failing. See
        // ledger_core::telemetry::TracedService's doc comment for why
        // this wraps the per-service type rather than using
        // Server::builder().layer(...) directly.
        .add_service(TracedService::wrap(LedgerCoreServer::new(service)))
        .serve(addr)
        .await?;

    Ok(())
}
