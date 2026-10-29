//! Typed Rust client for Ration's [`gateway.proto`](../../../proto/gateway.proto)
//! `Gateway` service (docs/SCALE-EXPANSION.md section 7) — the Rust
//! counterpart to `sdk/go/ration`. `gateway.proto`'s external surface is
//! unchanged by the section-1 service split, so this crate is stable
//! against both the pre-split monolith and the current thin-BFF gateway.
//!
//! Gateway's own `internal/tenantauth` interceptor defaults
//! `RATION_GATEWAY_REQUIRE_TENANT_AUTH` to `true` (see
//! `services/gateway/internal/config`), so every RPC below is rejected
//! with `codes::Unauthenticated` against a default-config deployment
//! unless a credential is attached — mint one with
//! `ration apikey issue --tenant <id>` and connect with
//! [`Client::connect_with_api_key`]:
//!
//! ```no_run
//! # async fn example() -> Result<(), ration_client::ClientError> {
//! let mut client =
//!     ration_client::Client::connect_with_api_key("http://localhost:7002", "rk_...").await?;
//! let card = client
//!     .issue_card("tenant_1", "Jane Doe", 1_000_00, "monthly", "USD")
//!     .await?;
//! println!("issued {}", card.id);
//! # Ok(())
//! # }
//! ```

pub mod error;

/// Generated protobuf/gRPC types, nested by proto package (matches the
/// convention `services/ledger-core/src/lib.rs` uses for the same
/// family of protos).
pub mod pb {
    pub mod ration {
        pub mod common {
            pub mod v1 {
                tonic::include_proto!("ration.common.v1");
            }
        }
        pub mod gateway {
            pub mod v1 {
                tonic::include_proto!("ration.gateway.v1");
            }
        }
    }
}

pub use error::ClientError;
pub use pb::ration::common::v1 as common;
pub use pb::ration::gateway::v1::{ActivityEvent, Card};

use pb::ration::gateway::v1::{
    gateway_client::GatewayClient, AdjustCardLimitRequest, CloseCardRequest, GetCardRequest,
    IssueCardRequest, ListCardsRequest, RationBridgeFundingRequest, SimulateDepositRequest,
    StreamActivityRequest,
};
use tonic::metadata::MetadataValue;
use tonic::service::interceptor::InterceptedService;
use tonic::service::Interceptor;
use tonic::transport::{Channel, Endpoint};
use tonic::{Request, Status, Streaming};

/// `x-api-key`/`authorization` must match
/// `services/gateway/internal/tenantauth`'s identically-named header
/// constants exactly — that package is the server side of this
/// exchange and is a separate Go module, so there is no shared import
/// to keep these in sync (same reasoning as `sdk/go/ration`'s own
/// repeated `apiKeyHeader`/`authorizationHeader` constants).
const API_KEY_HEADER: &str = "x-api-key";
const AUTHORIZATION_HEADER: &str = "authorization";
const BEARER_PREFIX: &str = "Bearer ";

/// Attaches a single credential header (if any) to every unary and
/// streaming RPC a [`Client`] makes — the Rust counterpart to
/// `sdk/go/ration`'s `WithAPIKey`/`WithBearerToken` dial options.
/// `None` makes this a no-op pass-through so [`Client::from_channel`]
/// keeps working unauthenticated for callers who attach their own
/// credential via a custom interceptor on the channel.
#[derive(Debug, Clone, Default)]
struct CredentialInterceptor {
    header: Option<(&'static str, String)>,
}

impl Interceptor for CredentialInterceptor {
    fn call(&mut self, mut request: Request<()>) -> Result<Request<()>, Status> {
        if let Some((key, value)) = &self.header {
            let metadata_value = MetadataValue::try_from(value.as_str())
                .map_err(|_| Status::invalid_argument("credential value is not valid metadata"))?;
            request.metadata_mut().insert(*key, metadata_value);
        }
        Ok(request)
    }
}

/// `RequestBridgeFunding`'s typed result — the grant may be partial or
/// zero, so check `fully_granted`/`explanation`, not just the `Result`.
#[derive(Debug, Clone)]
pub struct BridgeFundingResult {
    pub granted_minor_units: i64,
    pub fully_granted: bool,
    pub explanation: String,
}

/// A connected client for `gateway.proto`'s `Gateway` service.
#[derive(Debug, Clone)]
pub struct Client {
    inner: GatewayClient<InterceptedService<Channel, CredentialInterceptor>>,
}

impl Client {
    /// Connects to a Ration gateway at `addr` (e.g.
    /// `"http://localhost:7002"`, gateway's `GATEWAY_GRPC_ADDR` default
    /// over plaintext HTTP/2) with no credential attached — only usable
    /// against a deployment with `RATION_GATEWAY_REQUIRE_TENANT_AUTH`
    /// explicitly disabled. Use [`Client::connect_with_api_key`] or
    /// [`Client::connect_with_bearer_token`] against a default-config
    /// gateway. For mTLS against a production deployment (see
    /// `pkg/mtls`, `infra/dev-ca/`), build your own
    /// `tonic::transport::Channel` with `ClientTlsConfig` and construct
    /// via [`Client::from_channel`] instead.
    pub async fn connect(addr: impl Into<String>) -> Result<Self, ClientError> {
        let channel = Self::dial(addr).await?;
        Ok(Self::from_channel(channel))
    }

