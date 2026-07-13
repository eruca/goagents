package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	config, err := loadHostConfig(os.Getenv, loadOIDCApprovalAuthenticator)
	if err != nil {
		panic(err)
	}
	server, err := NewServer(config)
	if err != nil {
		panic(err)
	}
	server.StartQueuedWorker(context.Background())
	server.StartAgentApprovalJanitor(context.Background())
	addr := os.Getenv("HOST_API_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	fmt.Printf("host_api_addr=%s\n", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		panic(err)
	}
}

func loadHostConfig(
	getenv func(string) string,
	loadApprovalAuthenticator func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error),
) (Config, error) {
	keychainService := getenv(agentApprovalKeychainServiceEnv)
	keyID := getenv(agentApprovalKeyIDEnv)
	// Reject invalid identity before OIDC discovery; NewServer repeats this
	// validation so direct callers retain the same safety boundary.
	if _, err := resolveAgentApprovalKeychainConfig(keychainService, keyID); err != nil {
		return Config{}, err
	}
	catalog, skillGate, err := loadHostSkillConfig(getenv)
	if err != nil {
		return Config{}, err
	}

	startupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	approvalAuthenticator, err := loadApprovalAuthenticator(startupCtx, getenv)
	if err != nil {
		return Config{}, err
	}
	return Config{
		RuntimeHome:                  getenv("HOST_RUNTIME_HOME"),
		LLMKitHome:                   getenv("LLMKIT_HOME"),
		ApprovalAuthenticator:        approvalAuthenticator,
		AgentApprovalKeychainService: keychainService,
		AgentApprovalKeyID:           keyID,
		SkillCatalog:                 catalog,
		SkillGateContext:             skillGate,
	}, nil
}
