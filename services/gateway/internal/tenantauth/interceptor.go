// Package tenantauth closes docs/THREAT-MODEL.md's "Known gaps" item
// 1 — "no tenant-scoped authorization anywhere, still" — on gateway's
// unary gRPC surface. Every RPC that carries a tenant_id field (every
// gateway.proto request except GetCard/AdjustCardLimit, which key off
// card_id instead and are resolved via internal/cardtenant — see
// requestCardID below) previously trusted whatever tenant_id the caller
// supplied, with nothing checking that the caller was actually
// authorized to act as that tenant. services/auth's VerifyAPIKey/
// VerifyJWT RPCs have existed and been unit-tested since the section 1
// split, but — per the threat model's own words — "nothing calls them
// yet." This package is that caller.
//
// Enforcement is opt-in via RATION_GATEWAY_REQUIRE_TENANT_AUTH (see
// internal/config) — the same opt-in pattern this repo already uses for
// mTLS (pkg/mtls's <PREFIX>_MTLS_ENABLED). services/cli, services/tui
// and services/agent now each mint an operator-scoped credential at
// startup (see each service's internal/authclient and
// proto/auth.proto's is_operator field) and attach it to every gateway
// call, so RATION_GATEWAY_REQUIRE_TENANT_AUTH's default is now true —
// a bare `make run-demo` is a credentialed demo, not an
// all-requests-rejected one.
package tenantauth

import (
	"context"

	"github.com/caelum0x/ration/services/gateway/internal/cardtenant"
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
//
// isOperator mirrors proto/auth.proto's is_operator field: a credential
// minted with is_operator=true authenticates as a trusted cross-tenant
// caller (the CLI/TUI operator dashboards, the agent's polling loop)
// rather than one specific tenant_id — see UnaryServerInterceptor's doc
// comment for exactly how that changes the match below.
type Verifier interface {
	VerifyAPIKey(ctx context.Context, secret string) (tenantID string, valid bool, isOperator bool, reason string, err error)
	VerifyJWT(ctx context.Context, token string) (tenantID string, valid bool, isOperator bool, reason string, err error)
}

// tenantIDFieldName is the proto field name every gateway.proto request
// scoped to a single tenant uses (see proto/gateway.proto and
// internal/ratelimit's identical convention, which this mirrors on
// purpose — both packages extract the same field for different
// reasons). Extracted via reflection rather than a type switch per RPC
// so a newly added request message is automatically covered the moment
// it adds a tenant_id field, with zero changes here.
const tenantIDFieldName = "tenant_id"

// cardIDFieldName is the proto field name GetCard/AdjustCardLimit carry
// instead of tenant_id (see requestTenantID's doc comment and
// internal/cardtenant, which resolves this to the owning tenant_id).
const cardIDFieldName = "card_id"

// requestTenantID returns the tenant_id a request message carries, and
// whether it carries one at all. Requests that key off card_id instead
// (GetCard/AdjustCardLimit) are NOT resolved here — see requestCardID
// and UnaryServerInterceptor, which resolves those via
// internal/cardtenant before applying the identical credential check
// this function's callers use for every other RPC.
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

// requestCardID returns the card_id a request message carries (and
// whether it carries one at all), the same reflection-based pattern as
// requestTenantID. Only GetCardRequest/AdjustCardLimitRequest have this
// field today, but — like requestTenantID — this covers any future
// request message that adds one with zero changes here.
func requestCardID(req any) (string, bool) {
	m, ok := req.(proto.Message)
	if !ok {
		return "", false
	}
	fd := m.ProtoReflect().Descriptor().Fields().ByName(cardIDFieldName)
	if fd == nil {
		return "", false
	}
	cardID := m.ProtoReflect().Get(fd).String()
	return cardID, cardID != ""
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
// names as the thing nothing checks today.
//
// Requests that carry a tenant_id field (see requestTenantID) are
// checked against it directly. GetCard/AdjustCardLimit, which carry
// only a card_id, are resolved to their owning tenant_id via
// cardResolver (see internal/cardtenant) first, then checked
// identically — closing docs/THREAT-MODEL.md item 1(a)'s named blind
// spot instead of leaving those two RPCs permanently unscoped.
// cardResolver may be nil (e.g. in tests that don't exercise this path,
// or a deployment that genuinely has no card-issuance to resolve
// against); a nil resolver, or a request with neither field, passes
// through unchanged exactly as before — this interceptor only ever
// narrows what a request it can actually scope to a tenant can do, it
// never widens or replaces any other check already in place (rate
// limiting, mTLS, etc).
func UnaryServerInterceptor(v Verifier, required bool, cardResolver cardtenant.Resolver) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !required {
			return handler(ctx, req)
		}

		tenantID, scoped := requestTenantID(req)
		if !scoped {
			cardID, hasCardID := requestCardID(req)
			if !hasCardID || cardResolver == nil {
				// Not a tenant-scoped RPC by this project's own
				// definition, and no way (or no configured way) to
				// resolve one — nothing here to authorize.
				return handler(ctx, req)
			}
			resolved, err := cardResolver.ResolveTenantID(ctx, cardID)
			if err != nil {
				// An unresolvable card_id must never silently fall back
				// to "unscoped, so let it through" — see
				// internal/cardtenant's doc comment.
				return nil, status.Errorf(codes.PermissionDenied, "tenantauth: could not resolve tenant for card_id %q: %v", cardID, err)
			}
			tenantID = resolved
		}

		verifiedTenant, isOperator, err := authenticate(ctx, v, info.FullMethod)
		if err != nil {
			return nil, err
		}

		// An operator-scoped credential (proto/auth.proto's is_operator)
		// is pre-authorized for every tenant_id — this is the explicit,
		// narrow exception that lets the CLI/TUI operator dashboards and
		// the agent's polling loop (all of which legitimately act across
		// every tenant, not one) keep working once tenant-scoped
		// authorization is enforced. It is still authenticated — only
		// the per-tenant match below is skipped.
		if isOperator {
			return handler(ctx, req)
		}

		// An authenticated, non-operator caller presenting no tenant_id
		// of their own (verifiedTenant == "") can't happen for a
		// correctly-issued credential (services/auth always binds a
		// non-operator API key/JWT to a tenant_id at mint time) but is
		// treated as a mismatch, not a pass, out of caution — never let
		// an edge case in a future auth change silently become
		// "authenticated as every tenant."
		if verifiedTenant == "" || verifiedTenant != tenantID {
			return nil, status.Errorf(codes.PermissionDenied, "tenantauth: credential is not authorized for tenant_id %q", tenantID)
		}

		return handler(ctx, req)
	}
}

