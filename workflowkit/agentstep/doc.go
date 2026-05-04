// Package agentstep adapts goagent RunDetailed calls into workflowkit steps.
//
// The adapter is optional host glue: it preserves the agent run ID and lets
// callers map agent results into workflow statuses such as succeeded, failed, or
// waiting_approval.
package agentstep

