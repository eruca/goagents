module github.com/eruca/goagents/examples/host-runtime

go 1.26.1

require (
	github.com/eruca/goagents/artifactkit v0.1.0
	github.com/eruca/goagents/goagent v0.1.0
	github.com/eruca/goagents/llmkit v0.1.0
	github.com/eruca/goagents/runkit v0.1.0
	github.com/eruca/goagents/workflowkit v0.1.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.34.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/eruca/goagents/artifactkit => ../../artifactkit

replace github.com/eruca/goagents/goagent => ../../goagent

replace github.com/eruca/goagents/llmkit => ../../llmkit

replace github.com/eruca/goagents/runkit => ../../runkit

replace github.com/eruca/goagents/workflowkit => ../../workflowkit
