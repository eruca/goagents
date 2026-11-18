package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/llmkit/pghealth"
	"github.com/eruca/goagents/runkit/approvalcrypto"
	"github.com/eruca/goagents/runkit/goagentapproval"
	"github.com/eruca/goagents/workflowkit"
	workflowpg "github.com/eruca/goagents/workflowkit/pgstore"
	workflowsqlite "github.com/eruca/goagents/workflowkit/sqlitestore"
)

// These switches move one backend each to storage that several processes can
// share. They do not make the Host safe to run as several replicas: artifacts,
// llmkit audit files, agent runs, and in-process creation guards stay local.
const (
	workflowPostgresDSNEnv     = "HOST_API_WORKFLOW_POSTGRES_DSN"
	llmkitHealthPostgresDSNEnv = "HOST_API_LLMKIT_HEALTH_POSTGRES_DSN"
	agentApprovalKeyringEnv    = "HOST_API_AGENT_APPROVAL_KEYRING"

	sharedBackendOpenTimeout = 10 * time.Second
)

// hostWorkflowStore is everything the Host needs from its workflow store.
type hostWorkflowStore interface {
	workflowkit.CreateStore
	workflowkit.WorkflowQueryStore
	workflowkit.QueueRecoveryStore
	io.Closer
}

var (
	_ hostWorkflowStore = (*workflowsqlite.Store)(nil)
	_ hostWorkflowStore = (*workflowpg.Store)(nil)
)

// openWorkflowStore opens PostgreSQL when WorkflowPostgresDSN is set and the
// local SQLite file otherwise.
func openWorkflowStore(config Config) (hostWorkflowStore, error) {
	if dsn := strings.TrimSpace(config.WorkflowPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		store, err := workflowpg.Open(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("open workflow PostgreSQL store: %w", err)
		}
		return store, nil
	}
	return workflowsqlite.Open(config.WorkflowDBPath)
}

// openHealthStore opens the shared PostgreSQL health store when
// LLMKitHealthPostgresDSN is set and an in-memory store otherwise. The closer
// is nil for the in-memory store.
func openHealthStore(config Config) (llmkit.HealthStore, io.Closer, error) {
	if dsn := strings.TrimSpace(config.LLMKitHealthPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		store, err := pghealth.Open(ctx, dsn, pghealth.Config{})
		if err != nil {
			return nil, nil, fmt.Errorf("open llmkit health PostgreSQL store: %w", err)
		}
		return store, store, nil
	}
	return llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}), nil, nil
}

// resolveAgentApprovalKeys returns the approval cipher built from a static
// keyring when one is configured; otherwise it returns a nil cipher and the
// Keychain item the Host opens lazily. A keyring is shared by every process
// given it, unlike a Keychain item, which each machine creates for itself.
func resolveAgentApprovalKeys(config Config) (goagentapproval.Cipher, agentApprovalKeychainConfig, error) {
	keyring := strings.TrimSpace(config.AgentApprovalKeyring)
	if keyring == "" {
		keychain, err := resolveAgentApprovalKeychainConfig(config.AgentApprovalKeychainService, config.AgentApprovalKeyID)
		return config.AgentApprovalCipher, keychain, err
	}
	if config.AgentApprovalCipher != nil {
		return nil, agentApprovalKeychainConfig{}, errors.New("agent approval keyring and an injected cipher cannot both be configured")
	}
	if strings.TrimSpace(config.AgentApprovalKeychainService) != "" {
		return nil, agentApprovalKeychainConfig{}, fmt.Errorf("%s and %s cannot both be configured", agentApprovalKeyringEnv, agentApprovalKeychainServiceEnv)
	}
	activeID := strings.TrimSpace(config.AgentApprovalKeyID)
	if activeID == "" {
		return nil, agentApprovalKeychainConfig{}, fmt.Errorf("%s requires %s to name the active key", agentApprovalKeyringEnv, agentApprovalKeyIDEnv)
	}
	keys, err := approvalcrypto.ParseKeyring(keyring)
	if err != nil {
		return nil, agentApprovalKeychainConfig{}, fmt.Errorf("parse %s: %w", agentApprovalKeyringEnv, err)
	}
	provider, err := approvalcrypto.NewStaticKeyProvider(activeID, keys)
	if err != nil {
		return nil, agentApprovalKeychainConfig{}, fmt.Errorf("%s: %w", agentApprovalKeyringEnv, err)
	}
	cipher, err := approvalcrypto.NewAESGCMCipher(provider)
	if err != nil {
		return nil, agentApprovalKeychainConfig{}, err
	}
	return cipher, agentApprovalKeychainConfig{}, nil
}
