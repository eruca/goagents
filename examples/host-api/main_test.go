package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

func TestLoadHostConfigIncludesConfiguredSkillCatalog(t *testing.T) {
	root := t.TempDir()
	writeHostAPISkill(t, root, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\n---\n# Instructions\nReview scope and evidence.\n", nil)
	expectedAuthenticator := &OIDCApprovalAuthenticator{}
	env := map[string]string{hostAPISkillRootEnv: root}

	config, err := loadHostConfig(func(key string) string { return env[key] }, func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
		return expectedAuthenticator, nil
	})
	if err != nil {
		t.Fatalf("loadHostConfig returned error: %v", err)
	}
	if config.ApprovalAuthenticator != expectedAuthenticator || config.SkillCatalog == nil || config.SkillGateContext.OS == "" {
		t.Fatalf("config = %#v, want authenticator and Skill config", config)
	}
	if entries := config.SkillCatalog.List(); len(entries) != 1 || entries[0].Ref.Name != "workflow-review" {
		t.Fatalf("Skill entries = %#v, want workflow-review", entries)
	}
}

func TestLoadHostConfigRejectsInvalidSkillRootBeforeOIDCDiscovery(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	oidcLoads := 0
	_, err := loadHostConfig(func(key string) string {
		if key == hostAPISkillRootEnv {
			return missing
		}
		return ""
	}, func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
		oidcLoads++
		return nil, errors.New("OIDC loader called")
	})
	if oidcLoads != 0 {
		t.Fatalf("OIDC loader calls = %d, want 0", oidcLoads)
	}
	if err == nil || !strings.Contains(err.Error(), hostAPISkillRootEnv) || strings.Contains(err.Error(), missing) {
		t.Fatalf("loadHostConfig error = %v, want path-free Skill root error", err)
	}
}
