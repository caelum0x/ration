//! Mutual-TLS credentials for ledger-core's gRPC server and for the
//! gRPC client calls it makes to audit-log (see
//! `crate::audit_mirror`). This is the Rust-side counterpart to
//! `pkg/mtls` (Go) — same env var convention
//! (`<PREFIX>_MTLS_ENABLED`/`CERT_FILE`/`KEY_FILE`/`CA_FILE`/
//! `SERVER_NAME`), same opt-in-disabled-by-default posture, same
//! certs issued by `infra/dev-ca/generate-dev-ca.sh` in local/demo
//! runs.
//!
//! Design mirrors every other mock/live toggle already in this repo
//! (`AIRWALLEX_MODE`, for example): mTLS is opt-in and defaults to
//! disabled, so `cargo run` with zero certs on disk still works. Set
//! `LEDGER_CORE_MTLS_ENABLED=true` and point the `LEDGER_CORE_MTLS_*`
//! vars at files from `infra/dev-ca/` (or a real PKI in a non-dev
//! deployment) to require it.
//!
//! When enabled:
//!   - The server requires and verifies a client certificate signed by
//!     the configured CA (`ServerTlsConfig::client_ca_root`) — a
//!     service that cannot present a cert signed by the shared
//!     internal CA cannot open a connection at all, regardless of what
//!     it sends at the gRPC layer.
//!   - Clients verify the peer's certificate against the same CA and
//!     present their own client certificate, so the handshake is
//!     mutual in both directions — matching the Go side's
//!     `tls.RequireAndVerifyClientCert`.
//!
//! This module deliberately does not implement any kind of
//! certificate-based *authorization* (e.g. "only gateway may call
//! ApplyEntry") — it only establishes a strongly-authenticated
//! transport between services that are already trusted to call each
//! other's RPCs. See `docs/THREAT-MODEL.md` for what this is, and is
//! not, a defense against.

use tonic::transport::{Certificate, ClientTlsConfig, Identity, ServerTlsConfig};

/// One service's mTLS identity (leaf cert/key) plus the CA used to
/// verify whoever it talks to. The zero value (`Config::default()`)
/// is "mTLS disabled" — every method on this type treats a disabled
/// config as a request to skip TLS entirely, never as an error, so
/// callers can wire this unconditionally.
#[derive(Debug, Clone, Default)]
pub struct Config {
    pub enabled: bool,

    /// This service's own leaf certificate and private key (PEM
    /// files), signed by `ca_file`.
    pub cert_file: String,
    pub key_file: String,

    /// The CA certificate (PEM) used to verify peers: the internal dev
    /// CA from `infra/dev-ca/` in local/demo deployments, or a real
    /// internal CA's root/intermediate in a production one.
    pub ca_file: String,

    /// Overrides the name a client verifies the server's certificate
    /// against. Required whenever the dial target string (an addr
    /// like "localhost:7003") doesn't match the peer certificate's
    /// SAN — see `client_tls_config`'s `default_server_name`
    /// parameter, which this overrides when non-empty.
    pub server_name: String,
}

/// Everything that can go wrong loading or applying an mTLS `Config`.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("mtls: invalid {env_var} (expected \"true\" or \"false\"): {source}")]
    InvalidBool {
        env_var: String,
        #[source]
        source: std::str::ParseBoolError,
    },
    #[error("mtls: {enabled_var} requires env vars: {missing:?}")]
    MissingVars {
        enabled_var: String,
        missing: Vec<String>,
    },
    #[error("mtls: read {path}: {source}")]
    ReadFile {
        path: String,
        #[source]
        source: std::io::Error,
    },
    #[error("mtls: build TLS config: {0}")]
    Tonic(#[from] tonic::transport::Error),
}

fn env_key(prefix: &str, suffix: &str) -> String {
    format!("{}_MTLS_{}", prefix.to_uppercase(), suffix)
}

