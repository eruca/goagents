module github.com/eruca/goagents/examples/host-runtime

go 1.26.1

require (
	github.com/eruca/artifactkit v0.0.0
	github.com/eruca/goagent v0.0.0
	github.com/eruca/llmkit v0.0.0
	github.com/eruca/runkit v0.0.0
	github.com/eruca/workflowkit v0.0.0
	github.com/eruca/workflowkit/agentstep v0.0.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.34.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/eruca/artifactkit => ../../artifactkit

replace github.com/eruca/goagent => ../../goagent

replace github.com/eruca/llmkit => ../../llmkit

replace github.com/eruca/runkit => ../../runkit

replace github.com/eruca/workflowkit => ../../workflowkit

replace github.com/eruca/workflowkit/agentstep => ../../workflowkit/agentstep
