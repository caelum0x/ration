//! Best-effort, asynchronous mirror of every `ApplyEntry` and
//! `RecordAgentDecision` outcome into the `audit-log` service (see
//! `docs/SCALE-EXPANSION.md` section 8, `services/audit-log`,
//! `proto/audit.proto`).
//!
//! `audit-log` is a deliberately *separate trust boundary* from
//! ledger-core's own WAL (`crate::wal_ffi`, `crate::ledger`): it is a
//! different process, with its own append-only on-disk log, that
//! ledger-core (or whatever has compromised it) can write to but never
//! edit or delete from. The point of this module is to make that
//! second, independent copy happen — without ever being able to slow
//! down or fail the original `ApplyEntry`/`RecordAgentDecision` RPC it
//! is mirroring on behalf of. Every public method here is
//! fire-and-forget (it spawns a detached task and returns
//! immediately); every failure to reach or write to audit-log is
//! logged and dropped, never propagated to the caller. ledger-core's
//! own WAL remains the durable, authoritative record regardless of
//! whether the mirror succeeds.

use std::sync::Arc;

use tonic::transport::{Channel, Endpoint};

use crate::agent_decision::AgentDecisionRecord;
use crate::ledger::LedgerEntryRecord;
use crate::metrics::Metrics;
use crate::mtls;
use crate::pb::ration::audit::v1 as pb;
use pb::audit_log_client::AuditLogClient;

/// Identifies this process to audit-log in every record it mirrors
/// (`AppendRequest.source_service`).
const SOURCE_SERVICE: &str = "ledger-core";

/// The identity name `infra/dev-ca/generate-dev-ca.sh` issues
/// audit-log's certificate for — the default a client verifies
/// audit-log's certificate against unless
/// `LEDGER_CORE_MTLS_SERVER_NAME` overrides it (see
/// `mtls::Config::client_tls_config`).
const AUDIT_LOG_SERVER_NAME: &str = "audit-log";

/// A handle that mirrors ledger entries and agent decisions into
/// audit-log. Cheap to clone (wraps a lazily-connecting tonic
/// `Channel`, itself cheaply cloneable) so every gRPC handler that
/// needs to mirror something can hold its own clone.
///
/// `client` is `None` when mirroring is disabled (`AUDIT_LOG_ADDR`
/// unset — see `disabled`/`main.rs`): every mirror call then becomes a
/// true no-op (not even a serialization attempt), rather than dialing
/// a default address nothing may be listening on.
#[derive(Clone)]
pub struct AuditMirror {
    client: Option<AuditLogClient<Channel>>,
    /// Recorded into on every failed `spawn_append` call (see
    /// `Metrics::record_audit_mirror_spawn_append_failure`,
    /// `docs/THREAT-MODEL.md`'s "Known gaps" #3). Always present, even
    /// on a [`disabled`](Self::disabled) mirror — `spawn_append` never
    /// runs there, so it's simply never incremented, not a reason to
    /// make this field optional too.
    metrics: Arc<Metrics>,
}

impl AuditMirror {
    /// A mirror with audit-log mirroring turned off: every
    /// `mirror_*` call on it is an immediate no-op. Used when
    /// `AUDIT_LOG_ADDR` is unset, so ledger-core can run standalone
    /// (tests, a laptop demo without the full stack) without ever
    /// trying to dial an address nothing is listening on.
    pub fn disabled(metrics: Arc<Metrics>) -> Self {
        Self {
            client: None,
            metrics,
        }
    }

    /// Builds a mirror dialing `addr` (a "host:port" string from
    /// `AUDIT_LOG_ADDR`, e.g. `localhost:7003`) with `mtls_cfg`'s
    /// client settings (reusing ledger-core's own `LEDGER_CORE_MTLS_*`
    /// identity — the same cert doubles as this process's client cert
    /// when it dials out, exactly as `infra/dev-ca/` issues it).
    ///
    /// The connection itself is lazy (`Endpoint::connect_lazy`): no
    /// network I/O happens here, so a ledger-core boot never blocks on
    /// — or fails because of — audit-log being slow or down. The
    /// first mirrored call is what actually dials; if that fails, the
    /// error is logged and dropped inside `spawn_append`, never
    /// surfaced here or to whatever RPC triggered the mirror.
    pub fn connect(
        addr: &str,
        mtls_cfg: &mtls::Config,
        metrics: Arc<Metrics>,
    ) -> Result<Self, mtls::Error> {
        let scheme = mtls_cfg.dial_scheme();
        let mut endpoint = Endpoint::from_shared(format!("{scheme}://{addr}"))?;
        if let Some(tls) = mtls_cfg.client_tls_config(AUDIT_LOG_SERVER_NAME)? {
            endpoint = endpoint.tls_config(tls)?;
        }
        let channel = endpoint.connect_lazy();
        Ok(Self {
            client: Some(AuditLogClient::new(channel)),
            metrics,
        })
    }

