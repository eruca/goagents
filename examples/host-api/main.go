package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	config := Config{
		RuntimeHome: os.Getenv("HOST_RUNTIME_HOME"),
		LLMKitHome:  os.Getenv("LLMKIT_HOME"),
	}
	server, err := NewServer(config)
	if err != nil {
		panic(err)
	}
	addr := os.Getenv("HOST_API_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	fmt.Printf("host_api_addr=%s\n", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		panic(err)
	}
}
