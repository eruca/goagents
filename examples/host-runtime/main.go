package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	ctx := context.Background()
	home := os.Getenv("LLMKIT_HOME")
	if home == "" {
		temp, err := os.MkdirTemp("", "goagents-host-runtime-*")
		if err != nil {
			panic(err)
		}
		home = filepath.Join(temp, ".llmkit")
	}

	runtime, err := NewRuntime(Config{LLMKitHome: home})
	if err != nil {
		panic(err)
	}
	run, err := runtime.Start(ctx, Task{
		ID:    "wf-host-demo",
		Input: "Review the draft and prepare an approval summary.",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("workflow=%s status=%s approval=%s agent_run=%s output=%s\n",
		run.ID, run.Status, run.ApprovalRef, run.AgentRunID, run.OutputRef)

	run, err = runtime.Approve(ctx, run.ID, Approval{
		ApprovedBy: "operator-demo",
		Note:       "approved from host runtime example",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("workflow=%s status=%s output=%s audit=%s llmkit_home=%s\n",
		run.ID, run.Status, run.OutputRef, run.AuditRef, home)
}