    /// Mirrors a successfully-applied ledger entry. Never call this
    /// for `ApplyOutcome::ReserveFloorViolated` — there is no entry to
    /// mirror, and the reserve-floor rejection itself isn't a
    /// money-moving event audit-log's threat model (see
    /// `docs/THREAT-MODEL.md`) is about.
    pub fn mirror_ledger_entry(&self, tenant_id: &str, entry: &LedgerEntryRecord) {
        if self.client.is_none() {
            return;
        }
        let payload_json = match serde_json::to_string(entry) {
            Ok(json) => json,
            Err(err) => {
                // A serialization failure here is a bug in this
                // process, not a reason to fail the ApplyEntry call it
                // would otherwise be mirroring for — log loudly and
                // skip the mirror, same posture as a network failure.
                tracing::warn!(
                    error = %err,
                    entry_id = %entry.id,
                    "audit_mirror: failed to serialize ledger entry, skipping mirror"
                );
                return;
            }
        };
        self.spawn_append(
            pb::SourceKind::LedgerEntry,
            tenant_id.to_string(),
            entry.id.clone(),
            payload_json,
            entry.created_at_unix,
        );
    }

    /// Mirrors a recorded agent decision.
    pub fn mirror_agent_decision(&self, decision: &AgentDecisionRecord) {
        if self.client.is_none() {
            return;
        }
        let payload_json = match serde_json::to_string(decision) {
            Ok(json) => json,
            Err(err) => {
                tracing::warn!(
                    error = %err,
                    decision_id = %decision.id,
                    "audit_mirror: failed to serialize agent decision, skipping mirror"
                );
                return;
            }
        };
        self.spawn_append(
            pb::SourceKind::AgentDecision,
            decision.tenant_id.clone(),
            decision.id.clone(),
            payload_json,
            decision.created_at_unix,
        );
    }

    /// Spawns the detached task that actually calls audit-log's
    /// `Append` RPC. `ref_id` is the id ledger-core already assigned
    /// (the `LedgerEntry.id`/`AgentDecision.id`), not a new one minted
    /// here — audit-log's own `Append` is idempotent on
    /// `(source_service, kind, ref_id)` (see `proto/audit.proto`), so
    /// a retried mirror of the same event is always safe.
    fn spawn_append(
        &self,
        kind: pb::SourceKind,
        tenant_id: String,
        ref_id: String,
        payload_json: String,
        occurred_at_unix: i64,
    ) {
        // Callers (mirror_ledger_entry/mirror_agent_decision) already
        // return early when mirroring is disabled, but checking again
        // here keeps this method safe to call on its own.
        let Some(mut client) = self.client.clone() else {
            return;
        };
        let metrics = Arc::clone(&self.metrics);
        tokio::spawn(async move {
            let req = pb::AppendRequest {
                source_service: SOURCE_SERVICE.to_string(),
                kind: kind as i32,
                tenant_id,
                ref_id: ref_id.clone(),
                payload_json,
                occurred_at_unix,
            };
            if let Err(status) = client.append(req).await {
                tracing::warn!(
                    error = %status,
                    ref_id = %ref_id,
                    kind = kind.as_str_name(),
                    "audit_mirror: best-effort append to audit-log failed; \
                     ledger-core's own WAL remains the durable record"
                );
                metrics.record_audit_mirror_spawn_append_failure(metric_label(kind));
            }
        });
    }
}

