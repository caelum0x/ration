//! A gRPC client for `Auth`'s `VerifyAPIKey`/`VerifyJWT` RPCs, used by
//! [`crate::tenantauth`] to authorize every tenant-scoped `LedgerCore`
//! call directly — closing `docs/THREAT-MODEL.md`'s "Known gaps" item
//! 1's own words for this exact sub-gap: "the biggest one... cli/tui/
//! agent all dial ledger-core directly today... bypassing gateway and
//! [gateway's tenantauth] entirely, so any operator/compromised agent
//! can act as any tenant_id on the system's actual source of truth
//! regardless of what gateway enforces."
//!
//! Mirrors [`crate::audit_mirror::AuditMirror`]'s connection shape
//! (lazy `Endpoint::connect_lazy`, an `Option<Client>` that is `None`
//! when disabled) but is NOT best-effort/fire-and-forget like that
//! module: a verification failure here must fail the RPC it is
//! guarding, never be logged-and-dropped. Money access control is not
//! optional the way a second audit copy is.

use tonic::transport::{Channel, Endpoint};
use tonic::Status;

use crate::mtls;
use crate::pb::ration::auth::v1 as pb;
use pb::auth_client::AuthClient;

/// The identity name `infra/dev-ca/generate-dev-ca.sh` issues auth's
/// certificate for — the default a client verifies auth's certificate
/// against unless `LEDGER_CORE_MTLS_SERVER_NAME` overrides it (see
/// `mtls::Config::client_tls_config`), same convention as
/// `audit_mirror::AUDIT_LOG_SERVER_NAME`.
const AUTH_SERVER_NAME: &str = "auth";

/// The result of verifying a presented API key or JWT, shaped
/// identically regardless of which of the two credential types was
/// checked (mirrors `services/gateway/internal/tenantauth.Verifier`'s
/// Go method signatures exactly, so the authorization logic in
/// `crate::tenantauth` doesn't need to know which RPC produced this).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerifyOutcome {
    pub valid: bool,
    pub tenant_id: String,
    /// Mirrors `proto/auth.proto`'s `is_operator` field: `true` means
    /// this credential is pre-authorized for every `tenant_id`, not
    /// bound to one — see `crate::tenantauth`'s doc comment for how
    /// that changes the match.
    pub is_operator: bool,
    /// Explains an invalid result (e.g. "revoked", "expired"); empty
    /// when `valid` is `true`.
    pub reason: String,
}

/// A handle to Auth's verification RPCs. Cheap to clone (wraps a
/// lazily-connecting tonic `Channel`, itself cheaply cloneable).
#[derive(Clone)]
pub struct AuthVerifier {
    client: Option<AuthClient<Channel>>,
}

impl AuthVerifier {
    /// A verifier with no Auth connection configured: every `verify_*`
    /// call on it fails closed (`Status::internal`, never `Ok`) rather
    /// than silently treating every credential as invalid or, worse,
    /// every request as unauthenticated-but-allowed. Used when
    /// `AUTH_GRPC_ADDR` is unset — this is only safe to run with
    /// alongside `require_tenant_auth = false` (see
    /// `crate::tenantauth::Config`); main.rs refuses to start with the
    /// opposite combination.
    pub fn disabled() -> Self {
        Self { client: None }
    }

    /// Builds a verifier dialing `addr` (a "host:port" string from
    /// `AUTH_GRPC_ADDR`, e.g. `localhost:7010`) with `mtls_cfg`'s
    /// client settings (reusing ledger-core's own `LEDGER_CORE_MTLS_*`
    /// identity, exactly like `audit_mirror::AuditMirror::connect`).
    /// The connection is lazy: no network I/O happens here, so
    /// ledger-core's own boot never blocks on, or fails because of,
    /// auth being slow or down — the first `verify_*` call is what
    /// actually dials, and any failure there is returned to that
    /// call's own caller, not swallowed.
    pub fn connect(addr: &str, mtls_cfg: &mtls::Config) -> Result<Self, mtls::Error> {
        let scheme = mtls_cfg.dial_scheme();
        let mut endpoint = Endpoint::from_shared(format!("{scheme}://{addr}"))?;
        if let Some(tls) = mtls_cfg.client_tls_config(AUTH_SERVER_NAME)? {
            endpoint = endpoint.tls_config(tls)?;
        }
        let channel = endpoint.connect_lazy();
        Ok(Self {
            client: Some(AuthClient::new(channel)),
        })
    }

    /// Whether this verifier was built with a real Auth connection
    /// (`connect`) rather than `disabled`. Used at startup to refuse
    /// to run with tenant auth required but nothing configured to
    /// check it against (see `main.rs`).
    pub fn is_configured(&self) -> bool {
        self.client.is_some()
    }

    pub async fn verify_api_key(&self, secret: &str) -> Result<VerifyOutcome, Status> {
        let Some(client) = self.client.clone() else {
            return Err(Status::internal(
                "authclient: no AUTH_GRPC_ADDR configured, cannot verify API key",
            ));
        };
        let mut client = client;
        let resp = client
            .verify_api_key(pb::VerifyApiKeyRequest {
                secret: secret.to_string(),
            })
            .await
            .map_err(|status| {
                Status::unauthenticated(format!(
                    "authclient: VerifyAPIKey call failed: {status}"
                ))
            })?
            .into_inner();
        Ok(VerifyOutcome {
            valid: resp.valid,
            tenant_id: resp.tenant_id,
            is_operator: resp.is_operator,
            reason: resp.reason,
        })
    }

    pub async fn verify_jwt(&self, token: &str) -> Result<VerifyOutcome, Status> {
        let Some(client) = self.client.clone() else {
            return Err(Status::internal(
                "authclient: no AUTH_GRPC_ADDR configured, cannot verify JWT",
            ));
        };
        let mut client = client;
        let resp = client
            .verify_jwt(pb::VerifyJwtRequest {
                token: token.to_string(),
            })
            .await
            .map_err(|status| {
                Status::unauthenticated(format!("authclient: VerifyJWT call failed: {status}"))
            })?
            .into_inner();
        Ok(VerifyOutcome {
            valid: resp.valid,
            tenant_id: resp.tenant_id,
            is_operator: resp.is_operator,
            reason: resp.reason,
        })
    }
}