impl Config {
    /// Reads a `Config` for the given service env-var prefix (e.g.
    /// "LEDGER_CORE") from the process environment. Returns a disabled
    /// config, not an error, when `<PREFIX>_MTLS_ENABLED` is unset or
    /// empty — mTLS is opt-in. Returns an error if it is enabled but
    /// any of cert/key/ca file vars are missing, since a
    /// half-configured mTLS setup is worse than refusing to start.
    ///
    /// Only accepts the literal strings "true"/"false" for the
    /// enabled flag (narrower than Go's `strconv.ParseBool`, which
    /// also takes "1"/"t"/"T"/etc) — this is fine in practice since
    /// `infra/dev-ca/generate-dev-ca.sh` and every example `.env`
    /// always write lowercase "true".
    pub fn load_from_env(prefix: &str) -> Result<Self, Error> {
        let enabled_var = env_key(prefix, "ENABLED");
        let enabled_raw = std::env::var(&enabled_var).unwrap_or_default();
        let enabled = if enabled_raw.is_empty() {
            false
        } else {
            enabled_raw
                .parse::<bool>()
                .map_err(|source| Error::InvalidBool {
                    env_var: enabled_var.clone(),
                    source,
                })?
        };

        let cfg = Config {
            enabled,
            cert_file: std::env::var(env_key(prefix, "CERT_FILE")).unwrap_or_default(),
            key_file: std::env::var(env_key(prefix, "KEY_FILE")).unwrap_or_default(),
            ca_file: std::env::var(env_key(prefix, "CA_FILE")).unwrap_or_default(),
            server_name: std::env::var(env_key(prefix, "SERVER_NAME")).unwrap_or_default(),
        };

        if !cfg.enabled {
            return Ok(cfg);
        }

        let mut missing = Vec::new();
        if cfg.cert_file.is_empty() {
            missing.push(env_key(prefix, "CERT_FILE"));
        }
        if cfg.key_file.is_empty() {
            missing.push(env_key(prefix, "KEY_FILE"));
        }
        if cfg.ca_file.is_empty() {
            missing.push(env_key(prefix, "CA_FILE"));
        }
        if !missing.is_empty() {
            return Err(Error::MissingVars {
                enabled_var,
                missing,
            });
        }
        Ok(cfg)
    }

    /// Loads this config's leaf cert/key as a tonic `Identity` and its
    /// CA file as a tonic `Certificate`, common to both server and
    /// client setup.
    fn load_identity_and_ca(&self) -> Result<(Identity, Certificate), Error> {
        let cert = std::fs::read(&self.cert_file).map_err(|source| Error::ReadFile {
            path: self.cert_file.clone(),
            source,
        })?;
        let key = std::fs::read(&self.key_file).map_err(|source| Error::ReadFile {
            path: self.key_file.clone(),
            source,
        })?;
        let ca_pem = std::fs::read(&self.ca_file).map_err(|source| Error::ReadFile {
            path: self.ca_file.clone(),
            source,
        })?;
        Ok((Identity::from_pem(cert, key), Certificate::from_pem(ca_pem)))
    }

    /// Builds the `ServerTlsConfig` ledger-core's gRPC server should
    /// present — its own identity plus `client_ca_root` so only
    /// callers holding a cert signed by that CA can complete the
    /// handshake (mutual auth, matching the Go side's
    /// `RequireAndVerifyClientCert`). Returns `Ok(None)` when mTLS is
    /// disabled; the caller should then skip calling
    /// `Server::builder().tls_config(..)` entirely and serve
    /// plaintext (see `services/ledger-core/src/main.rs`).
    pub fn server_tls_config(&self) -> Result<Option<ServerTlsConfig>, Error> {
        if !self.enabled {
            return Ok(None);
        }
        let (identity, ca) = self.load_identity_and_ca()?;
        Ok(Some(
            ServerTlsConfig::new().identity(identity).client_ca_root(ca),
        ))
    }

