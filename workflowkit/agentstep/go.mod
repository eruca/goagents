module github.com/eruca/workflowkit/agentstep

go 1.26.1

require (
	github.com/eruca/goagent v0.0.0
	github.com/eruca/workflowkit v0.0.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.30.0 // indirect
)

replace github.com/eruca/goagent => ../../goagent

replace github.com/eruca/workflowkit => ..
