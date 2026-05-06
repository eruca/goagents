package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	home := os.Getenv("LLMKIT_HOME")
	if home == "" {
		temp, err := os.MkdirTemp("", "goagents-host-api-*")
		if err != nil {
			panic(err)
		}
		home = filepath.Join(temp, ".llmkit")
	}
	server, err := NewServer(Config{LLMKitHome: home})
	if err != nil {
		panic(err)
	}
	addr := os.Getenv("HOST_API_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	fmt.Printf("host_api_addr=%s llmkit_home=%s\n", addr, home)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		panic(err)
	}
}
