// Package tenantauth closes docs/THREAT-MODEL.md's "Known gaps" item
// 1 — "no tenant-scoped authorization anywhere, still" — on gateway's
// unary gRPC surface. Every RPC that carries a tenant_id field (every
// gateway.proto request except GetCard/AdjustCardLimit, which key off
// card_id instead — see requestTenantID below) previously trusted
// whatever tenant_id the caller supplied, with nothing checking that
// the caller was actually authorized to act as that tenant.
// services/auth's VerifyAPIKey/VerifyJWT RPCs have existed and been
// unit-tested since the section 1 split, but — per the threat model's
// own words — "nothing calls them yet." This package is that caller.
//
// Enforcement is opt-in via RATION_GATEWAY_REQUIRE_TENANT_AUTH (see
// internal/config), off by default — the same opt-in pattern this repo
// already uses for mTLS (pkg/mtls's <PREFIX>_MTLS_ENABLED). It defaults
// off because the demo/CLI/TUI flows do not yet present a credential on
// every call (services/cli, services/tui dial gateway directly with no
// x-api-key/authorization metadata) — flipping the default on today
// would turn `make run-demo` into an all-requests-rejected demo, not a
// safer one. The precise residual TODO once this lands: wire
// services/cli and services/tui to call auth.IssueAPIKey (or MintJWT)
// once and send the result as metadata on every gateway call, then
// flip this on by default for anything that isn't a local demo.
package tenantauth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// apiKeyHeader/authorizationHeader are the two metadata keys a caller
// may present a credential under — an API key (services/auth's
// IssueAPIKey output, meant for long-lived machine callers like the
// agent) or a bearer JWT (services/auth's MintJWT output, meant for a
// short-lived human-operator session via the CLI/TUI). Exactly one is
// required when enforcement is on; a caller presenting neither is
// Unauthenticated, not merely unscoped.
const (
	apiKeyHeader        = "x-api-key"
	authorizationHeader = "authorization"
	bearerPrefix        = "Bearer "
)

// Verifier is the subset of authclient.Client this package needs — a
// narrow interface so tests can supply a fake without standing up a
// real gRPC server to auth.
type Verifier interface {
	VerifyAPIKey(ctx context.Context, secret string) (tenantID string, valid bool, reason string, err error)
	VerifyJWT(ctx context.Context, token string) (tenantID string, valid bool, reason string, err error)
}

// tenantIDFieldName is the proto field name every gateway.proto request
// scoped to a single tenant uses (see proto/gateway.proto and
// internal/ratelimit's identical convention, which this mirrors on
// purpose — both packages extract the same field for different
// reasons). Extracted via reflection rather than a type switch per RPC
// so a newly added request message is automatically covered the moment
// it adds a tenant_id field, with zero changes here.
const tenantIDFieldName = "tenant_id"

// requestTenantID returns the tenant_id a request message carries, and
// whether it carries one at all. GetCard/AdjustCardLimit key off
// card_id instead (the same documented gap internal/ratelimit's keyFor
// has) — this package deliberately does not resolve card_id -> tenant_id
// here, for the identical reason ratelimit's doc comment gives: it
// would require a ledger/card-issuance lookup on every call just to
// decide whether to enforce, turning an auth check into an extra RPC on
// every request. Those two RPCs remain an open, named gap (see this
// package's doc comment and docs/THREAT-MODEL.md item 1's own
// "Precise TODO").
func requestTenantID(req any) (string, bool) {
	m, ok := req.(proto.Message)
	if !ok {
		return "", false
	}
	fd := m.ProtoReflect().Descriptor().Fields().ByName(tenantIDFieldName)
	if fd == nil {
		return "", false
	}
	return m.ProtoReflect().Get(fd).String(), true
}

// credentialFromContext extracts whichever of the two supported
// credential headers is present. Presenting both is not an error; the
// API key is checked first (it's the more common machine-caller case
// in this project's demo — the agent).
func credentialFromContext(ctx context.Context) (apiKey, bearer string, present bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", "", false
	}
	if vals := md.Get(apiKeyHeader); len(vals) > 0 && vals[0] != "" {
		apiKey = vals[0]
		present = true
	}
	if vals := md.Get(authorizationHeader); len(vals) > 0 {
		for _, v := range vals {
			if len(v) > len(bearerPrefix) && v[:len(bearerPrefix)] == bearerPrefix {
				bearer = v[len(bearerPrefix):]
				present = true
				break
			}
		}
	}
	return apiKey, bearer, present
}

// UnaryServerInterceptor rejects a unary RPC when required is true and
// either: no credential is presented (Unauthenticated), the presented
// credential doesn't verify (Unauthenticated), or it verifies for a
// tenant_id that doesn't match the request's own tenant_id
// (PermissionDenied) — the exact mismatch docs/THREAT-MODEL.md item 1
// names as the thing nothing checks today. Requests that don't carry a
// tenant_id field at all (see requestTenantID) pass through unchanged:
// this interceptor only ever narrows what a *tenant-scoped* call can
// do, it never widens or replaces any other check already in place
// (rate limiting, mTLS, etc).
func UnaryServerInterceptor(v Verifier, required bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !required {
			return handler(ctx, req)
		}

		tenantID, scoped := requestTenantID(req)
		if !scoped {
			// Not a tenant-scoped RPC by this project's own definition
			// (GetCard/AdjustCardLimit) — nothing here to authorize.
			return handler(ctx, req)
		}

		apiKey, bearer, present := credentialFromContext(ctx)
		if !present {
			return nil, status.Errorf(codes.Unauthenticated, "tenantauth: %s requires an %s or %s credential", info.FullMethod, apiKeyHeader, authorizationHeader)
		}

		var verifiedTenant string
		var valid bool
		var reason string
		var err error
		switch {
		case apiKey != "":
			verifiedTenant, valid, reason, err = v.VerifyAPIKey(ctx, apiKey)
		default:
			verifiedTenant, valid, reason, err = v.VerifyJWT(ctx, bearer)
		}
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "tenantauth: credential verification failed: %v", err)
		}
		if !valid {
			return nil, status.Errorf(codes.Unauthenticated, "tenantauth: invalid credential: %s", reason)
		}

		// An authenticated caller presenting no tenant_id of their own
		// (verifiedTenant == "") can't happen for a correctly-issued
		// credential (services/auth always binds an API key/JWT to a
		// tenant_id at mint time) but is treated as a mismatch, not a
		// pass, out of caution — never let an edge case in a future
		// auth change silently become "authenticated as every tenant."
		if verifiedTenant == "" || verifiedTenant != tenantID {
			return nil, status.Errorf(codes.PermissionDenied, "tenantauth: credential is not authorized for tenant_id %q", tenantID)
		}

		return handler(ctx, req)
	}
}
