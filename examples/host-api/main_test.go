package main

import (
	"context"
	"errors"
	"testing"
)

func TestLoadHostConfigRejectsInvalidKeychainBeforeOIDCDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		service string
		keyID   string
	}{
		{name: "missing service", keyID: "smoke-v1"},
		{name: "missing key ID", service: "goagents.host-api.approvals.smoke.test"},
		{name: "whitespace service", service: " ", keyID: "smoke-v1"},
		{name: "both whitespace", service: " ", keyID: "\t"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{
				agentApprovalKeychainServiceEnv: test.service,
				agentApprovalKeyIDEnv:           test.keyID,
			}
			oidcLoads := 0

			_, err := loadHostConfig(func(key string) string {
				return env[key]
			}, func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
				oidcLoads++
				return nil, errors.New("OIDC loader called")
			})

			if oidcLoads != 0 {
				t.Errorf("OIDC loader calls = %d, want 0", oidcLoads)
			}
			const wantError = "agent approval Keychain service and key ID must be configured together"
			if err == nil || err.Error() != wantError {
				t.Fatalf("load host config error = %v, want %q", err, wantError)
			}
		})
	}
}