    /// Connects to a Ration gateway at `addr` and attaches `secret` (a
    /// `services/auth` `IssueAPIKey` plaintext response, e.g. minted via
    /// `ration apikey issue`) as `x-api-key` metadata on every unary and
    /// streaming RPC this client makes — see the module-level docs for
    /// why a credential is required at all against a default-config
    /// gateway.
    pub async fn connect_with_api_key(
        addr: impl Into<String>,
        secret: impl Into<String>,
    ) -> Result<Self, ClientError> {
        let channel = Self::dial(addr).await?;
        Ok(Self::from_channel_with_api_key(channel, secret))
    }

    /// Connects to a Ration gateway at `addr` and attaches `token` (a
    /// `services/auth` `MintJWT` response) as `authorization: Bearer
    /// <token>` metadata on every unary and streaming RPC this client
    /// makes — a short-lived alternative to
    /// [`Client::connect_with_api_key`]'s long-lived API key.
    pub async fn connect_with_bearer_token(
        addr: impl Into<String>,
        token: impl Into<String>,
    ) -> Result<Self, ClientError> {
        let channel = Self::dial(addr).await?;
        Ok(Self::from_channel_with_bearer_token(channel, token))
    }

    async fn dial(addr: impl Into<String>) -> Result<Channel, ClientError> {
        let endpoint = Endpoint::from_shared(addr.into())?;
        endpoint.connect().await.map_err(ClientError::from)
    }

    /// Wraps an already-established channel with no credential attached
    /// — for mTLS, a custom interceptor already attached to the channel,
    /// or tests dialing an in-process server.
    pub fn from_channel(channel: Channel) -> Self {
        Self {
            inner: GatewayClient::with_interceptor(channel, CredentialInterceptor::default()),
        }
    }

    /// Wraps an already-established channel and attaches `secret` as
    /// `x-api-key` metadata on every RPC — see
    /// [`Client::connect_with_api_key`].
    pub fn from_channel_with_api_key(channel: Channel, secret: impl Into<String>) -> Self {
        Self {
            inner: GatewayClient::with_interceptor(
                channel,
                CredentialInterceptor {
                    header: Some((API_KEY_HEADER, secret.into())),
                },
            ),
        }
    }

    /// Wraps an already-established channel and attaches `token` as
    /// `authorization: Bearer <token>` metadata on every RPC — see
    /// [`Client::connect_with_bearer_token`].
    pub fn from_channel_with_bearer_token(channel: Channel, token: impl Into<String>) -> Self {
        Self {
            inner: GatewayClient::with_interceptor(
                channel,
                CredentialInterceptor {
                    header: Some((
                        AUTHORIZATION_HEADER,
                        format!("{BEARER_PREFIX}{}", token.into()),
                    )),
                },
            ),
        }
    }

