//! Tenant-scoped authorization for `LedgerCore`'s own gRPC surface —
//! closes `docs/THREAT-MODEL.md`'s "Known gaps" item 1's "the biggest
//! one" sub-gap: cli/tui/agent (and every other internal caller —
//! gateway's own forwarding, card-issuance's bridge/card services,
//! webhook-ingestion) dial ledger-core directly, bypassing gateway's
//! `internal/tenantauth` entirely, so without this, any of them (or
//! anything that compromises one of them) could act as any `tenant_id`
//! against the system's actual source of truth, regardless of what
//! gateway enforces upstream.
//!
//! Mirrors `services/gateway/internal/tenantauth`'s design on the Go
//! side as closely as Rust's lack of proto reflection allows: that
//! package resolves a request's `tenant_id` field generically via
//! `protoreflect`; here, every `LedgerCore` RPC handler in
//! `crate::grpc` instead calls [`authorize_tenant`] or
//! [`authorize_operator`] explicitly with its own already-decoded
//! `tenant_id` (or, for the handful of RPCs that are provisioning/
//! cross-tenant by nature — see [`authorize_operator`]'s doc comment —
//! no tenant_id at all). Same credential types, same header names, same
//! operator-bypass semantics; just enforced per-handler instead of via
//! one generic interceptor.

use tonic::metadata::MetadataMap;
use tonic::{Request, Status};

use crate::authclient::AuthVerifier;

/// Metadata key for an API-key credential — must match
/// `services/gateway/internal/tenantauth`'s `apiKeyHeader` constant
/// exactly (different Go/Rust modules, same wire contract).
const API_KEY_HEADER: &str = "x-api-key";
/// Metadata key for a JWT bearer credential — must match
/// `services/gateway/internal/tenantauth`'s `authorizationHeader`
/// constant exactly.
const AUTHORIZATION_HEADER: &str = "authorization";
const BEARER_PREFIX: &str = "Bearer ";

/// Runtime configuration for this module, loaded once at startup (see
/// `main.rs`) and held by `LedgerCoreService`.
#[derive(Clone)]
pub struct Config {
    pub verifier: AuthVerifier,
    /// Gates every `authorize_*` call below: `false` makes every one
    /// an immediate no-op, matching gateway's identical
    /// `RATION_GATEWAY_REQUIRE_TENANT_AUTH` opt-out knob (the same
    /// opt-in-by-default-true pattern as mTLS's `*_MTLS_ENABLED`).
    /// `RATION_LEDGER_REQUIRE_TENANT_AUTH` controls this; see
    /// `.env.example`.
    pub required: bool,
}

/// Extracts whichever of the two supported credential headers is
/// present on `md`, the same (apiKey, bearer, present) shape as
/// gateway's `credentialFromContext`. Presenting both is not an error;
/// the API key is checked first, matching gateway's own tie-break.
fn credential_from_metadata(md: &MetadataMap) -> (Option<String>, Option<String>) {
    let api_key = md
        .get(API_KEY_HEADER)
        .and_then(|v| v.to_str().ok())
        .filter(|v| !v.is_empty())
        .map(str::to_string);

    let bearer = md
        .get_all(AUTHORIZATION_HEADER)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .find_map(|v| v.strip_prefix(BEARER_PREFIX))
        .map(str::to_string);

    (api_key, bearer)
}

/// The outcome of a successful credential check: either a specific
/// tenant, or an operator (pre-authorized for every tenant_id).
enum Verified {
    Tenant(String),
    Operator,
}

/// Verifies whichever credential `md` carries against `cfg.verifier`,
/// returning the authenticated identity or a ready-to-return
/// `Unauthenticated` status. Shared by `authorize_tenant` and
/// `authorize_operator` below.
async fn authenticate(cfg: &Config, md: &MetadataMap) -> Result<Verified, Status> {
    let (api_key, bearer) = credential_from_metadata(md);
    let outcome = match (api_key, bearer) {
        (Some(key), _) => cfg.verifier.verify_api_key(&key).await?,
        (None, Some(token)) => cfg.verifier.verify_jwt(&token).await?,
        (None, None) => {
            return Err(Status::unauthenticated(format!(
                "tenantauth: this call requires an {API_KEY_HEADER} or {AUTHORIZATION_HEADER} credential"
            )));
        }
    };
    if !outcome.valid {
        return Err(Status::unauthenticated(format!(
            "tenantauth: invalid credential: {}",
            outcome.reason
        )));
    }
    if outcome.is_operator {
        return Ok(Verified::Operator);
    }
    Ok(Verified::Tenant(outcome.tenant_id))
}