// authenticate extracts and verifies whichever credential ctx carries,
// shared by both UnaryServerInterceptor and StreamServerInterceptor.
// Returns the credential's verified tenant_id (empty for an operator
// credential) and whether it is operator-scoped, or a ready-to-return
// gRPC status error (Unauthenticated) on any failure.
func authenticate(ctx context.Context, v Verifier, fullMethod string) (tenantID string, isOperator bool, err error) {
	apiKey, bearer, present := credentialFromContext(ctx)
	if !present {
		return "", false, status.Errorf(codes.Unauthenticated, "tenantauth: %s requires an %s or %s credential", fullMethod, apiKeyHeader, authorizationHeader)
	}

	var verifiedTenant string
	var valid bool
	var reason string
	var verifyErr error
	switch {
	case apiKey != "":
		verifiedTenant, valid, isOperator, reason, verifyErr = v.VerifyAPIKey(ctx, apiKey)
	default:
		verifiedTenant, valid, isOperator, reason, verifyErr = v.VerifyJWT(ctx, bearer)
	}
	if verifyErr != nil {
		return "", false, status.Errorf(codes.Unauthenticated, "tenantauth: credential verification failed: %v", verifyErr)
	}
	if !valid {
		return "", false, status.Errorf(codes.Unauthenticated, "tenantauth: invalid credential: %s", reason)
	}
	return verifiedTenant, isOperator, nil
}

// StreamServerInterceptor is UnaryServerInterceptor's counterpart for
// gateway.proto's one streaming RPC, StreamActivity
// (docs/THREAT-MODEL.md's "Known gaps" item 1(b)): before this existed,
// StreamActivityRequest.tenant_id being allowed to mean "all tenants"
// when empty (see internal/activity.Bus's doc comment) was reachable by
// ANY caller, authenticated or not — there was no check at all, unlike
// every unary RPC this package already covers.
//
// A non-operator credential may only subscribe to its OWN tenant_id —
// an empty (all-tenants) subscription is rejected for it, not silently
// narrowed, so a caller never gets a response shaped differently than
// what it asked for. An operator credential (the CLI/TUI operator
// dashboards, which is the only caller that legitimately needs the
// all-tenants view) may subscribe to any tenant_id, including empty.
func StreamServerInterceptor(v Verifier, required bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !required {
			return handler(srv, ss)
		}

		verifiedTenant, isOperator, err := authenticate(ss.Context(), v, info.FullMethod)
		if err != nil {
			return err
		}

		wrapped := &tenantScopedServerStream{
			ServerStream: ss,
			verify: func(requestedTenantID string) error {
				if isOperator {
					return nil
				}
				if requestedTenantID == "" {
					return status.Errorf(codes.PermissionDenied, "tenantauth: %s requires a non-empty tenant_id for a non-operator credential", info.FullMethod)
				}
				if requestedTenantID != verifiedTenant {
					return status.Errorf(codes.PermissionDenied, "tenantauth: credential is not authorized for tenant_id %q", requestedTenantID)
				}
				return nil
			},
		}
		return handler(srv, wrapped)
	}
}

// tenantScopedServerStream wraps a grpc.ServerStream so that the first
// (and, for a server-streaming RPC, only) client message received is
// checked by verify immediately after being decoded — before the
// wrapped handler acts on it or sends back a single byte — rather than
// requiring every StreamActivity-shaped handler to remember to call some
// separate check itself.
type tenantScopedServerStream struct {
	grpc.ServerStream
	verify func(requestedTenantID string) error
}

func (s *tenantScopedServerStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	tenantID, _ := requestTenantID(m)
	return s.verify(tenantID)
}