/// Maps a `SourceKind` to the label value
/// `Metrics::record_audit_mirror_spawn_append_failure` uses — a short,
/// lowercase, snake_case form rather than `kind.as_str_name()`'s raw
/// proto enum name (`SOURCE_KIND_LEDGER_ENTRY`), matching Prometheus
/// label-value convention elsewhere in this project.
fn metric_label(kind: pb::SourceKind) -> &'static str {
    match kind {
        pb::SourceKind::LedgerEntry => "ledger_entry",
        pb::SourceKind::AgentDecision => "agent_decision",
        pb::SourceKind::Unspecified => "unspecified",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent_decision::AgentAction;
    use std::net::SocketAddr;
    use tokio::sync::mpsc;
    use tonic::{Request, Response, Status};

    /// A fake AuditLog server that records every Append it receives on
    /// a channel, so tests can assert on exactly what AuditMirror sent
    /// without needing the real audit-log binary.
    struct FakeAuditLog {
        tx: mpsc::UnboundedSender<pb::AppendRequest>,
    }

    #[tonic::async_trait]
    impl pb::audit_log_server::AuditLog for FakeAuditLog {
        async fn append(
            &self,
            request: Request<pb::AppendRequest>,
        ) -> Result<Response<pb::AppendResponse>, Status> {
            let req = request.into_inner();
            let _ = self.tx.send(req.clone());
            Ok(Response::new(pb::AppendResponse {
                record: Some(pb::AuditRecord {
                    sequence: 1,
                    source_service: req.source_service,
                    kind: req.kind,
                    tenant_id: req.tenant_id,
                    ref_id: req.ref_id,
                    payload_json: req.payload_json,
                    occurred_at_unix: req.occurred_at_unix,
                    recorded_at_unix: 0,
                    prev_hash: String::new(),
                    hash: String::new(),
                }),
            }))
        }

        async fn list_records(
            &self,
            _request: Request<pb::ListRecordsRequest>,
        ) -> Result<Response<pb::ListRecordsResponse>, Status> {
            Err(Status::unimplemented("not needed by this fake"))
        }

        async fn verify_chain(
            &self,
            _request: Request<pb::VerifyChainRequest>,
        ) -> Result<Response<pb::VerifyChainResponse>, Status> {
            Err(Status::unimplemented("not needed by this fake"))
        }
    }

    /// Starts the fake AuditLog server in the background on loopback,
    /// returning an AuditMirror already connected to it (plaintext —
    /// this suite only exercises the mirroring logic, mTLS is covered
    /// by `crate::mtls`'s own test suite) and the receiver side of
    /// every Append it gets.
    async fn start_fake_audit_log() -> (AuditMirror, mpsc::UnboundedReceiver<pb::AppendRequest>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr: SocketAddr = listener.local_addr().unwrap();

        tokio::spawn(async move {
            tonic::transport::Server::builder()
                .add_service(pb::audit_log_server::AuditLogServer::new(FakeAuditLog {
                    tx,
                }))
                .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
                .await
                .unwrap();
        });

        let mirror = AuditMirror::connect(
            &addr.to_string(),
            &mtls::Config::default(),
            Arc::new(Metrics::new()),
        )
        .expect("connect");
        (mirror, rx)
    }

    #[tokio::test]
    async fn mirror_ledger_entry_sends_the_full_record_as_payload() {
        let (mirror, mut rx) = start_fake_audit_log().await;

        let entry = LedgerEntryRecord {
            id: "entry-1".to_string(),
            tenant_id: "tenant-1".to_string(),
            entry_type: crate::ledger::LedgerEntryType::Debit,
            amount_minor_units: 500,
            currency: "USD".to_string(),
            balance_after_minor_units: 1500,
            reference: "ref-1".to_string(),
            memo: "memo".to_string(),
            idempotency_key: "idem-1".to_string(),
            created_at_unix: 1_700_000_000,
        };

        mirror.mirror_ledger_entry("tenant-1", &entry);

        let got = tokio::time::timeout(std::time::Duration::from_secs(5), rx.recv())
            .await
            .expect("fake audit-log should receive the mirrored append in time")
            .expect("channel should not be closed");

        assert_eq!(got.source_service, "ledger-core");
        assert_eq!(got.kind, pb::SourceKind::LedgerEntry as i32);
        assert_eq!(got.tenant_id, "tenant-1");
        assert_eq!(got.ref_id, "entry-1");
        assert_eq!(got.occurred_at_unix, 1_700_000_000);
        let round_tripped: LedgerEntryRecord =
            serde_json::from_str(&got.payload_json).expect("payload_json should deserialize");
        assert_eq!(round_tripped, entry);
    }

    #[tokio::test]
    async fn mirror_agent_decision_sends_the_full_record_as_payload() {
        let (mirror, mut rx) = start_fake_audit_log().await;

        let decision = AgentDecisionRecord {
            id: "decision-1".to_string(),
            tenant_id: "tenant-2".to_string(),
            trigger: "late-deposit".to_string(),
            action: AgentAction::RationBridge,
            rationale: "bridging 20% of the shortfall".to_string(),
            policy_snapshot_json: "{}".to_string(),
            created_at_unix: 1_700_000_100,
        };

        mirror.mirror_agent_decision(&decision);

        let got = tokio::time::timeout(std::time::Duration::from_secs(5), rx.recv())
            .await
            .expect("fake audit-log should receive the mirrored append in time")
            .expect("channel should not be closed");

        assert_eq!(got.source_service, "ledger-core");
        assert_eq!(got.kind, pb::SourceKind::AgentDecision as i32);
        assert_eq!(got.tenant_id, "tenant-2");
        assert_eq!(got.ref_id, "decision-1");
        let round_tripped: AgentDecisionRecord =
            serde_json::from_str(&got.payload_json).expect("payload_json should deserialize");
        assert_eq!(round_tripped, decision);
    }

    #[tokio::test]
    async fn mirror_is_fire_and_forget_when_audit_log_is_unreachable() {
        // Dial an address nothing is listening on. mirror_ledger_entry
        // must not panic, block indefinitely, or otherwise affect the
        // caller — the whole point of "best-effort" is that this is
        // silently survivable.
        let unreachable_addr = "127.0.0.1:1"; // port 1 is never a gRPC server in CI/dev
        let metrics = Arc::new(Metrics::new());
        let mirror = AuditMirror::connect(
            unreachable_addr,
            &mtls::Config::default(),
            Arc::clone(&metrics),
        )
        .expect("connect (lazy — no I/O yet)");

        let entry = LedgerEntryRecord {
            id: "entry-unreachable".to_string(),
            tenant_id: "tenant-1".to_string(),
            entry_type: crate::ledger::LedgerEntryType::Credit,
            amount_minor_units: 100,
            currency: "USD".to_string(),
            balance_after_minor_units: 100,
            reference: String::new(),
            memo: String::new(),
            idempotency_key: "idem-unreachable".to_string(),
            created_at_unix: 0,
        };

        // Should return immediately; the actual failed dial happens in
        // the detached task and is only ever logged.
        mirror.mirror_ledger_entry("tenant-1", &entry);

        // The failed dial happens asynchronously in the detached task
        // spawn_append starts — poll briefly for the counter this
        // test's whole point is verifying (docs/THREAT-MODEL.md's
        // "Known gaps" #3) rather than racing a fixed sleep.
        let incremented = tokio::time::timeout(std::time::Duration::from_secs(5), async {
            loop {
                let rendered = metrics.render().expect("render should succeed");
                if rendered.contains(
                    "ledger_core_audit_mirror_spawn_append_failures_total{kind=\"ledger_entry\"} 1",
                ) {
                    return;
                }
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
        })
        .await;
        assert!(
            incremented.is_ok(),
            "expected the audit-mirror failure counter to increment after an unreachable dial"
        );
    }

    #[tokio::test]
    async fn disabled_mirror_is_a_true_no_op() {
        // disabled() is what main.rs constructs when AUDIT_LOG_ADDR is
        // unset — no Channel, no Endpoint, nothing to dial. Both
        // mirror_* calls must still be safe to call unconditionally
        // from grpc.rs regardless of whether mirroring is configured.
        let mirror = AuditMirror::disabled(Arc::new(Metrics::new()));

        let entry = LedgerEntryRecord {
            id: "entry-disabled".to_string(),
            tenant_id: "tenant-1".to_string(),
            entry_type: crate::ledger::LedgerEntryType::Credit,
            amount_minor_units: 100,
            currency: "USD".to_string(),
            balance_after_minor_units: 100,
            reference: String::new(),
            memo: String::new(),
            idempotency_key: "idem-disabled".to_string(),
            created_at_unix: 0,
        };
        mirror.mirror_ledger_entry("tenant-1", &entry);

        let decision = AgentDecisionRecord {
            id: "decision-disabled".to_string(),
            tenant_id: "tenant-1".to_string(),
            trigger: "test".to_string(),
            action: AgentAction::Notify,
            rationale: String::new(),
            policy_snapshot_json: "{}".to_string(),
            created_at_unix: 0,
        };
        mirror.mirror_agent_decision(&decision);
    }
}
