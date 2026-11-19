package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eruca/goagents/artifactkit"
	artifactpg "github.com/eruca/goagents/artifactkit/pgstore"
	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/llmkit/pgaudit"
	"github.com/eruca/goagents/llmkit/pghealth"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/approvalcrypto"
	"github.com/eruca/goagents/runkit/goagentapproval"
	runpg "github.com/eruca/goagents/runkit/pgstore"
	runsqlite "github.com/eruca/goagents/runkit/sqlitestore"
	"github.com/eruca/goagents/workflowkit"
	workflowpg "github.com/eruca/goagents/workflowkit/pgstore"
	workflowsqlite "github.com/eruca/goagents/workflowkit/sqlitestore"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Each switch moves one backend to PostgreSQL, shared by every process given
// the same DSN. With all of them set, several Host processes can serve one
// deployment; see the README's "Shared backends" section.
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
func openWorkflowStore(config Config, pools *pgPools) (hostWorkflowStore, error) {
	if dsn := strings.TrimSpace(config.WorkflowPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		db, err := pools.get(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("open workflow PostgreSQL store: %w", err)
		}
		store, err := workflowpg.OpenDB(ctx, db)
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
func openHealthStore(config Config, pools *pgPools) (llmkit.HealthStore, io.Closer, error) {
	if dsn := strings.TrimSpace(config.LLMKitHealthPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		db, err := pools.get(ctx, dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open llmkit health PostgreSQL store: %w", err)
		}
		store, err := pghealth.OpenDB(ctx, db, pghealth.Config{})
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

const (
	artifactPostgresDSNEnv    = "HOST_API_ARTIFACT_POSTGRES_DSN"
	agentRunPostgresDSNEnv    = "HOST_API_AGENT_RUN_POSTGRES_DSN"
	llmkitAuditPostgresDSNEnv = "HOST_API_LLMKIT_AUDIT_POSTGRES_DSN"
)

// hostRunStore is everything the Host needs from its agent run store: run
// records and events plus encrypted approval checkpoints.
type hostRunStore interface {
	runkit.Store
	runkit.CheckpointStore
	runkit.PendingCheckpointFailureStore
	io.Closer
}

var (
	_ hostRunStore = (*runsqlite.Store)(nil)
	_ hostRunStore = (*runpg.Store)(nil)
)

// openRunStore opens PostgreSQL when AgentRunPostgresDSN is set and the local
// SQLite file otherwise.
func openRunStore(config Config, pools *pgPools) (hostRunStore, error) {
	if dsn := strings.TrimSpace(config.AgentRunPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		db, err := pools.get(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("open agent run PostgreSQL store: %w", err)
		}
		store, err := runpg.OpenDB(ctx, db)
		if err != nil {
			return nil, fmt.Errorf("open agent run PostgreSQL store: %w", err)
		}
		return store, nil
	}
	return runsqlite.Open(config.AgentRunDBPath)
}

// openArtifactStore opens PostgreSQL when ArtifactPostgresDSN is set and the
// local file store otherwise. The closer is nil for the file store.
func openArtifactStore(config Config, pools *pgPools) (artifactkit.Store, io.Closer, error) {
	if dsn := strings.TrimSpace(config.ArtifactPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		db, err := pools.get(ctx, dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open artifact PostgreSQL store: %w", err)
		}
		store, err := artifactpg.OpenDB(ctx, db)
		if err != nil {
			return nil, nil, fmt.Errorf("open artifact PostgreSQL store: %w", err)
		}
		return store, store, nil
	}
	store, err := artifactkit.NewFileStore(config.ArtifactRoot)
	return store, nil, err
}

// auditBackend is where the Host records LLM routes and outcomes and reads
// them back for route views and routing statistics.
type auditBackend interface {
	Recorder() (llmkit.Recorder, error)
	ReadRouteAudits(context.Context, llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error)
	ModelStats(context.Context) (*llmkit.ModelStats, error)
}

// fileAudit keeps the audit in JSONL files under one llmkit home, readable
// only by processes on the same machine.
type fileAudit struct{ home string }

func (a fileAudit) Recorder() (llmkit.Recorder, error) { return llmkit.NewJSONLRecorder(a.home) }

func (a fileAudit) ReadRouteAudits(_ context.Context, filter llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error) {
	return llmkit.ReadRouteAudits(a.home, filter)
}

func (a fileAudit) ModelStats(context.Context) (*llmkit.ModelStats, error) {
	return llmkit.RefreshModelStats(a.home)
}

// pgAudit keeps the audit in PostgreSQL, shared by every process using it.
type pgAudit struct{ store *pgaudit.Store }

func (a pgAudit) Recorder() (llmkit.Recorder, error) { return a.store, nil }

func (a pgAudit) ReadRouteAudits(ctx context.Context, filter llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error) {
	return a.store.ReadRouteAudits(ctx, filter)
}

func (a pgAudit) ModelStats(ctx context.Context) (*llmkit.ModelStats, error) {
	return a.store.BuildModelStats(ctx)
}

// openAuditBackend opens PostgreSQL when LLMKitAuditPostgresDSN is set and
// the JSONL files under LLMKitHome otherwise. The closer is nil for files.
func openAuditBackend(config Config, pools *pgPools) (auditBackend, io.Closer, error) {
	if dsn := strings.TrimSpace(config.LLMKitAuditPostgresDSN); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), sharedBackendOpenTimeout)
		defer cancel()
		db, err := pools.get(ctx, dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open llmkit audit PostgreSQL store: %w", err)
		}
		store, err := pgaudit.OpenDB(ctx, db)
		if err != nil {
			return nil, nil, fmt.Errorf("open llmkit audit PostgreSQL store: %w", err)
		}
		return pgAudit{store: store}, store, nil
	}
	return fileAudit{home: config.LLMKitHome}, nil, nil
}

// pgPools opens one connection pool per distinct DSN, so every shared backend
// given the same DSN uses one pool instead of one pool each. Stores opened on
// a pool with OpenDB leave it open; Close closes the pools.
type pgPools struct {
	pools map[string]*sql.DB
	order []*sql.DB
}

func (p *pgPools) get(ctx context.Context, dsn string) (*sql.DB, error) {
	if db, ok := p.pools[dsn]; ok {
		return db, nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if p.pools == nil {
		p.pools = map[string]*sql.DB{}
	}
	p.pools[dsn] = db
	p.order = append(p.order, db)
	return db, nil
}

func (p *pgPools) Close() error {
	var err error
	for _, db := range p.order {
		err = errors.Join(err, db.Close())
	}
	p.pools, p.order = nil, nil
	return err
}
