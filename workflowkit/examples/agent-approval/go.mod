module github.com/eruca/goagents/workflowkit/examples/agent-approval

go 1.26.1

require (
	github.com/eruca/goagents/goagent v0.1.0
	github.com/eruca/goagents/workflowkit v0.1.0
	github.com/eruca/goagents/workflowkit/agentstep v0.1.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.30.0 // indirect
)

replace github.com/eruca/goagents/goagent => ../../../goagent

replace github.com/eruca/goagents/workflowkit => ../..

replace github.com/eruca/goagents/workflowkit/agentstep => ../../agentstep
