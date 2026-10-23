//! `LedgerCore` gRPC service implementation. Maps proto <-> domain types,
//! delegates every operation to [`crate::ledger::Ledger`], and maps
//! domain errors to `tonic::Status` via `LedgerError`'s `From` impl.

use std::sync::Arc;

use tonic::{Request, Response, Status};

use crate::agent_decision::AgentAction;
use crate::audit_mirror::AuditMirror;
use crate::deposit::DepositRecord;
use crate::ledger::{ApplyOutcome, BalanceRecord, Ledger, LedgerEntryRecord, LedgerEntryType};
use crate::money::Money;
use crate::pb::ration::common::v1 as common_pb;
use crate::pb::ration::ledger::v1 as pb;
use crate::tenant::TenantRecord;

pub struct LedgerCoreService {
    ledger: Arc<Ledger>,
    // Mirrors every successfully-applied ledger entry and recorded
    // agent decision into audit-log (see docs/SCALE-EXPANSION.md
    // section 8, crate::audit_mirror). Best-effort: never awaited
    // inline, never allowed to affect this service's own responses.
    audit: AuditMirror,
}

impl LedgerCoreService {
    pub fn new(ledger: Arc<Ledger>, audit: AuditMirror) -> Self {
        Self { ledger, audit }
    }
}

fn to_pb_money(minor_units: i64, currency: &str) -> common_pb::Money {
    common_pb::Money {
        minor_units,
        currency: currency.to_string(),
    }
}

// `tonic::Status` is inherently large (it carries a message, optional
// source error, and metadata map); boxing it here would just push the
// same size cost onto every call site's `?`-propagation into the
// handler's own `Result<Response<T>, Status>`. Allowing the lint locally
// is the standard tonic-ecosystem idiom for this exact shape.
#[allow(clippy::result_large_err)]
fn money_from_pb(m: Option<common_pb::Money>, field: &str) -> Result<Money, Status> {
    let m = m.ok_or_else(|| Status::invalid_argument(format!("{field} is required")))?;
    Money::non_negative(m.minor_units, m.currency)
        .map_err(|e| Status::invalid_argument(e.to_string()))
}

fn to_pb_tenant(t: &TenantRecord) -> pb::Tenant {
    pb::Tenant {
        id: t.id.clone(),
        name: t.name.clone(),
        currency: t.currency.clone(),
        reserve_floor_minor_units: t.reserve_floor_minor_units,
        created_at_unix: t.created_at_unix,
    }
}

fn to_pb_deposit(d: &DepositRecord) -> pb::Deposit {
    pb::Deposit {
        id: d.id.clone(),
        tenant_id: d.tenant_id.clone(),
        expected_amount: Some(to_pb_money(d.expected_minor_units, &d.currency)),
        expected_at_unix: d.expected_at_unix,
        received_at_unix: d.received_at_unix,
        status: d.status.as_i32(),
        external_ref: d.external_ref.clone(),
    }
}

fn to_pb_entry(e: &LedgerEntryRecord) -> pb::LedgerEntry {
    pb::LedgerEntry {
        id: e.id.clone(),
        tenant_id: e.tenant_id.clone(),
        r#type: e.entry_type.as_i32(),
        amount: Some(to_pb_money(e.amount_minor_units, &e.currency)),
        balance_after_minor_units: e.balance_after_minor_units,
        reference: e.reference.clone(),
        memo: e.memo.clone(),
        created_at_unix: e.created_at_unix,
    }
}

fn to_pb_balance(tenant_id: &str, b: &BalanceRecord, currency: &str) -> pb::Balance {
    pb::Balance {
        tenant_id: tenant_id.to_string(),
        available_minor_units: b.available_minor_units,
        held_minor_units: b.held_minor_units,
        currency: currency.to_string(),
    }
}

fn to_pb_agent_decision(d: &crate::agent_decision::AgentDecisionRecord) -> pb::AgentDecision {
    pb::AgentDecision {
        id: d.id.clone(),
        tenant_id: d.tenant_id.clone(),
        trigger: d.trigger.clone(),
        action: d.action.as_i32(),
        rationale: d.rationale.clone(),
        policy_snapshot_json: d.policy_snapshot_json.clone(),
        created_at_unix: d.created_at_unix,
    }
}

