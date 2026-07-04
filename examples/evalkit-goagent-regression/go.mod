module github.com/eruca/goagents/examples/evalkit-goagent-regression

go 1.26.1

require (
	github.com/eruca/evalkit v0.0.0
	github.com/eruca/goagent v0.0.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace github.com/eruca/evalkit => ../../evalkit

replace github.com/eruca/goagent => ../../goagent