    /// Issues a new card for `tenant_id` with the given spending limit.
    /// `limit_interval` is `"daily"` or `"monthly"`.
    pub async fn issue_card(
        &mut self,
        tenant_id: impl Into<String>,
        cardholder_name: impl Into<String>,
        limit_minor_units: i64,
        limit_interval: impl Into<String>,
        currency: impl Into<String>,
    ) -> Result<Card, ClientError> {
        let req = IssueCardRequest {
            tenant_id: tenant_id.into(),
            cardholder_name: cardholder_name.into(),
            limit_minor_units,
            limit_interval: limit_interval.into(),
            currency: currency.into(),
        };
        Ok(self.inner.issue_card(req).await?.into_inner())
    }

    /// Changes an existing card's spending limit.
    pub async fn adjust_card_limit(
        &mut self,
        card_id: impl Into<String>,
        new_limit_minor_units: i64,
        reason: impl Into<String>,
    ) -> Result<Card, ClientError> {
        let req = AdjustCardLimitRequest {
            card_id: card_id.into(),
            new_limit_minor_units,
            reason: reason.into(),
        };
        Ok(self.inner.adjust_card_limit(req).await?.into_inner())
    }

    /// Permanently closes a card: releases its full remaining ledger
    /// hold back to the tenant's available balance and marks it
    /// `CARD_STATUS_CLOSED`. Closing an already-closed card is
    /// idempotent — it returns the card unchanged, not an error.
    /// `adjust_card_limit` rejects a closed card outright rather than
    /// reopening a hold on it.
    pub async fn close_card(&mut self, card_id: impl Into<String>) -> Result<Card, ClientError> {
        let req = CloseCardRequest {
            card_id: card_id.into(),
        };
        Ok(self.inner.close_card(req).await?.into_inner())
    }

    /// Fetches a single card by id.
    pub async fn get_card(&mut self, card_id: impl Into<String>) -> Result<Card, ClientError> {
        let req = GetCardRequest {
            card_id: card_id.into(),
        };
        Ok(self.inner.get_card(req).await?.into_inner())
    }

    /// Lists every card for `tenant_id`.
    pub async fn list_cards(
        &mut self,
        tenant_id: impl Into<String>,
    ) -> Result<Vec<Card>, ClientError> {
        let req = ListCardsRequest {
            tenant_id: tenant_id.into(),
        };
        Ok(self.inner.list_cards(req).await?.into_inner().cards)
    }

    /// Asks the agent's hard-rule policy envelope (see
    /// `docs/AGENT-DESIGN.md`) to grant bridge funding for `tenant_id`.
    pub async fn request_bridge_funding(
        &mut self,
        tenant_id: impl Into<String>,
        requested_minor_units: i64,
        reason: impl Into<String>,
    ) -> Result<BridgeFundingResult, ClientError> {
        let req = RationBridgeFundingRequest {
            tenant_id: tenant_id.into(),
            requested_minor_units,
            reason: reason.into(),
        };
        let resp = self.inner.ration_bridge_funding(req).await?.into_inner();
        Ok(BridgeFundingResult {
            granted_minor_units: resp.granted_minor_units,
            fully_granted: resp.fully_granted,
            explanation: resp.explanation,
        })
    }

    /// Records an expected deposit with ledger-core (a demo/judging hook —
    /// see `grpcserver.Server.SimulateDeposit` on the Go side). `expected_at_unix`
    /// of `0` defaults to 24h from now; `mark_late` backdates it so it is
    /// immediately reportable as late. Returns the new deposit's id.
    pub async fn simulate_deposit(
        &mut self,
        tenant_id: impl Into<String>,
        amount_minor_units: i64,
        expected_at_unix: i64,
        mark_late: bool,
    ) -> Result<String, ClientError> {
        let req = SimulateDepositRequest {
            tenant_id: tenant_id.into(),
            amount_minor_units,
            expected_at_unix,
            mark_late,
        };
        Ok(self
            .inner
            .simulate_deposit(req)
            .await?
            .into_inner()
            .deposit_id)
    }

