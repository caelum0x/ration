// Package operatorauth closes a real gap in docs/THREAT-MODEL.md's
// "Known gaps" item 1 that the item's own 2026-10-28/29 "Fixed" update
// never actually covered: tenant-registry's own inbound gRPC server
// installed no authentication check at all. services/gateway's
// internal/tenantauth and services/ledger-core/src/tenantauth.rs both
// got real tenant-scoped checks in that round, but
// CreateTenant/GetTenant/ListTenants (tenant-registry's entire RPC
// surface) have no tenant_id to check a caller against — they're
// operator-only by nature, so they were never in scope for either of
// those two checks, and nothing filled the gap for them. The result,
// verified live in docker-compose.yml (tenant-registry's gRPC port is
// published to the host, "7011:7011", binding 0.0.0.0 inside the
// container): ANY caller that can reach that port — no API key, no
// JWT, nothing — could create unlimited platform tenants or list every
// tenant on the platform (cross-tenant name/currency/reserve-floor
// disclosure), because tenant-registry mints and attaches its own
// pre-provisioned operator credential to every ledger-core call it
// makes on the caller's behalf, regardless of who the caller actually
// is. This package requires the caller to present a valid
// operator-scoped credential of their own before any RPC is allowed to
// proceed, mirroring gateway's internal/tenantauth credential-handling
// (same two headers, same auth.VerifyAPIKey/VerifyJWT round-trip) but
// simplified for the operator-only case: there is no tenant_id to
// match, so a verified, non-operator credential is rejected exactly
// like no credential at all.
package operatorauth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// apiKeyHeader/authorizationHeader/bearerPrefix match
// services/gateway/internal/tenantauth's identical constants exactly —
// a credential minted by `ration apikey issue --operator` or `ration`'s
// own startup MintOperatorJWT call must be presentable under the same
// headers everywhere in this project.
const (
	apiKeyHeader        = "x-api-key"
	authorizationHeader = "authorization"
	bearerPrefix        = "Bearer "
)

// Verifier is the subset of authclient.Client this package needs — a
// narrow interface so tests can supply a fake without standing up a
// real gRPC server to auth.
type Verifier interface {
	VerifyAPIKey(ctx context.Context, secret string) (tenantID string, valid bool, isOperator bool, reason string, err error)
	VerifyJWT(ctx context.Context, token string) (tenantID string, valid bool, isOperator bool, reason string, err error)
}

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
// credential doesn't verify (Unauthenticated), or it verifies but is
// not operator-scoped (PermissionDenied) — every RPC
// tenant_registry.proto defines is operator-only by nature (tenant
// provisioning and cross-tenant listing), so there is no narrower,
// per-tenant match to make the way gateway's equivalent interceptor
// does.
func UnaryServerInterceptor(v Verifier, required bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !required {
			return handler(ctx, req)
		}

		apiKey, bearer, present := credentialFromContext(ctx)
		if !present {
			return nil, status.Errorf(codes.Unauthenticated, "operatorauth: %s requires an %s or %s credential", info.FullMethod, apiKeyHeader, authorizationHeader)
		}

		var isOperator, valid bool
		var reason string
		var verifyErr error
		if apiKey != "" {
			_, valid, isOperator, reason, verifyErr = v.VerifyAPIKey(ctx, apiKey)
		} else {
			_, valid, isOperator, reason, verifyErr = v.VerifyJWT(ctx, bearer)
		}
		if verifyErr != nil {
			return nil, status.Errorf(codes.Unauthenticated, "operatorauth: credential verification failed: %v", verifyErr)
		}
		if !valid {
			return nil, status.Errorf(codes.Unauthenticated, "operatorauth: invalid credential: %s", reason)
		}
		if !isOperator {
			return nil, status.Errorf(codes.PermissionDenied, "operatorauth: %s requires an operator-scoped credential", info.FullMethod)
		}

		return handler(ctx, req)
	}
}