/// Authorizes a tenant-scoped RPC: the caller's credential must verify
/// for exactly `tenant_id`, or be operator-scoped (pre-authorized for
/// every tenant_id — see `proto/auth.proto`'s `is_operator` field).
/// A no-op when `cfg.required` is `false`.
///
/// Call this from every `LedgerCore` handler whose request carries a
/// real `tenant_id` it acts on: `RecordDeposit`, `MarkDepositReceived`
/// (via the deposit's own tenant, looked up first), `ListDeposits`,
/// `ApplyEntry`, `GetBalance`, `ListEntries`, `CheckReserveFloor`,
/// `RecordAgentDecision`, `ListAgentDecisions`.
pub async fn authorize_tenant<T>(cfg: &Config, request: &Request<T>, tenant_id: &str) -> Result<(), Status> {
    if !cfg.required {
        return Ok(());
    }
    match authenticate(cfg, request.metadata()).await? {
        Verified::Operator => Ok(()),
        Verified::Tenant(verified) if verified == tenant_id => Ok(()),
        Verified::Tenant(_) => Err(Status::permission_denied(format!(
            "tenantauth: credential is not authorized for tenant_id {tenant_id:?}"
        ))),
    }
}

/// Authorizes an operator-only RPC: the caller's credential must be
/// operator-scoped, full stop. A no-op when `cfg.required` is `false`.
///
/// `LedgerCore` has a handful of RPCs with no single tenant to check a
/// credential against, by nature: `CreateTenant`/`GetTenant`/
/// `ListTenants` (tenant provisioning and cross-tenant listing — a
/// tenant can't hold a credential for itself before it exists, and
/// listing every tenant is definitionally cross-tenant) and
/// `ListLateDeposits` (scans every tenant's outstanding deposits — this
/// is exactly what the agent's poll loop needs, and only the agent, an
/// operator-scoped caller, calls it today). A tenant-scoped credential
/// is never accepted for these, even for its own tenant_id, because
/// most of them don't have one to check against in the first place.
pub async fn authorize_operator<T>(cfg: &Config, request: &Request<T>) -> Result<(), Status> {
    if !cfg.required {
        return Ok(());
    }
    match authenticate(cfg, request.metadata()).await? {
        Verified::Operator => Ok(()),
        Verified::Tenant(_) => Err(Status::permission_denied(
            "tenantauth: this call requires an operator-scoped credential",
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::metadata::MetadataValue;

    #[test]
    fn credential_from_metadata_prefers_api_key_over_bearer() {
        let mut md = MetadataMap::new();
        md.insert(API_KEY_HEADER, MetadataValue::try_from("secret-123").unwrap());
        md.insert(
            AUTHORIZATION_HEADER,
            MetadataValue::try_from("Bearer tok-abc").unwrap(),
        );
        let (api_key, bearer) = credential_from_metadata(&md);
        assert_eq!(api_key, Some("secret-123".to_string()));
        assert_eq!(bearer, Some("tok-abc".to_string()));
    }

    #[test]
    fn credential_from_metadata_extracts_bearer_token() {
        let mut md = MetadataMap::new();
        md.insert(
            AUTHORIZATION_HEADER,
            MetadataValue::try_from("Bearer tok-xyz").unwrap(),
        );
        let (api_key, bearer) = credential_from_metadata(&md);
        assert_eq!(api_key, None);
        assert_eq!(bearer, Some("tok-xyz".to_string()));
    }

    #[test]
    fn credential_from_metadata_ignores_non_bearer_authorization() {
        let mut md = MetadataMap::new();
        md.insert(
            AUTHORIZATION_HEADER,
            MetadataValue::try_from("Basic dXNlcjpwYXNz").unwrap(),
        );
        let (api_key, bearer) = credential_from_metadata(&md);
        assert_eq!(api_key, None);
        assert_eq!(bearer, None);
    }

    #[test]
    fn credential_from_metadata_empty_when_nothing_present() {
        let md = MetadataMap::new();
        let (api_key, bearer) = credential_from_metadata(&md);
        assert_eq!(api_key, None);
        assert_eq!(bearer, None);
    }
}
