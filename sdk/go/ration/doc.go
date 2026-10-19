// Package ration is a thin, typed Go client for Ration's gateway
// service (docs/SCALE-EXPANSION.md section 7).
//
// gateway.proto (package ration.gateway.v1) is the source of truth for
// every RPC this client exposes — see docs/adr/0003 for why gRPC, not
// REST, is the source of truth, and
// github.com/caelum0x/ration/pkg/pb/gateway for the generated stubs
// this package wraps. The gateway's REST/JSON facade
// (services/gateway/internal/httpapi) is a convenience translation
// layer over the exact same gRPC surface this SDK talks to directly;
// a Go consumer should prefer this package over the REST facade for
// the usual reasons (no JSON round-trip, native streaming for
// StreamActivity, compile-time checked request/response types).
//
// Typical use:
//
//	client, err := ration.Dial("gateway.example.com:7002")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close()
//
//	card, err := client.IssueCard(ctx, "tenant_1", "Ada Lovelace", 500_00, "monthly", "USD")
//
// Every method accepts a context.Context for cancellation/deadlines and
// returns the generated protobuf response type directly (re-exported
// from this package as a type alias, e.g. ration.Card) so callers never
// need to import github.com/caelum0x/ration/pkg/pb/gateway themselves.
package ration
