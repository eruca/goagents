// Package agentcore contains the host-facing Agent API and the advanced ReAct
// runtime used to execute it.
//
// Most applications should use NewAgent, Agent.Run, Agent.RunDetailed,
// RunRequest, RunResult, options, events, budgets, skills, and module providers.
// The stage pipeline,
// RunState, ReActRunner, and individual stage types are exported for tests and
// specialized runtime composition, but hosts should prefer Agent unless they are
// intentionally replacing the default loop.
package agentcore