    /// Builds the `ClientTlsConfig` a client dialing with this config
    /// should use: its own identity (presented when the peer requests
    /// a client cert) and the shared CA used to verify the peer's
    /// certificate, checked against `default_server_name` unless
    /// `self.server_name` overrides it. Returns `Ok(None)` when mTLS
    /// is disabled.
    pub fn client_tls_config(
        &self,
        default_server_name: &str,
    ) -> Result<Option<ClientTlsConfig>, Error> {
        if !self.enabled {
            return Ok(None);
        }
        let (identity, ca) = self.load_identity_and_ca()?;
        let server_name = if self.server_name.is_empty() {
            default_server_name.to_string()
        } else {
            self.server_name.clone()
        };
        Ok(Some(
            ClientTlsConfig::new()
                .domain_name(server_name)
                .ca_certificate(ca)
                .identity(identity),
        ))
    }

    /// The URI scheme a client dialing with this config should build
    /// its endpoint string with ("https" when mTLS is enabled, "http"
    /// for plaintext) — see `crate::audit_mirror::connect`.
    pub fn dial_scheme(&self) -> &'static str {
        if self.enabled {
            "https"
        } else {
            "http"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::SocketAddr;
    use tonic::transport::{Channel, Server};
    use tonic_health::pb::health_client::HealthClient;
    use tonic_health::pb::HealthCheckRequest;

    // --- load_from_env ----------------------------------------------
    //
    // Each test uses its own env-var prefix so parallel `cargo test`
    // execution never races on the same keys (`std::env::set_var`
    // mutates whole-process state).

    #[test]
    fn load_from_env_disabled_by_default() {
        let prefix = "RATION_TEST_MTLS_DISABLED";
        std::env::remove_var(env_key(prefix, "ENABLED"));
        let cfg = Config::load_from_env(prefix).expect("load_from_env");
        assert!(!cfg.enabled);
    }

    #[test]
    fn load_from_env_enabled_requires_all_files() {
        let prefix = "RATION_TEST_MTLS_MISSING_FILES";
        std::env::set_var(env_key(prefix, "ENABLED"), "true");
        std::env::remove_var(env_key(prefix, "CERT_FILE"));
        std::env::remove_var(env_key(prefix, "KEY_FILE"));
        std::env::remove_var(env_key(prefix, "CA_FILE"));

        let err = Config::load_from_env(prefix).expect_err("expected missing-vars error");
        assert!(matches!(err, Error::MissingVars { .. }), "got {err:?}");

        std::env::remove_var(env_key(prefix, "ENABLED"));
    }

    #[test]
    fn load_from_env_enabled_with_all_files() {
        let prefix = "RATION_TEST_MTLS_ALL_FILES";
        std::env::set_var(env_key(prefix, "ENABLED"), "true");
        std::env::set_var(env_key(prefix, "CERT_FILE"), "/tmp/cert.pem");
        std::env::set_var(env_key(prefix, "KEY_FILE"), "/tmp/key.pem");
        std::env::set_var(env_key(prefix, "CA_FILE"), "/tmp/ca.pem");

        let cfg = Config::load_from_env(prefix).expect("load_from_env");
        assert!(cfg.enabled);
        assert_eq!(cfg.cert_file, "/tmp/cert.pem");
        assert_eq!(cfg.key_file, "/tmp/key.pem");
        assert_eq!(cfg.ca_file, "/tmp/ca.pem");

        for suffix in ["ENABLED", "CERT_FILE", "KEY_FILE", "CA_FILE"] {
            std::env::remove_var(env_key(prefix, suffix));
        }
    }

    #[test]
    fn load_from_env_rejects_non_bool_enabled() {
        let prefix = "RATION_TEST_MTLS_BAD_BOOL";
        std::env::set_var(env_key(prefix, "ENABLED"), "yes-please");
        let err = Config::load_from_env(prefix).expect_err("expected parse error");
        assert!(matches!(err, Error::InvalidBool { .. }), "got {err:?}");
        std::env::remove_var(env_key(prefix, "ENABLED"));
    }

    // --- real end-to-end TLS handshakes ------------------------------
    //
    // These build a throwaway CA and leaf certs entirely in-memory via
    // `rcgen` (no shelling out to openssl, no fixtures on disk beyond
    // a tempdir this test owns) and exercise the real tonic/rustls
    // handshake against tonic-health's standard health service — the
    // same proof the Go `pkg/mtls` test suite runs.

    struct TestCa {
        cert_pem: String,
        key_pair: rcgen::KeyPair,
        cert: rcgen::Certificate,
    }

    fn new_test_ca() -> TestCa {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "ration-dev-ca-test");
        let key_pair = rcgen::KeyPair::generate().unwrap();
        let cert = params.self_signed(&key_pair).unwrap();
        TestCa {
            cert_pem: cert.pem(),
            key_pair,
            cert,
        }
    }