    /// Subscribes to `tenant_id`'s activity feed (empty `tenant_id`
    /// subscribes to every tenant). The returned stream yields
    /// `Result<ActivityEvent, tonic::Status>` items directly from the
    /// underlying gRPC stream — poll it with `futures_util::StreamExt` or
    /// `tokio_stream::StreamExt`.
    pub async fn stream_activity(
        &mut self,
        tenant_id: impl Into<String>,
    ) -> Result<Streaming<ActivityEvent>, ClientError> {
        let req = StreamActivityRequest {
            tenant_id: tenant_id.into(),
        };
        Ok(self.inner.stream_activity(req).await?.into_inner())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pb::ration::gateway::v1::gateway_server::{Gateway, GatewayServer};
    use crate::pb::ration::gateway::v1::{
        ListCardsResponse, RationBridgeFundingResponse, SimulateDepositResponse,
    };
    use hyper_util::rt::TokioIo;
    use std::pin::Pin;
    use tokio::io::duplex;
    use tokio_stream::Stream;
    use tonic::transport::{Server, Uri};
    use tonic::{Request, Response, Status};
    use tower::service_fn;

    /// Implements the generated `Gateway` server trait with no network and
    /// no real services behind it, so this client's wire-level behavior
    /// can be tested without standing up services/gateway and its four
    /// backing services — the Rust counterpart to sdk/go/ration's
    /// `fakeGateway`.
    #[derive(Default)]
    struct FakeGateway;

    #[tonic::async_trait]
    impl Gateway for FakeGateway {
        async fn issue_card(
            &self,
            request: Request<IssueCardRequest>,
        ) -> Result<Response<Card>, Status> {
            let req = request.into_inner();
            Ok(Response::new(Card {
                id: "card_1".to_string(),
                tenant_id: req.tenant_id,
                cardholder_name: req.cardholder_name,
                limit_minor_units: req.limit_minor_units,
                limit_interval: req.limit_interval,
                currency: req.currency,
                ..Default::default()
            }))
        }

        async fn adjust_card_limit(
            &self,
            request: Request<AdjustCardLimitRequest>,
        ) -> Result<Response<Card>, Status> {
            let req = request.into_inner();
            Ok(Response::new(Card {
                id: req.card_id,
                limit_minor_units: req.new_limit_minor_units,
                ..Default::default()
            }))
        }

        async fn get_card(
            &self,
            request: Request<GetCardRequest>,
        ) -> Result<Response<Card>, Status> {
            Ok(Response::new(Card {
                id: request.into_inner().card_id,
                ..Default::default()
            }))
        }

        async fn list_cards(
            &self,
            request: Request<ListCardsRequest>,
        ) -> Result<Response<ListCardsResponse>, Status> {
            let tenant_id = request.into_inner().tenant_id;
            Ok(Response::new(ListCardsResponse {
                cards: vec![
                    Card {
                        id: "card_1".to_string(),
                        tenant_id: tenant_id.clone(),
                        ..Default::default()
                    },
                    Card {
                        id: "card_2".to_string(),
                        tenant_id,
                        ..Default::default()
                    },
                ],
            }))
        }

        async fn ration_bridge_funding(
            &self,
            request: Request<RationBridgeFundingRequest>,
        ) -> Result<Response<RationBridgeFundingResponse>, Status> {
            let req = request.into_inner();
            Ok(Response::new(RationBridgeFundingResponse {
                granted_minor_units: req.requested_minor_units / 2,
                fully_granted: false,
                explanation: "reserve floor headroom only covers half the request".to_string(),
            }))
        }

        async fn simulate_deposit(
            &self,
            request: Request<SimulateDepositRequest>,
        ) -> Result<Response<SimulateDepositResponse>, Status> {
            if request.into_inner().tenant_id.is_empty() {
                return Err(Status::invalid_argument("tenant_id is required"));
            }
            Ok(Response::new(SimulateDepositResponse {
                deposit_id: "dep_1".to_string(),
            }))
        }

        type StreamActivityStream =
            Pin<Box<dyn Stream<Item = Result<ActivityEvent, Status>> + Send + 'static>>;

        async fn stream_activity(
            &self,
            request: Request<StreamActivityRequest>,
        ) -> Result<Response<Self::StreamActivityStream>, Status> {
            let tenant_id = request.into_inner().tenant_id;
            let event = ActivityEvent {
                id: "evt_1".to_string(),
                tenant_id,
                kind: "card".to_string(),
                summary: "issued card_1".to_string(),
                at_unix: 0,
            };
            Ok(Response::new(Box::pin(tokio_stream::once(Ok(event)))))
        }
    }

    /// Spins up `gw` as an in-process gRPC server over an in-memory duplex
    /// pipe (no TCP port, no network) and returns a `Client` connected to
    /// it — this crate's equivalent of sdk/go/ration's bufconn helper.
    async fn test_client(gw: FakeGateway) -> Client {
        let (client_io, server_io) = duplex(1024 * 1024);

        tokio::spawn(async move {
            Server::builder()
                .add_service(GatewayServer::new(gw))
                .serve_with_incoming(tokio_stream::once(Ok::<_, std::io::Error>(server_io)))
                .await
                .expect("fake gateway server failed");
        });

        let mut client_io = Some(client_io);
        let channel = Endpoint::try_from("http://[::]:0")
            .expect("static in-process endpoint")
            .connect_with_connector(service_fn(move |_: Uri| {
                let io = client_io
                    .take()
                    .expect("test_client called more than once per duplex pipe");
                async move { Ok::<_, std::io::Error>(TokioIo::new(io)) }
            }))
            .await
            .expect("connect to in-process fake gateway");

        Client::from_channel(channel)
    }

    #[tokio::test]
    async fn issue_card_returns_the_fake_servers_card() {
        let mut client = test_client(FakeGateway).await;
        let card = client
            .issue_card("tenant_1", "Jane Doe", 10_000, "monthly", "USD")
            .await
            .expect("issue_card");
        assert_eq!(card.id, "card_1");
        assert_eq!(card.tenant_id, "tenant_1");
        assert_eq!(card.cardholder_name, "Jane Doe");
        assert_eq!(card.limit_minor_units, 10_000);
    }

    #[tokio::test]
    async fn adjust_card_limit_returns_the_new_limit() {
        let mut client = test_client(FakeGateway).await;
        let card = client
            .adjust_card_limit("card_9", 5_000, "customer requested increase")
            .await
            .expect("adjust_card_limit");
        assert_eq!(card.id, "card_9");
        assert_eq!(card.limit_minor_units, 5_000);
    }

    #[tokio::test]
    async fn get_card_round_trips_the_id() {
        let mut client = test_client(FakeGateway).await;
        let card = client.get_card("card_42").await.expect("get_card");
        assert_eq!(card.id, "card_42");
    }

    #[tokio::test]
    async fn list_cards_returns_every_card_for_the_tenant() {
        let mut client = test_client(FakeGateway).await;
        let cards = client.list_cards("tenant_7").await.expect("list_cards");
        assert_eq!(cards.len(), 2);
        assert_eq!(cards[0].tenant_id, "tenant_7");
    }

    #[tokio::test]
    async fn request_bridge_funding_surfaces_partial_grants() {
        let mut client = test_client(FakeGateway).await;
        let result = client
            .request_bridge_funding("tenant_1", 1_000, "card settlement due")
            .await
            .expect("request_bridge_funding");
        assert_eq!(result.granted_minor_units, 500);
        assert!(!result.fully_granted);
        assert!(!result.explanation.is_empty());
    }

    #[tokio::test]
    async fn simulate_deposit_rejects_an_empty_tenant_id() {
        let mut client = test_client(FakeGateway).await;
        let id = client
            .simulate_deposit("tenant_1", 25_000, 0, false)
            .await
            .expect("simulate_deposit");
        assert_eq!(id, "dep_1");

        let err = client
            .simulate_deposit("", 25_000, 0, false)
            .await
            .expect_err("empty tenant_id should be rejected");
        assert!(matches!(err, ClientError::Rpc(_)));
    }

    #[tokio::test]
    async fn stream_activity_yields_the_fake_servers_event() {
        use tokio_stream::StreamExt;

        let mut client = test_client(FakeGateway).await;
        let mut stream = client
            .stream_activity("tenant_1")
            .await
            .expect("stream_activity");
        let event = stream
            .next()
            .await
            .expect("stream yielded no items")
            .expect("stream yielded an error");
        assert_eq!(event.id, "evt_1");
        assert_eq!(event.summary, "issued card_1");
    }
}
