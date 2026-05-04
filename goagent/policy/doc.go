// Package policy provides permission checks for tool execution.
//
// Policy is the host-side safety gate between model-requested tool calls and
// tool execution. The model can request a tool call, but policy must allow it
// before ActStage runs the tool.
//
// The default Engine allows explicit read tools. It denies write, exec, empty,
// and unknown permissions unless the request-scoped allowed permissions include
// the requested permission. A policy denial aborts the run before tool
// execution.
//
// This package is not a full RBAC or approval system. It is the Agent-side
// enforcement point that host applications can replace with their own
// PolicyEngine.
package policy
