package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	startupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	approvalAuthenticator, err := loadOIDCApprovalAuthenticator(startupCtx, os.Getenv)
	if err != nil {
		panic(err)
	}
	config := Config{
		RuntimeHome:           os.Getenv("HOST_RUNTIME_HOME"),
		LLMKitHome:            os.Getenv("LLMKIT_HOME"),
		ApprovalAuthenticator: approvalAuthenticator,
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
