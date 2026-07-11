package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	ErrApprovalUnauthorized      = errors.New("approval authentication failed")
	ErrInvalidOIDCApprovalConfig = errors.New("invalid OIDC approval configuration")
)

const (
	oidcIssuerEnv   = "HOST_API_OIDC_ISSUER"
	oidcAudienceEnv = "HOST_API_OIDC_AUDIENCE"
)

// ApprovalIdentity is the verified identity allowed to approve a workflow.
// Subject is copied from the OIDC subject claim for audit metadata.
type ApprovalIdentity struct {
	Subject string
}

// ApprovalAuthenticator validates an HTTP authorization value at the host boundary.
type ApprovalAuthenticator interface {
	AuthenticateApproval(ctx context.Context, authorization string) (ApprovalIdentity, error)
}

// OIDCApprovalAuthenticator verifies bearer tokens through the provider's discovery document and JWKS.
type OIDCApprovalAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

func NewOIDCApprovalAuthenticator(ctx context.Context, issuer, audience string) (*OIDCApprovalAuthenticator, error) {
	issuer = strings.TrimSpace(issuer)
	audience = strings.TrimSpace(audience)
	if issuer == "" || audience == "" {
		return nil, ErrInvalidOIDCApprovalConfig
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC provider: %w", err)
	}
	return &OIDCApprovalAuthenticator{
		verifier: provider.Verifier(&oidc.Config{ClientID: audience}),
	}, nil
}

func loadOIDCApprovalAuthenticator(ctx context.Context, getenv func(string) string) (*OIDCApprovalAuthenticator, error) {
	return NewOIDCApprovalAuthenticator(ctx, getenv(oidcIssuerEnv), getenv(oidcAudienceEnv))
}

func (a *OIDCApprovalAuthenticator) AuthenticateApproval(ctx context.Context, authorization string) (ApprovalIdentity, error) {
	if a == nil || a.verifier == nil {
		return ApprovalIdentity{}, ErrApprovalUnauthorized
	}
	token := strings.TrimSpace(authorization)
	if !strings.HasPrefix(token, "Bearer ") {
		return ApprovalIdentity{}, ErrApprovalUnauthorized
	}
	rawToken := strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if rawToken == "" {
		return ApprovalIdentity{}, ErrApprovalUnauthorized
	}
	verified, err := a.verifier.Verify(ctx, rawToken)
	if err != nil || strings.TrimSpace(verified.Subject) == "" {
		return ApprovalIdentity{}, ErrApprovalUnauthorized
	}
	return ApprovalIdentity{Subject: verified.Subject}, nil
}

type rejectingApprovalAuthenticator struct{}

func (rejectingApprovalAuthenticator) AuthenticateApproval(context.Context, string) (ApprovalIdentity, error) {
	return ApprovalIdentity{}, ErrApprovalUnauthorized
}
