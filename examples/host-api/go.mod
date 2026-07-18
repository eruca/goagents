module github.com/eruca/goagents/examples/host-api

go 1.26.1

require (
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/eruca/goagents/artifactkit v0.1.0
	github.com/eruca/goagents/evalkit v0.1.0
	github.com/eruca/goagents/goagent v0.1.0
	github.com/eruca/goagents/hostkit v0.1.0
	github.com/eruca/goagents/llmkit v0.1.0
	github.com/eruca/goagents/runkit v0.1.0
	github.com/eruca/goagents/skillkit v0.1.0
	github.com/eruca/goagents/workflowkit v0.1.0
)

require (
	github.com/99designs/go-keychain v0.0.0-20191008050251-8e49817e8af4 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.50.0 // indirect
)

replace github.com/eruca/goagents/artifactkit => ../../artifactkit

replace github.com/eruca/goagents/evalkit => ../../evalkit

replace github.com/eruca/goagents/goagent => ../../goagent

replace github.com/eruca/goagents/hostkit => ../../hostkit

replace github.com/eruca/goagents/llmkit => ../../llmkit

replace github.com/eruca/goagents/runkit => ../../runkit

replace github.com/eruca/goagents/skillkit => ../../skillkit

replace github.com/eruca/goagents/workflowkit => ../../workflowkit
