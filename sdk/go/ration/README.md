# ration (Go SDK)

A typed Go client for Ration's [`gateway.proto`](../../../proto/gateway.proto)
`Gateway` service — the same RPC surface Ration's own TUI and CLI use, now
packaged for a platform customer to import directly.

```go
import "github.com/caelum0x/ration/sdk/go/ration"

client, err := ration.Dial("localhost:7002")
if err != nil {
	log.Fatal(err)
}
defer client.Close()

ctx := context.Background()

card, err := client.IssueCard(ctx, "tenant_1", "Jane Doe", 10_00, "monthly", "USD")
if err != nil {
	log.Fatal(err)
}
fmt.Println("issued", card.GetId())

result, err := client.RequestBridgeFunding(ctx, "tenant_1", 5_00, "card settlement due")
if err != nil {
	log.Fatal(err)
}
fmt.Printf("granted %d of %d requested: %s\n", result.GrantedMinorUnits, 5_00, result.Explanation)

events, errs, err := client.StreamActivity(ctx, "tenant_1")
if err != nil {
	log.Fatal(err)
}
for ev := range events {
	fmt.Println(ev.GetKind(), ev.GetSummary())
}
if err := <-errs; err != nil {
	log.Println("stream ended with:", err)
}
```

`Dial` connects in plaintext by default. For mTLS against a production
deployment (see `pkg/mtls`, `infra/dev-ca/`), pass your own transport
credentials:

```go
creds, _ := credentials.NewClientTLSFromFile("ca.pem", "gateway")
client, err := ration.Dial("gateway.example.com:7002",
	ration.WithDialOptions(grpc.WithTransportCredentials(creds)))
```

`Card`, `ActivityEvent`, and `CardStatus` are re-exported directly from the
generated protobuf types (`pkg/pb/gateway`, `pkg/pb/common`) — this SDK adds
ergonomic constructors and typed method signatures on top of the gRPC stubs,
it does not reinvent the wire format.

REST is also available as a convenience facade over this same gRPC service —
see [`../../../services/gateway/internal/httpapi`](../../../services/gateway/internal/httpapi)
— for callers who'd rather `curl` than link a gRPC client.
