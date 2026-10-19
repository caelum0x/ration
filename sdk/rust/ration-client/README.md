# ration-client

A typed Rust client for Ration's [`gateway.proto`](../../../proto/gateway.proto)
`Gateway` service — the Rust counterpart to
[`sdk/go/ration`](../../go/ration).

```rust
let mut client = ration_client::Client::connect("http://localhost:7002").await?;

let card = client
    .issue_card("tenant_1", "Jane Doe", 1_000_00, "monthly", "USD")
    .await?;
println!("issued {}", card.id);

let result = client
    .request_bridge_funding("tenant_1", 500_00, "card settlement due")
    .await?;
println!(
    "granted {} of 50000 requested: {}",
    result.granted_minor_units, result.explanation
);

use tokio_stream::StreamExt;
let mut events = client.stream_activity("tenant_1").await?;
while let Some(event) = events.next().await {
    let event = event?;
    println!("{} {}", event.kind, event.summary);
}
```

`Client::connect` dials in plaintext by default. For mTLS against a
production deployment (see `pkg/mtls`, `infra/dev-ca/`), build your own
`tonic::transport::Channel` with `ClientTlsConfig` and construct via
`Client::from_channel`.

`Card` and `ActivityEvent` are re-exported directly from the generated
protobuf types — this crate adds ergonomic, typed method signatures on top
of the tonic-generated client, it does not reinvent the wire format.

REST is also available as a convenience facade over this same gRPC service —
see
[`../../../services/gateway/internal/httpapi`](../../../services/gateway/internal/httpapi)
— for callers who'd rather `curl` than link a gRPC client.
