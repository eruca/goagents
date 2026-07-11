package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCApprovalAuthenticatorUsesVerifiedSubject(t *testing.T) {
	provider := newOIDCTestProvider(t)
	authenticator, err := NewOIDCApprovalAuthenticator(context.Background(), provider.issuer, "host-api")
	if err != nil {
		t.Fatalf("NewOIDCApprovalAuthenticator: %v", err)
	}

	identity, err := authenticator.AuthenticateApproval(context.Background(), "Bearer "+provider.mintToken(t, "operator-1", "host-api", time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("AuthenticateApproval: %v", err)
	}
	if identity.Subject != "operator-1" {
		t.Fatalf("subject = %q, want operator-1", identity.Subject)
	}
}

func TestOIDCApprovalAuthenticatorRejectsInvalidBearerClaims(t *testing.T) {
	provider := newOIDCTestProvider(t)
	authenticator, err := NewOIDCApprovalAuthenticator(context.Background(), provider.issuer, "host-api")
	if err != nil {
		t.Fatalf("NewOIDCApprovalAuthenticator: %v", err)
	}
	cases := []struct {
		name          string
		authorization string
	}{
		{name: "malformed", authorization: "Token value"},
		{name: "wrong audience", authorization: "Bearer " + provider.mintToken(t, "operator-1", "other-api", time.Now().Add(time.Hour))},
		{name: "expired", authorization: "Bearer " + provider.mintToken(t, "operator-1", "host-api", time.Now().Add(-time.Minute))},
		{name: "missing subject", authorization: "Bearer " + provider.mintToken(t, "", "host-api", time.Now().Add(time.Hour))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := authenticator.AuthenticateApproval(context.Background(), tc.authorization)
			if !errors.Is(err, ErrApprovalUnauthorized) {
				t.Fatalf("AuthenticateApproval error = %v, want ErrApprovalUnauthorized", err)
			}
		})
	}
}

type oidcTestProvider struct {
	issuer     string
	privateKey *rsa.PrivateKey
}

func newOIDCTestProvider(t *testing.T) oidcTestProvider {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	provider := oidcTestProvider{privateKey: privateKey}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeOIDCTestJSON(t, w, map[string]any{
				"issuer":   provider.issuer,
				"jwks_uri": provider.issuer + "/keys",
			})
		case "/keys":
			publicKey := privateKey.PublicKey
			writeOIDCTestJSON(t, w, map[string]any{
				"keys": []map[string]any{{
					"kty": "RSA",
					"kid": "test-key",
					"use": "sig",
					"alg": "RS256",
					"n":   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	provider.issuer = server.URL
	return provider
}

func writeOIDCTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode OIDC response: %v", err)
	}
}

func (p oidcTestProvider) mintToken(t *testing.T, subject, audience string, expiresAt time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"iss": p.issuer,
		"sub": subject,
		"aud": audience,
		"iat": time.Now().Unix(),
		"exp": expiresAt.Unix(),
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCApprovalAuthenticatorRejectsBlankConfiguration(t *testing.T) {
	_, err := NewOIDCApprovalAuthenticator(context.Background(), "", "host-api")
	if !errors.Is(err, ErrInvalidOIDCApprovalConfig) {
		t.Fatalf("blank issuer error = %v, want ErrInvalidOIDCApprovalConfig", err)
	}
	_, err = NewOIDCApprovalAuthenticator(context.Background(), "https://issuer.example", "")
	if !errors.Is(err, ErrInvalidOIDCApprovalConfig) {
		t.Fatalf("blank audience error = %v, want ErrInvalidOIDCApprovalConfig", err)
	}
}

func TestLoadOIDCApprovalAuthenticatorRequiresIssuerAndAudience(t *testing.T) {
	_, err := loadOIDCApprovalAuthenticator(context.Background(), func(string) string { return "" })
	if !errors.Is(err, ErrInvalidOIDCApprovalConfig) {
		t.Fatalf("load OIDC configuration error = %v, want ErrInvalidOIDCApprovalConfig", err)
	}
}
