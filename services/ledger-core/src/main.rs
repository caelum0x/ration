use std::net::{SocketAddr, ToSocketAddrs};
use std::path::PathBuf;
use std::sync::Arc;

use ledger_core::grpc::LedgerCoreService;
use ledger_core::ledger::Ledger;
use ledger_core::pb::ration::ledger::v1::ledger_core_server::LedgerCoreServer;

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
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
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".into()),
        )
        .init();

    let wal_path = PathBuf::from(env_or("RATION_LEDGER_WAL_PATH", "./data/ledger.wal"));
    let db_path = PathBuf::from(env_or("RATION_LEDGER_DB_PATH", "./data/ledger.db"));
    // LEDGER_CORE_ADDR is the single env var every service agrees on (see
    // .env.example, services/gateway/internal/config, services/agent,
    // services/cli), all dialing it as the same "host:port" string.
    let ledger_addr = env_or("LEDGER_CORE_ADDR", "localhost:7001");
    let addr: SocketAddr = resolve_bind_addr(&ledger_addr)?;

    tracing::info!(?wal_path, ?db_path, "opening ledger (recovering WAL before accepting RPCs)");
    let (ledger, replayed) = Ledger::open(&wal_path, &db_path)?;
    tracing::info!(replayed_events = replayed, "WAL recovery complete");

    let service = LedgerCoreService::new(Arc::new(ledger));

    tracing::info!(%addr, "LedgerCore gRPC server listening");
    tonic::transport::Server::builder()
        .add_service(LedgerCoreServer::new(service))
        .serve(addr)
        .await?;

    Ok(())
}