    /// Signs a leaf certificate for `cn`, usable as either a server or
    /// client cert, returning (cert_pem, key_pem).
    fn issue_leaf(ca: &TestCa, cn: &str) -> (String, String) {
        let mut params =
            rcgen::CertificateParams::new(vec![cn.to_string(), "localhost".to_string()]).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, cn);
        let leaf_key = rcgen::KeyPair::generate().unwrap();
        let leaf_cert = params.signed_by(&leaf_key, &ca.cert, &ca.key_pair).unwrap();
        (leaf_cert.pem(), leaf_key.serialize_pem())
    }

    fn write_fixture(dir: &std::path::Path, name: &str, data: &str) -> String {
        let path = dir.join(name);
        std::fs::write(&path, data).expect("write fixture");
        path.to_string_lossy().into_owned()
    }

    /// Starts tonic-health's standard health service on loopback with
    /// `cfg`'s server TLS settings (or plaintext if disabled), serving
    /// in a background task. Returns its dial address and a oneshot
    /// to stop it.
    async fn start_health_server(cfg: &Config) -> (SocketAddr, tokio::sync::oneshot::Sender<()>) {
        // health_reporter() seeds the overall-server status (service
        // name "") as Serving already, which is exactly the check
        // these tests make (HealthCheckRequest::default() has an
        // empty service name) — no explicit set_serving call needed.
        let (_health_reporter, health_service) = tonic_health::server::health_reporter();

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        let mut builder = Server::builder();
        if let Some(tls) = cfg.server_tls_config().expect("server_tls_config") {
            builder = builder.tls_config(tls).expect("apply tls_config");
        }

        let (tx, rx) = tokio::sync::oneshot::channel::<()>();
        tokio::spawn(async move {
            builder
                .add_service(health_service)
                .serve_with_incoming_shutdown(
                    tokio_stream::wrappers::TcpListenerStream::new(listener),
                    async {
                        let _ = rx.await;
                    },
                )
                .await
                .unwrap();
        });

        (addr, tx)
    }

    async fn dial(
        addr: SocketAddr,
        cfg: &Config,
        default_server_name: &str,
    ) -> Result<Channel, tonic::transport::Error> {
        let scheme = cfg.dial_scheme();
        let endpoint = Channel::from_shared(format!("{scheme}://{addr}")).unwrap();
        let endpoint = match cfg
            .client_tls_config(default_server_name)
            .expect("client_tls_config")
        {
            Some(tls) => endpoint.tls_config(tls)?,
            None => endpoint,
        };
        endpoint.connect().await
    }

    #[tokio::test]
    async fn disabled_config_round_trips_over_plaintext() {
        let cfg = Config::default();
        let (addr, _stop) = start_health_server(&cfg).await;

        let channel = dial(addr, &cfg, "ignored").await.expect("dial");
        let mut client = HealthClient::new(channel);
        let resp = client
            .check(HealthCheckRequest::default())
            .await
            .expect("health check over plaintext should succeed");
        assert_eq!(
            resp.into_inner().status,
            tonic_health::pb::health_check_response::ServingStatus::Serving as i32
        );
    }

    #[tokio::test]
    async fn mtls_client_with_valid_cert_succeeds() {
        let dir = tempfile::tempdir().unwrap();
        let ca = new_test_ca();
        let ca_path = write_fixture(dir.path(), "ca.pem", &ca.cert_pem);

        let (server_cert, server_key) = issue_leaf(&ca, "ledger-core-test");
        let server_cert_path = write_fixture(dir.path(), "server-cert.pem", &server_cert);
        let server_key_path = write_fixture(dir.path(), "server-key.pem", &server_key);

        let (client_cert, client_key) = issue_leaf(&ca, "audit-log-test");
        let client_cert_path = write_fixture(dir.path(), "client-cert.pem", &client_cert);
        let client_key_path = write_fixture(dir.path(), "client-key.pem", &client_key);

        let server_cfg = Config {
            enabled: true,
            cert_file: server_cert_path,
            key_file: server_key_path,
            ca_file: ca_path.clone(),
            server_name: String::new(),
        };
        let (addr, _stop) = start_health_server(&server_cfg).await;

        let client_cfg = Config {
            enabled: true,
            cert_file: client_cert_path,
            key_file: client_key_path,
            ca_file: ca_path,
            server_name: String::new(),
        };
        let channel = dial(addr, &client_cfg, "ledger-core-test")
            .await
            .expect("mTLS dial should succeed");
        let mut client = HealthClient::new(channel);
        let resp = client
            .check(HealthCheckRequest::default())
            .await
            .expect("expected mTLS handshake to succeed");
        assert_eq!(
            resp.into_inner().status,
            tonic_health::pb::health_check_response::ServingStatus::Serving as i32
        );
    }

    #[tokio::test]
    async fn mtls_client_with_untrusted_ca_is_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let trusted_ca = new_test_ca();
        let untrusted_ca = new_test_ca();

        let trusted_ca_path = write_fixture(dir.path(), "trusted-ca.pem", &trusted_ca.cert_pem);
        let (server_cert, server_key) = issue_leaf(&trusted_ca, "ledger-core-test");
        let server_cert_path = write_fixture(dir.path(), "server-cert.pem", &server_cert);
        let server_key_path = write_fixture(dir.path(), "server-key.pem", &server_key);

        let server_cfg = Config {
            enabled: true,
            cert_file: server_cert_path,
            key_file: server_key_path,
            ca_file: trusted_ca_path.clone(),
            server_name: String::new(),
        };
        let (addr, _stop) = start_health_server(&server_cfg).await;

        // Client presents a cert signed by the untrusted CA, but still
        // trusts (verifies the server against) the actually-trusted
        // CA — modeling a rogue/compromised service whose client cert
        // was never issued by the real internal CA.
        let (rogue_cert, rogue_key) = issue_leaf(&untrusted_ca, "rogue-test");
        let rogue_cert_path = write_fixture(dir.path(), "rogue-cert.pem", &rogue_cert);
        let rogue_key_path = write_fixture(dir.path(), "rogue-key.pem", &rogue_key);

        let client_cfg = Config {
            enabled: true,
            cert_file: rogue_cert_path,
            key_file: rogue_key_path,
            ca_file: trusted_ca_path,
            server_name: "ledger-core-test".to_string(),
        };

        // `Endpoint::connect()` itself succeeds here — tonic/hyper
        // pool the connection lazily under the hood regardless of
        // which connect method is called, so the TLS handshake (and
        // therefore the rejection) only actually happens once a
        // request is sent on it. Assert on the RPC, not on `connect`.
        let channel = dial(addr, &client_cfg, "ledger-core-test")
            .await
            .expect("connect() itself does not eagerly handshake");
        let mut client = HealthClient::new(channel);
        let result = client.check(HealthCheckRequest::default()).await;
        assert!(
            result.is_err(),
            "expected a client cert from an untrusted CA to be rejected, got {result:?}"
        );
    }
}