#[tonic::async_trait]
impl pb::ledger_core_server::LedgerCore for LedgerCoreService {
    async fn create_tenant(
        &self,
        request: Request<pb::CreateTenantRequest>,
    ) -> Result<Response<pb::Tenant>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let tenant = tokio::task::spawn_blocking(move || {
            ledger.create_tenant(req.name, req.currency, req.reserve_floor_minor_units)
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(to_pb_tenant(&tenant)))
    }

    async fn get_tenant(
        &self,
        request: Request<pb::GetTenantRequest>,
    ) -> Result<Response<pb::Tenant>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let tenant = tokio::task::spawn_blocking(move || ledger.get_tenant(&req.id))
            .await
            .map_err(join_error)??;
        Ok(Response::new(to_pb_tenant(&tenant)))
    }

    async fn list_tenants(
        &self,
        request: Request<pb::ListTenantsRequest>,
    ) -> Result<Response<pb::ListTenantsResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let (tenants, next_page_token) = tokio::task::spawn_blocking(move || {
            ledger.list_tenants(req.page_size, &req.page_token)
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(pb::ListTenantsResponse {
            tenants: tenants.iter().map(to_pb_tenant).collect(),
            next_page_token,
        }))
    }

    async fn record_deposit(
        &self,
        request: Request<pb::RecordDepositRequest>,
    ) -> Result<Response<pb::Deposit>, Status> {
        let req = request.into_inner();
        let amount = money_from_pb(req.expected_amount, "expected_amount")?;
        let ledger = self.ledger.clone();
        let deposit = tokio::task::spawn_blocking(move || {
            ledger.record_deposit(
                req.tenant_id,
                amount,
                req.expected_at_unix,
                req.external_ref,
            )
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(to_pb_deposit(&deposit)))
    }

    async fn mark_deposit_received(
        &self,
        request: Request<pb::MarkDepositReceivedRequest>,
    ) -> Result<Response<pb::Deposit>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let (deposit, credit_entry, released_holds) = tokio::task::spawn_blocking(move || {
            ledger.mark_deposit_received_with_entry(
                req.deposit_id,
                req.received_at_unix,
                req.external_ref,
            )
        })
        .await
        .map_err(join_error)??;

        // Mirror into audit-log (see docs/SCALE-EXPANSION.md section 8),
        // same posture as apply_entry's handler above: fire-and-forget,
        // never allowed to delay or fail this response. `credit_entry` is
        // `None` when the deposit was already `Received` (idempotent
        // retry) and nothing new was credited — nothing to mirror.
        // `released_holds` are the bridge-funding HOLDs this deposit just
        // settled (see ledger.rs's `mark_deposit_received_with_entry`
        // doc comment) and must be mirrored too, or the audit trail
        // would show money held forever while the live ledger correctly
        // shows it released.
        if let Some(entry) = &credit_entry {
            self.audit.mirror_ledger_entry(&deposit.tenant_id, entry);
        }
        for release in &released_holds {
            self.audit.mirror_ledger_entry(&deposit.tenant_id, release);
        }

        Ok(Response::new(to_pb_deposit(&deposit)))
    }

    async fn list_deposits(
        &self,
        request: Request<pb::ListDepositsRequest>,
    ) -> Result<Response<pb::ListDepositsResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let deposits = tokio::task::spawn_blocking(move || ledger.list_deposits(&req.tenant_id))
            .await
            .map_err(join_error)??;
        Ok(Response::new(pb::ListDepositsResponse {
            deposits: deposits.iter().map(to_pb_deposit).collect(),
        }))
    }

    async fn list_late_deposits(
        &self,
        request: Request<pb::ListLateDepositsRequest>,
    ) -> Result<Response<pb::ListDepositsResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let deposits =
            tokio::task::spawn_blocking(move || ledger.list_late_deposits(req.as_of_unix))
                .await
                .map_err(join_error)??;
        Ok(Response::new(pb::ListDepositsResponse {
            deposits: deposits.iter().map(to_pb_deposit).collect(),
        }))
    }

    async fn apply_entry(
        &self,
        request: Request<pb::ApplyEntryRequest>,
    ) -> Result<Response<pb::ApplyEntryResponse>, Status> {
        let req = request.into_inner();
        let amount = money_from_pb(req.amount, "amount")?;
        let entry_type = LedgerEntryType::from_i32(req.r#type);
        let ledger = self.ledger.clone();
        let outcome = tokio::task::spawn_blocking(move || {
            ledger
                .apply_entry(
                    req.tenant_id.clone(),
                    entry_type,
                    amount,
                    req.reference,
                    req.idempotency_key,
                    req.memo,
                )
                .map(|outcome| (outcome, req.tenant_id))
        })
        .await
        .map_err(join_error)??;

        let (outcome, tenant_id) = outcome;
        let response = match outcome {
            ApplyOutcome::Applied { entry, balance } => {
                // Mirror into audit-log (see docs/SCALE-EXPANSION.md
                // section 8) only for entries that actually landed —
                // a ReserveFloorViolated rejection moved no money and
                // isn't a ledger event to mirror. Fire-and-forget:
                // never awaited, never allowed to delay or fail this
                // response.
                self.audit.mirror_ledger_entry(&tenant_id, &entry);
                pb::ApplyEntryResponse {
                    entry: Some(to_pb_entry(&entry)),
                    balance_after: Some(to_pb_balance(&tenant_id, &balance, &entry.currency)),
                    reserve_floor_violated: false,
                }
            }
            ApplyOutcome::ReserveFloorViolated { balance, currency } => pb::ApplyEntryResponse {
                entry: None,
                balance_after: Some(to_pb_balance(&tenant_id, &balance, &currency)),
                reserve_floor_violated: true,
            },
        };
        Ok(Response::new(response))
    }

    async fn get_balance(
        &self,
        request: Request<pb::GetBalanceRequest>,
    ) -> Result<Response<pb::Balance>, Status> {
        let req = request.into_inner();
        let tenant_id = req.tenant_id;
        let ledger = self.ledger.clone();
        let tenant_id_for_lookup = tenant_id.clone();
        let (balance, currency) =
            tokio::task::spawn_blocking(move || ledger.get_balance(&tenant_id_for_lookup))
                .await
                .map_err(join_error)??;
        Ok(Response::new(to_pb_balance(
            &tenant_id, &balance, &currency,
        )))
    }

    async fn list_entries(
        &self,
        request: Request<pb::ListEntriesRequest>,
    ) -> Result<Response<pb::ListEntriesResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let (entries, next_page_token) = tokio::task::spawn_blocking(move || {
            ledger.list_entries(&req.tenant_id, req.page_size, &req.page_token)
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(pb::ListEntriesResponse {
            entries: entries.iter().map(to_pb_entry).collect(),
            next_page_token,
        }))
    }

    async fn check_reserve_floor(
        &self,
        request: Request<pb::CheckReserveFloorRequest>,
    ) -> Result<Response<pb::CheckReserveFloorResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let (allowed, headroom) = tokio::task::spawn_blocking(move || {
            ledger.check_reserve_floor(&req.tenant_id, req.proposed_debit_minor_units)
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(pb::CheckReserveFloorResponse {
            allowed,
            headroom_minor_units: headroom,
        }))
    }

    async fn record_agent_decision(
        &self,
        request: Request<pb::RecordAgentDecisionRequest>,
    ) -> Result<Response<pb::AgentDecision>, Status> {
        let req = request.into_inner();
        let action = AgentAction::from_i32(req.action);
        let ledger = self.ledger.clone();
        let decision = tokio::task::spawn_blocking(move || {
            ledger.record_agent_decision(
                req.tenant_id,
                req.trigger,
                action,
                req.rationale,
                req.policy_snapshot_json,
            )
        })
        .await
        .map_err(join_error)??;
        // Mirror into audit-log (see docs/SCALE-EXPANSION.md section
        // 8) — fire-and-forget, same posture as apply_entry above.
        self.audit.mirror_agent_decision(&decision);
        Ok(Response::new(to_pb_agent_decision(&decision)))
    }

    async fn list_agent_decisions(
        &self,
        request: Request<pb::ListAgentDecisionsRequest>,
    ) -> Result<Response<pb::ListAgentDecisionsResponse>, Status> {
        let req = request.into_inner();
        let ledger = self.ledger.clone();
        let decisions = tokio::task::spawn_blocking(move || {
            ledger.list_agent_decisions(&req.tenant_id, req.limit)
        })
        .await
        .map_err(join_error)??;
        Ok(Response::new(pb::ListAgentDecisionsResponse {
            decisions: decisions.iter().map(to_pb_agent_decision).collect(),
        }))
    }
}

fn join_error(e: tokio::task::JoinError) -> Status {
    Status::internal(format!("internal task join error: {e}"))
}
