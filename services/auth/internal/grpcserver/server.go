// Package grpcserver implements auth.proto's Auth service by delegating
// to internal/apikey, internal/jwtauth and internal/servicecertca — this
// package is the one place that translates between wire types
// (pkg/pb/auth) and those packages' Go-native APIs.
package grpcserver

import (
	"context"
	"time"

	authpb "github.com/caelum0x/ration/pkg/pb/auth"
	"github.com/caelum0x/ration/services/auth/internal/apikey"
	"github.com/caelum0x/ration/services/auth/internal/jwtauth"
	"github.com/caelum0x/ration/services/auth/internal/servicecertca"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements authpb.AuthServer.
type Server struct {
	authpb.UnimplementedAuthServer

	keys *apikey.Store
	jwt  *jwtauth.Signer
	ca   *servicecertca.CA
}

// New constructs a Server wiring the given sub-services.
func New(keys *apikey.Store, jwt *jwtauth.Signer, ca *servicecertca.CA) *Server {
	return &Server{keys: keys, jwt: jwt, ca: ca}
}

// IssueAPIKey implements authpb.AuthServer.
func (s *Server) IssueAPIKey(ctx context.Context, req *authpb.IssueAPIKeyRequest) (*authpb.IssueAPIKeyResponse, error) {
	keyID, secret, createdAt, err := s.keys.Issue(req.GetTenantId(), req.GetName(), req.GetIsOperator())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &authpb.IssueAPIKeyResponse{
		KeyId:         keyID,
		TenantId:      req.GetTenantId(),
		Secret:        secret,
		CreatedAtUnix: createdAt,
		IsOperator:    req.GetIsOperator(),
	}, nil
}

// VerifyAPIKey implements authpb.AuthServer.
func (s *Server) VerifyAPIKey(ctx context.Context, req *authpb.VerifyAPIKeyRequest) (*authpb.VerifyAPIKeyResponse, error) {
	result := s.keys.Verify(req.GetSecret())
	return &authpb.VerifyAPIKeyResponse{
		Valid:      result.Valid,
		KeyId:      result.KeyID,
		TenantId:   result.TenantID,
		Reason:     result.Reason,
		IsOperator: result.IsOperator,
	}, nil
}

// RevokeAPIKey implements authpb.AuthServer.
func (s *Server) RevokeAPIKey(ctx context.Context, req *authpb.RevokeAPIKeyRequest) (*authpb.RevokeAPIKeyResponse, error) {
	if err := s.keys.Revoke(req.GetKeyId()); err != nil {
		if err == apikey.ErrNotFound {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &authpb.RevokeAPIKeyResponse{Revoked: true}, nil
}

// MintJWT implements authpb.AuthServer.
func (s *Server) MintJWT(ctx context.Context, req *authpb.MintJWTRequest) (*authpb.MintJWTResponse, error) {
	token, expiresAt, err := s.jwt.Mint(req.GetSubject(), req.GetTenantId(), time.Duration(req.GetTtlSeconds())*time.Second, req.GetIsOperator())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &authpb.MintJWTResponse{Token: token, ExpiresAtUnix: expiresAt.Unix()}, nil
}

// VerifyJWT implements authpb.AuthServer.
func (s *Server) VerifyJWT(ctx context.Context, req *authpb.VerifyJWTRequest) (*authpb.VerifyJWTResponse, error) {
	result := s.jwt.Verify(req.GetToken())
	return &authpb.VerifyJWTResponse{
		Valid:      result.Valid,
		Subject:    result.Subject,
		TenantId:   result.TenantID,
		Reason:     result.Reason,
		IsOperator: result.IsOperator,
	}, nil
}

// IssueServiceCert implements authpb.AuthServer.
func (s *Server) IssueServiceCert(ctx context.Context, req *authpb.IssueServiceCertRequest) (*authpb.IssueServiceCertResponse, error) {
	certPEM, expiresAt, err := s.ca.SignCSR(req.GetCsrPem(), req.GetServiceName(), time.Duration(req.GetTtlSeconds())*time.Second)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &authpb.IssueServiceCertResponse{
		CertPem:       certPEM,
		CaPem:         s.ca.CAPEM(),
		ExpiresAtUnix: expiresAt.Unix(),
	}, nil
}
